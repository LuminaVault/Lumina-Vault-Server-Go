// Package system is /v1/system/hermes (wave 7C): HermesUpdateController and
// HermesUpdateService (Sources/App/System). Every step of an update drives docker.
// This process has none (owner decision O1), so the routes, the job rows and the
// SSE stream are ported, and each docker call fails as Swift's does in a pod with
// no docker binary: preflight fails and the job ends failed.
package system

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/httpserver"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Docker is the part of DockerExec the update engine uses.
type Docker interface {
	Run(ctx context.Context, args []string) (stdout string, exitCode int, err error)
}

// NoDocker is the default Docker: the binary does not exist, so nothing runs.
type NoDocker struct{}

// ErrNoDocker is what launching a missing docker binary reports.
var ErrNoDocker = errors.New("docker binary not found")

func (NoDocker) Run(context.Context, []string) (string, int, error) { return "", -1, ErrNoDocker }

// Options are the hermes.central.* settings and the admin token.
type Options struct {
	Identify       echo.MiddlewareFunc
	AdminToken     string
	ContainerName  string
	RegistryImage  string
	ChannelTag     string
	PerTenant      bool // a container manager exists (always, beside the secret box)
	Docker         Docker
	Now            func() time.Time
	Log            *slog.Logger
	BackgroundWait *sync.WaitGroup // tests wait for the job engine
}

// Module serves the update routes.
type Module struct {
	pool *pgxpool.Pool
	opt  Options
	// mu serialises start and rollback, as the Swift actor does.
	mu   sync.Mutex
	subs *subscribers
}

func New(pool *pgxpool.Pool, opt Options) *Module {
	if opt.Docker == nil {
		opt.Docker = NoDocker{}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.ContainerName == "" {
		opt.ContainerName = "luminavault-hermes"
	}
	if opt.RegistryImage == "" {
		opt.RegistryImage = "ghcr.io/luminavault/luminavault-hermes"
	}
	if opt.ChannelTag == "" {
		opt.ChannelTag = "latest"
	}
	return &Module{pool: pool, opt: opt, subs: &subscribers{m: map[uuid.UUID]map[int]chan event{}}}
}

// Register mounts JWT then the admin token, as App+build.swift does.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	chain := []echo.MiddlewareFunc{m.opt.Identify, m.requireAdminToken}
	r.GET("/v1/system/hermes/version", m.version, chain...)
	r.POST("/v1/system/hermes/update", m.startUpdate, chain...)
	r.GET("/v1/system/hermes/update/current", m.currentJob, chain...)
	r.GET("/v1/system/hermes/update/:jobID", m.jobStatus, chain...)
	r.GET("/v1/system/hermes/update/:jobID/stream", m.stream, chain...)
	r.Streaming(http.MethodGet, "/v1/system/hermes/update/:jobID/stream")
	r.POST("/v1/system/hermes/update/:jobID/rollback", m.rollback, chain...)
	r.POST("/v1/system/hermes/reprovision", m.reprovision, chain...)
}

// requireAdminToken is AdminTokenMiddleware.
func (m *Module) requireAdminToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if m.opt.AdminToken == "" {
			return wire.NewError(http.StatusNotFound, "admin disabled")
		}
		presented := c.Request().Header.Values("X-Admin-Token")
		if len(presented) == 0 || !constantTimeEquals(presented[0], m.opt.AdminToken) {
			return wire.NewError(http.StatusUnauthorized, "admin token invalid")
		}
		return next(c)
	}
}

