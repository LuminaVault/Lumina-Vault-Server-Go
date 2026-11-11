// Package jobauthor is Skills/JobAuthoring.swift's file half: the slug, the
// SKILL.md a job is authored as, and the atomic write. POST /v1/jobs and Kanban
// card promotion share it so both write identical, schedulable jobs; each keeps
// its own skills_state upsert on its own connection or transaction.
package jobauthor

import (
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// Write creates <tenant root>/skills/<slug>/ and writes SKILL.md atomically.
func Write(paths vaultpaths.Paths, tenant uuid.UUID, slug, markdown string) error {
	dir := filepath.Join(paths.TenantRoot(tenant), "skills", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // Foundation createDirectory default; Swift reads it too
		return err
	}
	return WriteAtomic(filepath.Join(dir, "SKILL.md"), []byte(markdown))
}

// WriteAtomic is Data.write(to:options: .atomic): a temp file renamed over the target.
func WriteAtomic(path string, data []byte) error {
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

// Slug is JobAuthoring.slug: lowercase, letters and numbers kept, every
// other run collapsed to one "-", trimmed of "-", cut to 40, prefixed "job-".
// An empty core gets 8 random hex digits, as in Swift.
func Slug(title string) string {
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

// Markdown is JobAuthoring.skillMarkdown byte for byte.
func Markdown(slug, title string, cron, domain *string, spec string) string {
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
