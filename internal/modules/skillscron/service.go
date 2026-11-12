// Package skillscron serves /v1/me/hermes/cron: the Go port of Swift's
// CronBridgeService and CronBridgeController (modules.yaml `skills-cron`). It
// bridges LuminaVault to a tenant's Hermes cron jobs (the real `hermes cron`
// ones, not LuminaVault's own CronScheduler) over one of two transports:
//
//   - Managed: `docker exec` into the tenant's container, reading
//     $HERMES_HOME/cron/jobs.json and running the `hermes cron` CLI.
//   - BYO: the Hermes dashboard cron API (<cron_dashboard_url>/api/cron/jobs,
//     Bearer dashboard token) for a remote Hermes with no container.
//
// The service also feeds the dashboard's cronJobs preview (DashboardCronPreview).
package skillscron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/hermescontainers"
	"lumina/internal/hermesdash"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/skills"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"lumina/internal/wire"
	"net/http"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hermesHome is HERMES_HOME inside the managed container.
const hermesHome = "/opt/data"

// Containers is the part of HermesContainerManager the bridge uses: the
// tenant's handle, nil with no error when there is none.
type Containers interface {
	Handle(ctx context.Context, tenant uuid.UUID) (*hermescontainers.Handle, error)
}

// Docker is the DockerExec call the bridge uses.
type Docker interface {
	Exec(ctx context.Context, container string, command []string, stdin []byte) (hermescontainers.Result, error)
}