func constantTimeEquals(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range len(a) {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// MARK: - DTOs

// Step is HermesUpdateStep.
type Step struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	Detail     *string    `json:"detail,omitempty"`
	StartedAt  *wire.Time `json:"startedAt,omitempty"`
	FinishedAt *wire.Time `json:"finishedAt,omitempty"`
}

// JobStatus is HermesUpdateJobStatus.
type JobStatus struct {
	JobID        wire.UUID `json:"jobID"`
	State        string    `json:"state"`
	Steps        []Step    `json:"steps"`
	FromVersion  *string   `json:"fromVersion,omitempty"`
	ToVersion    *string   `json:"toVersion,omitempty"`
	ErrorMessage *string   `json:"errorMessage,omitempty"`
	StartedAt    wire.Time `json:"startedAt"`
	UpdatedAt    wire.Time `json:"updatedAt"`
}

var pipeline = []string{
	"preflight", "pullImage", "verifyImage", "snapshotCurrent",
	"swapCentral", "healthCheckCentral", "reprovisionTenants", "verifyTenants", "promote",
}

var stepIDs = map[string]bool{
	"preflight": true, "pullImage": true, "verifyImage": true, "snapshotCurrent": true, "swapCentral": true,
	"healthCheckCentral": true, "reprovisionTenants": true, "verifyTenants": true, "promote": true, "rollback": true,
}

var stepStates = map[string]bool{"pending": true, "running": true, "succeeded": true, "failed": true, "skipped": true}

var jobStates = map[string]bool{"running": true, "succeeded": true, "failed": true, "rolledBack": true}

// MARK: - Steps codec (HermesUpdateJob.encodeSteps / steps)

const iso8601 = "2006-01-02T15:04:05Z07:00"

type storedStep struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	Detail     *string `json:"detail,omitempty"`
	StartedAt  *string `json:"startedAt,omitempty"`
	FinishedAt *string `json:"finishedAt,omitempty"`
}

func isoString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(iso8601)
	return &s
}

