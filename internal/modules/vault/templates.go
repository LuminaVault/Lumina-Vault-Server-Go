package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/modules/vault/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// Backlog E6 (spec: docs/superpowers/specs/2026-11-16-e6-templates-daily.md).
// Templates and daily notes live in two system Spaces, so their folders are
// the Space slugs and every existing route (upload, list, move, trash) works
// on them unchanged.

type systemSpace struct{ slug, name, icon string }

var (
	templatesSpace = systemSpace{"templates", "Templates", "doc.on.doc"}
	dailySpace     = systemSpace{"daily", "Daily", "calendar"}
)

// starterTemplates are written by POST /v1/vault/templates/seed. dailyStarter
// doubles as the daily note's body when templates/daily.md doesn't exist.
const dailyStarter = "# {{date}}\n\n## Today\n- \n\n## Notes\n\n"

var starterTemplates = []struct{ name, body string }{
	{"daily.md", dailyStarter},
	{"meeting.md", "# {{title}}\n\n{{date}} {{time}} · {{space}}\n\n## Attendees\n- \n\n## Notes\n\n## Action items\n- [ ] \n"},
	{"project.md", "# {{title}}\n\nStarted {{date}}\n\n## Goal\n\n## Next steps\n- [ ] \n"},
}

type templateVars struct{ date, time, title, space string }

// renderTemplate fills {{date}}, {{time}}, {{title}} and {{space}}. Anything
// else in braces, Obsidian's {{date:FORMAT}} included, is left as written.
func renderTemplate(src string, v templateVars) string {
	return strings.NewReplacer(
		"{{date}}", v.date, "{{time}}", v.time,
		"{{title}}", v.title, "{{space}}", v.space,
	).Replace(src)
}

// location parses the caller's IANA time zone; empty means UTC.
func location(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "Local" {
		return nil, wire.NewError(http.StatusBadRequest, "`tz` is not a valid IANA time zone")
	}
	return loc, nil
}

