package kanban

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/modules/kanban/db"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Port of Skills/JobAuthoring.swift, the part card promotion uses: validate
// the schedule, write <vault>/tenants/<ID>/skills/<slug>/SKILL.md, and upsert
// the skills_state row the Swift CronScheduler reads. JobsController
// (POST /v1/jobs) shares it in Swift and will reuse this when it is ported.

var (
	errInvalidCron = wire.NewError(http.StatusBadRequest, "invalid cron schedule")
	errOneSchedule = wire.NewError(http.StatusBadRequest, "job requires exactly one of cron or run_at")
)

type jobOrigin struct {
	kind string
	id   uuid.UUID
}

type jobAuthoring struct {
	paths vaultpaths.Paths
	log   *slog.Logger
}

// author writes the skill and enables it on q (the caller's transaction, as
// in Swift, so the upsert commits or rolls back with the card update).
func (a jobAuthoring) author(ctx context.Context, q *db.Queries, tenant uuid.UUID, title string,
	cron *string, runAt *time.Time, domain *string, spec string, spaceID *uuid.UUID, origin *jobOrigin,
) (string, error) {
	switch {
	case cron != nil && runAt == nil:
		if !validCron(*cron) {
			return "", errInvalidCron
		}
	case cron == nil && runAt != nil:
	default:
		return "", errOneSchedule
	}
	slug := jobSlug(title)

	dir := filepath.Join(a.paths.TenantRoot(tenant), "skills", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // Foundation createDirectory default; Swift reads it too
		return "", err
	}
	if err := writeAtomic(filepath.Join(dir, "SKILL.md"), []byte(skillMarkdown(slug, title, cron, domain, spec))); err != nil {
		return "", err
	}

	params := db.UpsertSkillStateParams{TenantID: tenant, Name: slug, Domain: domain, SpaceID: spaceID, RunAt: runAt}
	if origin != nil {
		params.OriginKind, params.OriginID = &origin.kind, &origin.id
	}
	if err := q.UpsertSkillState(ctx, params); err != nil {
		return "", err
	}

	when := "-"
	switch {
	case cron != nil:
		when = *cron
	case runAt != nil:
		when = "once@" + runAt.UTC().Format("2006-01-02 15:04:05 +0000")
	}
	d := "-"
	if domain != nil {
		d = *domain
	}
	a.log.InfoContext(ctx, fmt.Sprintf("job authored tenant=%s slug=%s when=%s domain=%s", ids.Upper(tenant), slug, when, d))
	return slug, nil
}

// writeAtomic is Data.write(to:options: .atomic): a temp file renamed over the target.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".SKILL.md.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // what Data.write leaves (umask 022)
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// jobSlug is JobAuthoring.slug: lowercase, letters and numbers kept, every
// other run collapsed to one "-", trimmed of "-", cut to 40, prefixed "job-".
// An empty core gets 8 random hex digits, as in Swift.
func jobSlug(title string) string {
	var out []rune
	lastDash := false
	for _, ch := range strings.ToLower(title) {
		if unicode.IsLetter(ch) || unicode.IsNumber(ch) {
			out = append(out, ch)
			lastDash = false
		} else if !lastDash {
			out = append(out, '-')
			lastDash = true
		}
	}
	core := []rune(strings.Trim(string(out), "-"))
	if len(core) == 0 {
		return "job-" + strings.ToLower(uuid.NewString()[:8])
	}
	return "job-" + string(core[:min(40, len(core))])
}

// skillMarkdown is JobAuthoring.skillMarkdown byte for byte.
func skillMarkdown(slug, title string, cron, domain *string, spec string) string {
	domainLine := ""
	if domain != nil {
		domainLine = "Domain: " + *domain + "."
	}
	scheduleLine := ""
	if cron != nil {
		scheduleLine = "\n  schedule: \"" + *cron + "\""
	}
	return "---\n" +
		"name: " + slug + "\n" +
		"description: " + title + "\n" +
		"license: MIT\n" +
		"allowed-tools: session_search vault_read weather_forecast mail_inbox_recent\n" +
		"metadata:\n" +
		"  capability: medium" + scheduleLine + "\n" +
		"  on_event: []\n" +
		"  daily_run_cap: { trial: 1, pro: 6, ultimate: 24 }\n" +
		"  maxInputTokens: 8000\n" +
		"  outputs:\n" +
		"    - kind: chat_message\n" +
		"---\n" +
		"You are a LuminaVault job: \"" + title + "\".\n" +
		domainLine + "\n" +
		"\n" +
		"Each run, do exactly this and present the result for the user:\n" +
		spec + "\n" +
		"\n" +
		"Output a SINGLE JSON object: {\"markdown\":\"<short summary>\",\"blocks\":[ ... ]}\n" +
		"using the Lumina block schema (statCard, lineChart, barChart, list, table,\n" +
		"keyValue, heading, paragraph, badge). Choose the blocks that best present\n" +
		"this domain's data — e.g. statCard + lineChart for metrics, table/list for\n" +
		"collections, paragraph for prose. Keep it concise and scannable. Output\n" +
		"only the JSON, no code fence."
}