func encodeSteps(steps []Step) string {
	out := make([]storedStep, len(steps))
	for i, s := range steps {
		out[i] = storedStep{ID: s.ID, State: s.State, Detail: s.Detail}
		if s.StartedAt != nil {
			out[i].StartedAt = isoString(&s.StartedAt.Time)
		}
		if s.FinishedAt != nil {
			out[i].FinishedAt = isoString(&s.FinishedAt.Time)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return strings.ReplaceAll(string(b), "/", `\/`)
}

// decodeSteps is the steps getter: an undecodable column is no steps.
func decodeSteps(raw string) []Step {
	var stored []storedStep
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return []Step{}
	}
	out := make([]Step, len(stored))
	for i, s := range stored {
		if !stepIDs[s.ID] || !stepStates[s.State] {
			return []Step{}
		}
		out[i] = Step{ID: s.ID, State: s.State, Detail: s.Detail}
		for _, p := range []struct {
			src *string
			dst **wire.Time
		}{{s.StartedAt, &out[i].StartedAt}, {s.FinishedAt, &out[i].FinishedAt}} {
			if p.src == nil {
				continue
			}
			t, err := time.Parse(iso8601, *p.src)
			if err != nil {
				return []Step{}
			}
			*p.dst = &wire.Time{Time: t}
		}
	}
	return out
}

type jobRow struct {
	id                   uuid.UUID
	state, stepsJSON     string
	from, to, errMessage *string
	createdAt, updatedAt *time.Time
}

const jobColumns = `id, state, steps_json, from_version, to_version, error_message, created_at, updated_at`

func scanJob(row pgx.Row) (*jobRow, error) {
	var j jobRow
	err := row.Scan(&j.id, &j.state, &j.stepsJSON, &j.from, &j.to, &j.errMessage, &j.createdAt, &j.updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // Swift's Optional
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (j *jobRow) snapshot(now time.Time) JobStatus {
	state := j.state
	if !jobStates[state] {
		state = "failed"
	}
	started, updated := now, now
	if j.createdAt != nil {
		started = *j.createdAt
	}
	if j.updatedAt != nil {
		updated = *j.updatedAt
	}
	return JobStatus{
		JobID: wire.UUID{UUID: j.id}, State: state, Steps: decodeSteps(j.stepsJSON),
		FromVersion: j.from, ToVersion: j.to, ErrorMessage: j.errMessage,
		StartedAt: wire.Time{Time: started}, UpdatedAt: wire.Time{Time: updated},
	}
}

func (m *Module) loadJob(ctx context.Context, id uuid.UUID) (*jobRow, error) {
	return scanJob(m.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM hermes_update_jobs WHERE id = $1`, id))
}

func (m *Module) hasRunningJob(ctx context.Context) (bool, error) {
	var running bool
	err := m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hermes_update_jobs WHERE state = 'running')`).Scan(&running)
	return running, err
}

// MARK: - Central manager (CentralHermesManager): docker inspection

func (m *Module) fullRef(tag string) string { return m.opt.RegistryImage + ":" + tag }

func (m *Module) inspect(ctx context.Context, format string) *string {
	out, code, err := m.opt.Docker.Run(ctx, []string{"inspect", "--format", format, m.opt.ContainerName})
	if err != nil || code != 0 {
		return nil
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return nil
	}
	return &v
}

// MARK: - Handlers

type versionInfo struct {
	CurrentRef      string     `json:"currentRef"`
	CurrentDigest   *string    `json:"currentDigest,omitempty"`
	CurrentLabel    string     `json:"currentLabel"`
	AvailableLabel  *string    `json:"availableLabel,omitempty"`
	UpdateAvailable bool       `json:"updateAvailable"`
	LastUpdatedAt   *wire.Time `json:"lastUpdatedAt,omitempty"`
}

// splitLast is Swift's split(separator:).last: empty pieces are dropped.
func splitLast(s string, sep rune) (string, bool) {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == sep })
	if len(parts) == 0 {
		return "", false
	}
	return parts[len(parts)-1], true
}

// shortLabel is HermesUpdateService.shortLabel(ref:digest:).
func shortLabel(ref string, digest *string) string {
	if tag, ok := splitLast(ref, ':'); ok && strings.Contains(ref, ":") {
		return tag
	}
	if digest != nil {
		if short, ok := splitLast(*digest, ':'); ok {
			if len(short) > 12 {
				short = short[:12]
			}
			return "sha256:" + short
		}
	}
	return ref
}

func (m *Module) version(c *echo.Context) error {
	ctx := c.Request().Context()
	ref := m.fullRef(m.opt.ChannelTag)
	if r := m.inspect(ctx, "{{.Config.Image}}"); r != nil {
		ref = *r
	}
	digest := m.inspect(ctx, "{{.Image}}")
	info := versionInfo{CurrentRef: ref, CurrentDigest: digest, CurrentLabel: shortLabel(ref, digest)}
	var last *time.Time
	err := m.pool.QueryRow(ctx, `SELECT updated_at FROM hermes_update_jobs WHERE state = 'succeeded'
		ORDER BY updated_at DESC NULLS LAST LIMIT 1`).Scan(&last)
	if err == nil && last != nil {
		info.LastUpdatedAt = &wire.Time{Time: *last}
	}
	return wire.JSON(c, http.StatusOK, map[string]any{"info": info})
}

type startRequest struct {
	TargetTag *string `json:"targetTag,omitempty"`
}

type startResponse struct {
	JobID wire.UUID `json:"jobID"`
	State string    `json:"state"`
}

var errInProgress = wire.NewError(http.StatusConflict, "update_in_progress")

func (m *Module) startUpdate(c *echo.Context) error {
	body, err := wire.Bind[startRequest](c)
	if err != nil {
		body = &startRequest{} // (try? req.decode) ?? StartHermesUpdateRequest(targetTag: nil)
	}
	ctx := c.Request().Context()
	m.mu.Lock()
	defer m.mu.Unlock()
	if running, err := m.hasRunningJob(ctx); err != nil {
		return err
	} else if running {
		return errInProgress
	}
	tag := m.opt.ChannelTag
	if body.TargetTag != nil {
		if t := strings.TrimSpace(*body.TargetTag); t != "" {
			tag = t
		}
	}
	targetRef := m.fullRef(tag)
	currentRef := m.inspect(ctx, "{{.Config.Image}}")
	steps := make([]Step, len(pipeline))
	for i, id := range pipeline {
		steps[i] = Step{ID: id, State: "pending"}
	}
	id := uuid.New()
	now := m.opt.Now()
	if _, err := m.pool.Exec(ctx, `INSERT INTO hermes_update_jobs (`+jobColumns+`)
		VALUES ($1, 'running', $2, $3, $4, NULL, $5, $5)`, id, encodeSteps(steps), currentRef, targetRef, now); err != nil {
		return err
	}
	m.background(func(ctx context.Context) { m.execute(ctx, id) })
	return wire.JSON(c, http.StatusOK, startResponse{JobID: wire.UUID{UUID: id}, State: "running"})
}

func (m *Module) background(f func(context.Context)) {
	if wg := m.opt.BackgroundWait; wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg := m.opt.BackgroundWait; wg != nil {
			defer wg.Done()
		}
		f(context.Background())
	}()
}

func (m *Module) currentJob(c *echo.Context) error {
	ctx := c.Request().Context()
	j, err := scanJob(m.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM hermes_update_jobs WHERE state = 'running'
		ORDER BY created_at DESC NULLS LAST LIMIT 1`))
	if err != nil {
		return err
	}
	if j == nil {
		if j, err = scanJob(m.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM hermes_update_jobs
			ORDER BY created_at DESC NULLS LAST LIMIT 1`)); err != nil {
			return err
		}
	}
	if j == nil {
		return wire.NewError(http.StatusNotFound, "no_update_job")
	}
	return wire.JSON(c, http.StatusOK, map[string]any{"status": j.snapshot(m.opt.Now())})
}