// noteFileName turns a title into a name SanitizePath accepts: letters, marks,
// numbers, '.', '_' and '-'. Other runs become one '-'; at most 120 bytes.
func noteFileName(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.TrimSpace(title) {
		if unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) || r == '_' || r == '.' {
			if b.Len()+utf8.RuneLen(r) > 120 {
				break
			}
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 && b.Len() < 120 {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		name = "untitled"
	}
	return name
}

// ensureSpace returns the vault's Space with this slug, creating it when
// missing.
func (m *Module) ensureSpace(ctx context.Context, tenant, actor uuid.UUID, s systemSpace) (uuid.UUID, error) {
	find := func() (uuid.UUID, error) {
		return m.q.SpaceIDBySlug(ctx, db.SpaceIDBySlugParams{TenantID: tenant, Slug: s.slug})
	}
	id, err := find()
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	if m.spaces == nil {
		return uuid.Nil, errors.New("vault: no space service to create " + s.slug)
	}
	if err := m.spaces.CreateSystemSpace(ctx, tenant, actor, s.name, s.slug, s.icon); err != nil {
		// Another request may have created it first; the lookup decides.
		if id, ferr := find(); ferr == nil {
			return id, nil
		}
		return uuid.Nil, err
	}
	return find()
}

var errNoteExists = wire.NewError(http.StatusConflict, "a note with that name already exists")

// createNote writes a new note unless the path is taken. It returns the row
// and whether it created the file; an existing file is returned as is. The
// existence check and the write hold the E5.1 path lock, so two devices
// asking at once get one file.
func (m *Module) createNote(ctx context.Context, tenant, actor uuid.UUID, spaceID *uuid.UUID, path string, data []byte, meta *vaultfiles.Metadata) (*vaultfiles.VaultFile, bool, error) {
	if err := m.paths.EnsureTenantDirectories(tenant); err != nil {
		return nil, false, err
	}
	target, err := vaultfiles.ResolveInside(m.paths.Raw(tenant), path)
	if err != nil {
		return nil, false, err
	}
	unlock := m.locks.lock(tenant, path)
	defer unlock()
	if _, err := os.Stat(target); err == nil {
		row, err := m.existingRow(ctx, tenant, actor, spaceID, path, target)
		return row, false, err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	if m.guard != nil {
		if err := m.guard.Check(int64(len(data))); err != nil {
			return nil, false, err
		}
	}
	if err := vaultfiles.ReplaceFile(target, data); err != nil {
		return nil, false, err
	}
	id, err := m.saveRow(ctx, tenant, actor, spaceID, path, data, meta)
	if err != nil {
		return nil, false, err
	}
	m.upsertNoteMemory(ctx, tenant, id, spaceID, path, data, meta)
	row, err := m.store.FindByID(ctx, tenant, id)
	return row, true, err
}

func (m *Module) saveRow(ctx context.Context, tenant, actor uuid.UUID, spaceID *uuid.UUID, path string, data []byte, meta *vaultfiles.Metadata) (uuid.UUID, error) {
	id, err := m.store.Upsert(ctx, vaultfiles.UpsertParams{
		TenantID: tenant, SpaceID: spaceID, Path: path, ContentType: "text/markdown",
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data), Metadata: meta,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, m.store.StampActors(ctx, id, actor)
}

// existingRow returns the row of a file already on disk, adding one when a
// writer left the file without a row.
func (m *Module) existingRow(ctx context.Context, tenant, actor uuid.UUID, spaceID *uuid.UUID, path, target string) (*vaultfiles.VaultFile, error) {
	row, err := m.store.FindByPath(ctx, tenant, path)
	if err != nil || row != nil {
		return row, err
	}
	data, err := os.ReadFile(target) //nolint:gosec // target comes from ResolveInside
	if err != nil {
		return nil, err
	}
	id, err := m.saveRow(ctx, tenant, actor, spaceID, path, data, nil)
	if err != nil {
		return nil, err
	}
	return m.store.FindByID(ctx, tenant, id)
}

// seedTemplates writes the starter set into the Templates Space, never
// overwriting a template the user already has.
func (m *Module) seedTemplates(c *echo.Context) error {
	actor, err := requireIdentity(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	spaceID, err := m.ensureSpace(ctx, a.VaultID, actor, templatesSpace)
	if err != nil {
		return err
	}
	created := []string{}
	for _, t := range starterTemplates {
		_, made, err := m.createNote(ctx, a.VaultID, actor, &spaceID, templatesSpace.slug+"/"+t.name, []byte(t.body), nil)
		if err != nil {
			return err
		}
		if made {
			created = append(created, t.name)
		}
	}
	return wire.JSON(c, http.StatusOK, VaultTemplatesSeedResponse{SpaceId: wire.UUID{UUID: spaceID}, Created: created})
}

func (m *Module) noteFromTemplate(c *echo.Context) error {
	actor, err := requireIdentity(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[VaultFromTemplateRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	tenant := a.VaultID
	title := strings.TrimSpace(body.Title)
	if title == "" {
		return wire.NewError(http.StatusBadRequest, "`title` is required")
	}
	loc, err := location(deref(body.Tz))
	if err != nil {
		return err
	}
	tplPath, err := vaultfiles.SanitizePath(templatesSpace.slug + "/" + vaultfiles.LastPathComponent(body.Template))
	if err != nil {
		return err
	}
	tplTarget, err := vaultfiles.ResolveInside(m.paths.Raw(tenant), tplPath)
	if err != nil {
		return err
	}
	src, err := os.ReadFile(tplTarget) //nolint:gosec // tplTarget comes from SanitizePath and ResolveInside
	if errors.Is(err, fs.ErrNotExist) {
		return wire.NewError(http.StatusNotFound, "no template with that name")
	}
	if err != nil {
		return err
	}
	spaceID, folder, err := m.uploadSpace(ctx, tenant, spaceParam(body.SpaceId))
	if err != nil {
		return err
	}
	spaceName := ""
	if spaceID != nil {
		spaceName = m.spaceName(ctx, tenant, *spaceID)
	}
	path, err := vaultfiles.SanitizePath(folder + "/" + noteFileName(title) + ".md")
	if err != nil {
		return err
	}
	now := m.now().In(loc)
	data := []byte(renderTemplate(string(src), templateVars{
		date: now.Format(time.DateOnly), time: now.Format("15:04"), title: title, space: spaceName,
	}))
	row, made, err := m.createNote(ctx, tenant, actor, spaceID, path, data, &vaultfiles.Metadata{Title: &title})
	if err != nil {
		return err
	}
	if !made {
		return errNoteExists
	}
	slog.InfoContext(ctx, "vault note from template", "tenant", ids.Upper(tenant), "path", path)
	m.analytics.Capture("vault_note_from_template", ids.Upper(actor), map[string]any{"template": vaultfiles.LastPathComponent(tplPath)})
	return wire.JSON(c, http.StatusCreated, toDTO(row))
}

func (m *Module) dailyNote(c *echo.Context) error {
	actor, err := requireIdentity(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	tenant := a.VaultID
	loc, err := location(wire.Query(c, "tz"))
	if err != nil {
		return err
	}
	now := m.now().In(loc)
	day := now
	if raw := wire.Query(c, "date"); raw != "" {
		day, err = time.ParseInLocation(time.DateOnly, raw, loc)
		if err != nil {
			return wire.NewError(http.StatusBadRequest, "`date` must be YYYY-MM-DD")
		}
	}
	date := day.Format(time.DateOnly)
	path := dailySpace.slug + "/" + date + ".md"

	// Fast path: today's note exists, so no Space or template work.
	if row, err := m.store.FindByPath(ctx, tenant, path); err != nil {
		return err
	} else if row != nil {
		return wire.JSON(c, http.StatusOK, toDTO(row))
	}

	spaceID, err := m.ensureSpace(ctx, tenant, actor, dailySpace)
	if err != nil {
		return err
	}
	src := dailyStarter
	if target, err := vaultfiles.ResolveInside(m.paths.Raw(tenant), templatesSpace.slug+"/daily.md"); err == nil {
		if b, err := os.ReadFile(target); err == nil { //nolint:gosec // target comes from ResolveInside
			src = string(b)
		}
	}
	data := []byte(renderTemplate(src, templateVars{
		date: date, time: now.Format("15:04"), title: date, space: dailySpace.name,
	}))
	row, made, err := m.createNote(ctx, tenant, actor, &spaceID, path, data, &vaultfiles.Metadata{Title: &date})
	if err != nil {
		return err
	}
	status := http.StatusOK
	if made {
		status = http.StatusCreated
		m.analytics.Capture("vault_daily_note_created", ids.Upper(actor), nil)
	}
	return wire.JSON(c, status, toDTO(row))
}

// spaceName is the Space's display name, or "" when it can't be read: a
// template variable isn't worth failing the request for.
func (m *Module) spaceName(ctx context.Context, tenant, id uuid.UUID) string {
	var name string
	err := m.pool.QueryRow(ctx, `SELECT name FROM spaces WHERE tenant_id = $1 AND id = $2`, tenant, id).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func spaceParam(id *wire.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