// Service is CronBridgeService.
type Service struct {
	Pool       *pgxpool.Pool
	Box        *secretbox.Box
	Guard      ssrfguard.Guard
	Containers Containers
	Docker     Docker
	// HTTP is the dashboard client's transport (BYOHTTP.httpClient); nil means
	// hermesdash.NewClient().
	HTTP hermesdash.Doer
	// Transport and Model are the NL → spec classifier's (JobIntentClassifier on
	// the routed transport). A nil Transport classifies nothing as a job.
	Transport llm.ChatTransport
	Model     string
	Log       *slog.Logger
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// hasContainer is `(try? containerManager.handle(tenantID:)) ?? nil != nil`:
// a handle that fails to load counts as no container.
func (s *Service) hasContainer(ctx context.Context, tenant uuid.UUID) bool {
	h, err := s.Containers.Handle(ctx, tenant)
	return err == nil && h != nil
}

// List is list(tenantID:): the managed container, else a configured BYO
// dashboard, else 404 no_hermes_cron_source.
func (s *Service) List(ctx context.Context, tenant uuid.UUID) (ListResponse, error) {
	if s.hasContainer(ctx, tenant) {
		jobs, err := s.listManaged(ctx, tenant)
		if err != nil {
			return ListResponse{}, err
		}
		return ListResponse{Source: "managed", Jobs: jobs}, nil
	}
	byo, err := s.byoConfig(ctx, tenant)
	if err != nil {
		return ListResponse{}, err
	}
	if byo != nil {
		jobs, err := s.listBYO(ctx, byo)
		if err != nil {
			return ListResponse{}, err
		}
		return ListResponse{Source: "byo", Jobs: jobs}, nil
	}
	return ListResponse{}, wire.NewError(http.StatusNotFound, "no_hermes_cron_source")
}

// Create is create(tenantID:spec:): on whichever source the tenant has.
func (s *Service) Create(ctx context.Context, tenant uuid.UUID, spec CreateSpec) (ListResponse, error) {
	if s.hasContainer(ctx, tenant) {
		jobs, err := s.createManaged(ctx, tenant, spec)
		if err != nil {
			return ListResponse{}, err
		}
		return ListResponse{Source: "managed", Jobs: jobs}, nil
	}
	byo, err := s.byoConfig(ctx, tenant)
	if err != nil {
		return ListResponse{}, err
	}
	if byo != nil {
		jobs, err := s.createBYO(ctx, byo, spec)
		if err != nil {
			return ListResponse{}, err
		}
		return ListResponse{Source: "byo", Jobs: jobs}, nil
	}
	return ListResponse{}, wire.NewError(http.StatusNotFound, "no_hermes_cron_source")
}

// Preview is preview(tenantID:text:): natural language to a cron spec for the
// user to confirm, without writing anything. The deterministic parser comes
// first (the managed transport's agent ignores a JSON-only prompt); the LLM is a
// fallback. false means not a schedulable job.
func (s *Service) Preview(ctx context.Context, tenant uuid.UUID, text string) (CreateSpec, bool) {
	deliver := deliverTarget(text)
	if parsed, ok := ParseSchedule(text); ok {
		return CreateSpec{Schedule: parsed.Cron, Prompt: ptr(text), Name: ptr(DeriveName(text)), Deliver: &deliver}, true
	}
	// Fallback: ask the LLM (works for BYO/plain models that obey JSON).
	proposal := skills.Classify(ctx, s.Transport, s.Model, text, tenant)
	cron := "nil"
	if proposal.Cron != nil {
		cron = *proposal.Cron
	}
	s.log().InfoContext(ctx, fmt.Sprintf("cron preview LLM-fallback isJob=%t cron=%s", proposal.IsJob, cron))
	if !proposal.IsJob || proposal.Cron == nil || *proposal.Cron == "" {
		return CreateSpec{}, false
	}
	prompt := text
	if proposal.Spec != nil {
		prompt = *proposal.Spec
	}
	var name string
	if proposal.Title != nil {
		name = *proposal.Title
	} else {
		name = DeriveName(text)
	}
	return CreateSpec{Schedule: *proposal.Cron, Prompt: &prompt, Name: &name, Deliver: &deliver}, true
}

// SetBYOConfig is setBYOConfig: validate the dashboard URL, seal the token, and
// store both on the tenant's user_hermes_config row, creating one with an empty
// api_server base_url (no override for the resolver) when there is none.
func (s *Service) SetBYOConfig(ctx context.Context, tenant uuid.UUID, rawURL, token string) error {
	validated, err := s.Guard.Validate(ctx, rawURL)
	if err != nil {
		return err
	}
	sealed, err := s.Box.Seal(token, tenant)
	if err != nil {
		return err
	}
	var id uuid.UUID
	err = s.Pool.QueryRow(ctx, `SELECT id FROM user_hermes_config WHERE tenant_id = $1 LIMIT 1`, tenant).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = s.Pool.Exec(ctx, `INSERT INTO user_hermes_config
			(id, tenant_id, base_url, cron_dashboard_url, cron_dashboard_token_ciphertext, cron_dashboard_token_nonce, created_at, updated_at)
			VALUES ($1, $2, '', $3, $4, $5, NOW(), NOW())`, uuid.New(), tenant, validated.String(), sealed.Ciphertext, sealed.Nonce)
	case err == nil:
		_, err = s.Pool.Exec(ctx, `UPDATE user_hermes_config SET cron_dashboard_url = $2, cron_dashboard_token_ciphertext = $3,
			cron_dashboard_token_nonce = $4, updated_at = NOW() WHERE id = $1`, id, validated.String(), sealed.Ciphertext, sealed.Nonce)
	}
	return err
}

func (s *Service) byoConfig(ctx context.Context, tenant uuid.UUID) (*hermesdash.Credentials, error) {
	return hermesdash.DashboardCredentials(ctx, s.Pool, s.Box, tenant)
}

// dashboard is the one dashboard client for every BYO call (the mirror shares
// it); it validates the user's URL against SSRF on every call.
func (s *Service) dashboard(byo *hermesdash.Credentials) *hermesdash.Dashboard {
	return hermesdash.NewDashboard(byo.URL, byo.Token, s.Guard, s.HTTP)
}

func (s *Service) listBYO(ctx context.Context, byo *hermesdash.Credentials) ([]Job, error) {
	raw, err := s.dashboard(byo).ListCronJobsRaw(ctx)
	if err != nil {
		return nil, ByoError(err, "http")
	}
	return Parse(raw), nil
}

func (s *Service) createBYO(ctx context.Context, byo *hermesdash.Credentials, spec CreateSpec) ([]Job, error) {
	deliver := "origin"
	if spec.Deliver != nil {
		deliver = *spec.Deliver
	}
	body := map[string]any{"schedule": spec.Schedule, "deliver": deliver}
	if spec.Prompt != nil && *spec.Prompt != "" {
		body["prompt"] = *spec.Prompt
	}
	if spec.Name != nil && *spec.Name != "" {
		body["name"] = *spec.Name
	}
	if _, err := s.dashboard(byo).CreateCronJobRaw(ctx, body); err != nil {
		return nil, ByoError(err, "create")
	}
	return s.listBYO(ctx, byo)
}