func jobID(c *echo.Context) (uuid.UUID, error) {
	raw := c.Param("jobID")
	id, err := uuid.Parse(raw)
	if err != nil || len(raw) != 36 {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid_job_id")
	}
	return id, nil
}

var errJobNotFound = wire.NewError(http.StatusNotFound, "job_not_found")

func (m *Module) jobStatus(c *echo.Context) error {
	id, err := jobID(c)
	if err != nil {
		return err
	}
	j, err := m.loadJob(c.Request().Context(), id)
	if err != nil {
		return err
	}
	if j == nil {
		return errJobNotFound
	}
	return wire.JSON(c, http.StatusOK, map[string]any{"status": j.snapshot(m.opt.Now())})
}

func (m *Module) rollback(c *echo.Context) error {
	id, err := jobID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	m.mu.Lock()
	defer m.mu.Unlock()
	if running, err := m.hasRunningJob(ctx); err != nil {
		return err
	} else if running {
		return errInProgress
	}
	failed, err := m.loadJob(ctx, id)
	if err != nil {
		return err
	}
	if failed == nil || failed.from == nil {
		return wire.NewError(http.StatusBadRequest, "rollback_unavailable:no recorded previous version")
	}
	restoreRef := *failed.from
	newID := uuid.New()
	now := m.opt.Now()
	if _, err := m.pool.Exec(ctx, `INSERT INTO hermes_update_jobs (`+jobColumns+`)
		VALUES ($1, 'running', $2, $3, $4, NULL, $5, $5)`,
		newID, encodeSteps([]Step{{ID: "rollback", State: "pending"}}), failed.to, restoreRef, now); err != nil {
		return err
	}
	m.background(func(ctx context.Context) { m.executeManualRollback(ctx, newID, restoreRef) })
	return wire.JSON(c, http.StatusOK, startResponse{JobID: wire.UUID{UUID: newID}, State: "running"})
}

// reprovision is reprovisionTenants: the container manager's reprovisionAll
// starts with `docker network inspect`, which throws here.
func (m *Module) reprovision(c *echo.Context) error {
	if !m.opt.PerTenant {
		return wire.NewError(http.StatusServiceUnavailable, "per_tenant_disabled")
	}
	if _, _, err := m.opt.Docker.Run(c.Request().Context(), []string{"network", "inspect", "lv-hermes"}); err != nil {
		return err
	}
	return errors.New("reprovision needs docker")
}

// MARK: - Step engine

func (m *Module) mark(ctx context.Context, id uuid.UUID, stepID, state string, detail *string) error {
	j, err := m.loadJob(ctx, id)
	if err != nil || j == nil {
		return err
	}
	steps := decodeSteps(j.stepsJSON)
	ts := wire.Time{Time: m.opt.Now()}
	for i, s := range steps {
		if s.ID != stepID {
			continue
		}
		next := Step{ID: stepID, State: state, Detail: s.Detail, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt}
		if detail != nil {
			next.Detail = detail
		}
		if state == "running" {
			next.StartedAt = &ts
		}
		if state == "succeeded" || state == "failed" || state == "skipped" {
			next.FinishedAt = &ts
		}
		steps[i] = next
	}
	if _, err := m.pool.Exec(ctx, `UPDATE hermes_update_jobs SET steps_json = $2, updated_at = $3 WHERE id = $1`,
		id, encodeSteps(steps), m.opt.Now()); err != nil {
		return err
	}
	for _, s := range steps {
		if s.ID == stepID {
			m.subs.broadcast(id, event{Type: "step", Payload: s})
		}
	}
	return nil
}

func (m *Module) finalize(ctx context.Context, id uuid.UUID, state string, errMessage *string) {
	if _, err := m.pool.Exec(ctx, `UPDATE hermes_update_jobs SET state = $2, error_message = $3, updated_at = $4 WHERE id = $1`,
		id, state, errMessage, m.opt.Now()); err != nil {
		return
	}
	m.subs.broadcast(id, event{Type: "status", Payload: state})
	j, err := m.loadJob(ctx, id)
	if err != nil || j == nil {
		return
	}
	m.subs.finish(id, event{Type: "done", Payload: j.snapshot(m.opt.Now())})
}

func ptr(s string) *string { return &s }