// ByoError is byoError: keeps the pre-mirror codes clients key off
// (byo_cron_http_<status>, byo_cron_create_<status>), all 502. Only a
// HermesMirrorTransportError is mapped; anything else (an SSRF rejection) passes
// through, as Swift's `catch let error as HermesMirrorTransportError` does.
func ByoError(err error, operation string) error {
	var te *hermesdash.Error
	if !errors.As(err, &te) {
		return err
	}
	switch te.Kind {
	case hermesdash.ErrHTTP:
		return wire.NewError(http.StatusBadGateway, fmt.Sprintf("byo_cron_%s_%d", operation, te.Status))
	case hermesdash.ErrDashboardUnauthorized:
		return wire.NewError(http.StatusBadGateway, "byo_cron_"+operation+"_401")
	default:
		return wire.NewError(http.StatusBadGateway, te.Code())
	}
}

func (s *Service) requireHandle(ctx context.Context, tenant uuid.UUID) (*hermescontainers.Handle, error) {
	h, err := s.Containers.Handle(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, wire.NewError(http.StatusNotFound, "no_hermes_container")
	}
	return h, nil
}

func (s *Service) listManaged(ctx context.Context, tenant uuid.UUID) ([]Job, error) {
	h, err := s.requireHandle(ctx, tenant)
	if err != nil {
		return nil, err
	}
	res, err := s.Docker.Exec(ctx, h.ContainerName, []string{"cat", hermesHome + "/cron/jobs.json"}, nil)
	if err != nil {
		return nil, err
	}
	// Missing file (no jobs yet) → empty list, not an error.
	if !res.OK() {
		return []Job{}, nil
	}
	return Parse([]byte(res.Stdout)), nil
}

// createManaged runs the CLI inside the container. The command is an argv
// array (no shell), so values are not shell-interpolated.
func (s *Service) createManaged(ctx context.Context, tenant uuid.UUID, spec CreateSpec) ([]Job, error) {
	h, err := s.requireHandle(ctx, tenant)
	if err != nil {
		return nil, err
	}
	command := []string{"hermes", "cron", "create", spec.Schedule}
	if spec.Prompt != nil && *spec.Prompt != "" {
		command = append(command, *spec.Prompt)
	}
	if spec.Name != nil && *spec.Name != "" {
		command = append(command, "--name", *spec.Name)
	}
	if spec.Deliver != nil && *spec.Deliver != "" {
		command = append(command, "--deliver", *spec.Deliver)
	}
	if spec.Skills != nil {
		for _, skill := range *spec.Skills {
			if skill != "" {
				command = append(command, "--skill", skill)
			}
		}
	}
	res, err := s.Docker.Exec(ctx, h.ContainerName, command, nil)
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		s.log().ErrorContext(ctx, fmt.Sprintf("hermes cron create failed tenant=%s exit=%d: %s", ids.Upper(tenant), res.ExitCode, redact(res.Stderr)))
		return nil, wire.NewError(http.StatusBadGateway, "cron_create_failed")
	}
	return s.listManaged(ctx, tenant)
}

// MutateManaged is mutateManaged: `hermes cron <action> <id>` in the container.
func (s *Service) MutateManaged(ctx context.Context, tenant uuid.UUID, action, id string) ([]Job, error) {
	h, err := s.requireHandle(ctx, tenant)
	if err != nil {
		return nil, err
	}
	res, err := s.Docker.Exec(ctx, h.ContainerName, []string{"hermes", "cron", action, id}, nil)
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, wire.NewError(http.StatusBadGateway, "cron_"+action+"_failed")
	}
	return s.listManaged(ctx, tenant)
}

var bearerRe = regexp.MustCompile(`Bearer\s+[A-Za-z0-9._\-]+`)

// redact is Logger.redact: bearer tokens become "Bearer ***".
func redact(s string) string { return bearerRe.ReplaceAllString(s, "Bearer ***") }