// userMessage is HermesUpdateService.userMessage(for:): with no docker binary
// every failure is the default case.
func userMessage(err error) string { return fmt.Sprintf("The update failed: %v", err) }

// execute is the step engine up to its first docker call, preflight's
// `docker version`, which fails here: the active step fails and the job ends
// failed, as Swift's does when Process cannot launch docker.
func (m *Module) execute(ctx context.Context, id uuid.UUID) {
	fail := func(err error) {
		m.opt.Log.ErrorContext(ctx, "hermes update failed", "jobID", id, "error", err)
		m.markActiveStepFailed(ctx, id, err)
		m.finalize(ctx, id, "failed", ptr(userMessage(err)))
	}
	if err := m.mark(ctx, id, "preflight", "running", nil); err != nil {
		fail(err)
		return
	}
	if _, code, err := m.opt.Docker.Run(ctx, []string{"version", "--format", "{{.Server.Version}}"}); err != nil {
		fail(err)
		return
	} else if code != 0 {
		fail(errors.New("dockerUnreachable"))
		return
	}
	fail(errors.New("hermes update needs docker"))
}

func (m *Module) markActiveStepFailed(ctx context.Context, id uuid.UUID, cause error) {
	j, err := m.loadJob(ctx, id)
	if err != nil || j == nil {
		return
	}
	steps := decodeSteps(j.stepsJSON)
	for _, want := range []string{"running", "pending"} {
		for _, s := range steps {
			if s.State == want {
				_ = m.mark(ctx, id, s.ID, "failed", ptr(userMessage(cause)))
				return
			}
		}
	}
}

// executeManualRollback: restoreCanonical's first docker call fails.
func (m *Module) executeManualRollback(ctx context.Context, id uuid.UUID, restoreRef string) {
	if err := m.mark(ctx, id, "rollback", "running", ptr("restoring "+restoreRef)); err == nil {
		_, _, err = m.opt.Docker.Run(ctx, []string{"rm", "-f", m.opt.ContainerName})
		if err == nil {
			err = errors.New("restore needs docker")
		}
		_ = m.mark(ctx, id, "rollback", "failed", ptr(err.Error()))
	}
	m.finalize(ctx, id, "failed", ptr("Restore did not succeed. Manual recovery may be required."))
}

// MARK: - SSE (HermesUpdateSSEResponse)

type event struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

type subscribers struct {
	mu   sync.Mutex
	next int
	m    map[uuid.UUID]map[int]chan event
}

func (s *subscribers) add(id uuid.UUID) (int, chan event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ch := make(chan event, 64)
	if s.m[id] == nil {
		s.m[id] = map[int]chan event{}
	}
	s.m[id][s.next] = ch
	return s.next, ch
}

func (s *subscribers) remove(id uuid.UUID, sub int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m[id], sub)
	if len(s.m[id]) == 0 {
		delete(s.m, id)
	}
}

func (s *subscribers) broadcast(id uuid.UUID, e event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.m[id] {
		select {
		case ch <- e:
		default:
		}
	}
}

func (s *subscribers) finish(id uuid.UUID, e event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.m[id] {
		select {
		case ch <- e:
		default:
		}
		close(ch)
	}
	delete(s.m, id)
}

// sseEvent encodes an event as JSONEncoder with .iso8601 does: dates to the
// second, "/" escaped.
func sseEvent(e event) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	b = []byte(strings.ReplaceAll(string(b), "/", `\/`))
	return append(append([]byte("data: "), b...), '\n', '\n')
}

func (m *Module) stream(c *echo.Context) error {
	id, err := jobID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	j, err := m.loadJob(ctx, id)
	if err != nil {
		return err
	}
	if j == nil {
		return errJobNotFound
	}
	// Subscribe before reading the snapshot, so no step between the two is lost.
	sub, ch := m.subs.add(id)
	defer m.subs.remove(id, sub)
	w := c.Response()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	j, err = m.loadJob(ctx, id)
	if err != nil || j == nil {
		_, _ = w.Write(sseEvent(event{Type: "error", Payload: "notFound"}))
		return nil
	}
	snap := j.snapshot(m.opt.Now())
	for _, s := range snap.Steps {
		_, _ = w.Write(sseEvent(event{Type: "step", Payload: s}))
	}
	if snap.State != "running" {
		_, _ = w.Write(sseEvent(event{Type: "done", Payload: snap}))
		flush()
		return nil
	}
	flush()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			_, _ = w.Write(sseEvent(e))
			flush()
		}
	}
}
