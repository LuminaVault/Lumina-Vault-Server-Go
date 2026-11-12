package hermesmirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"lumina/internal/hermesdash"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Artifact limits (HermesMirrorArtifacts, HermesArtifact.retainLimit).
const (
	ArtifactSessionCap  = 200
	ArtifactRetainLimit = 500
)

// ArtifactRecord is HermesArtifactExtractor.Record.
type ArtifactRecord struct {
	Kind, Value, Href, Label string
	SessionID, SessionTitle  string
	OccurredAt               time.Time
	ContentHash              string
}

// ICU's \s inside a character class: Go's \s is ASCII only.
const ws = `\t\n\v\f\r\x{85}\p{Z}`

var (
	reImage = regexp.MustCompile(`!\[(?:[^\]]*)\]\(([^)` + ws + `]+)\)`)
	reLink  = regexp.MustCompile(`\[(?:[^\]]+)\]\(([^)` + ws + `]+)\)`)
	reURL   = regexp.MustCompile(`https?://[^` + ws + `<>"')]+`)
	rePath  = regexp.MustCompile("(?:^|[" + ws + "(\"'`])((?:/|~/|\\.\\.?/)[^" + ws + "\"'`<>]+(?:\\.[A-Za-z0-9]{1,8})?)")

	imageExt = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp"}
	fileExt  = []string{
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp",
		".pdf", ".txt", ".json", ".md", ".csv", ".zip", ".tar", ".gz",
		".mp3", ".wav", ".mp4", ".mov",
	}
	keyHint = []string{"path", "file", "url", "image", "artifact", "output", "download", "result", "target"}
)

// ExtractArtifacts is HermesArtifactExtractor.extract: images, files and links
// from assistant and tool messages, one record per session and value.
func ExtractArtifacts(sessionID, sessionTitle string, sessionTimestamp *time.Time, messages []hermesdash.Message) []ArtifactRecord {
	found := map[string]ArtifactRecord{}
	title := swiftstr.TrimWhitespacesAndNewlines(sessionTitle)
	if title == "" {
		title = "Untitled session"
	}
	for _, m := range messages {
		if m.Role != "assistant" && m.Role != "tool" {
			continue
		}
		occurred := time.Unix(0, 0).UTC()
		if m.Timestamp != nil {
			occurred = *m.Timestamp
		} else if sessionTimestamp != nil {
			occurred = *sessionTimestamp
		}
		collectArtifacts(m.Content, func(candidate string) {
			value := normalizeArtifact(candidate)
			if value == "" || !looksLikeArtifact(value) {
				return
			}
			key := sessionID + ":" + value
			if _, ok := found[key]; ok {
				return
			}
			kind := artifactKind(value)
			found[key] = ArtifactRecord{
				Kind: kind, Value: value, Href: value, Label: artifactLabel(value),
				SessionID: sessionID, SessionTitle: title, OccurredAt: occurred,
				ContentHash: ArtifactHash(kind, value, sessionID),
			}
		})
	}
	out := make([]ArtifactRecord, 0, len(found))
	for _, r := range found {
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OccurredAt.After(out[j].OccurredAt) })
	return out
}

// ArtifactHash is sha256 hex of "kind|value|sessionID".
func ArtifactHash(kind, value, sessionID string) string {
	sum := sha256.Sum256([]byte(kind + "|" + value + "|" + sessionID))
	return hex.EncodeToString(sum[:])
}

func groups(re *regexp.Regexp, text string, group int) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		if m[2*group] < 0 {
			continue
		}
		out = append(out, text[m[2*group]:m[2*group+1]])
	}
	return out
}

func collectArtifacts(text string, push func(string)) {
	for _, v := range groups(reImage, text, 1) {
		push(v)
	}
	for _, v := range groups(reLink, text, 1) {
		if looksLikeArtifact(v) {
			push(v)
		}
	}
	for _, v := range groups(reURL, text, 0) {
		if looksLikeArtifact(v) {
			push(v)
		}
	}
	for _, v := range groups(rePath, text, 1) {
		push(v)
	}
	trimmed := swiftstr.TrimWhitespacesAndNewlines(text)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return
	}
	var parsed any
	if json.Unmarshal([]byte(trimmed), &parsed) != nil {
		return
	}
	collectStrings(parsed, "tool_result", func(value, keyPath string) {
		normalized := normalizeArtifact(value)
		if normalized == "" {
			return
		}
		lower := strings.ToLower(keyPath)
		hinted := false
		for _, h := range keyHint {
			if strings.Contains(lower, h) {
				hinted = true
				break
			}
		}
		if (hinted || looksLikePathOrURL(normalized)) && looksLikeArtifact(normalized) {
			push(normalized)
		}
	})
}

func collectStrings(value any, keyPath string, into func(string, string)) {
	switch v := value.(type) {
	case string:
		into(v, keyPath)
	case []any:
		for i, child := range v {
			collectStrings(child, keyPath+"."+strconv.Itoa(i), into)
		}
	case map[string]any:
		for key, child := range v {
			next := keyPath + "." + key
			if keyPath == "" {
				next = key
			}
			collectStrings(child, next, into)
		}
	}
}

func containsAny(value string, list []string) bool {
	lower := strings.ToLower(value)
	for _, ext := range list {
		if strings.Contains(lower, ext) {
			return true
		}
	}
	return false
}

func artifactKind(value string) string {
	if strings.HasPrefix(value, "data:image/") || containsAny(value, imageExt) {
		return "image"
	}
	for _, p := range []string{"/", "./", "../", "~/", "file://"} {
		if strings.HasPrefix(value, p) {
			return "file"
		}
	}
	return "link"
}

func artifactLabel(value string) string {
	if u, err := url.Parse(value); err == nil && u.Host != "" {
		parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1]
		}
		return u.Hostname()
	}
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return value
	}
	return parts[len(parts)-1]
}

func looksLikeArtifact(value string) bool {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "data:image/") {
		return true
	}
	if looksLikePathOrURL(value) && (containsAny(value, imageExt) || containsAny(value, fileExt)) {
		return true
	}
	return strings.HasPrefix(value, "/") && strings.Contains(value, ".")
}

func looksLikePathOrURL(value string) bool {
	for _, p := range []string{"http://", "https://", "file://", "data:image/", "/", "./", "../", "~/"} {
		if strings.HasPrefix(value, p) {
			return true
		}
	}
	return false
}

func normalizeArtifact(value string) string {
	return strings.Trim(swiftstr.TrimWhitespacesAndNewlines(value), "),.;")
}

// CollectArtifacts walks recent sessions, harvests artifacts and upserts them
// by content hash, keeping the newest ArtifactRetainLimit per tenant.
func (s *Service) CollectArtifacts(ctx context.Context, tenant uuid.UUID) (int, error) {
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return 0, err
	}
	page, err := transport.ListSessions(ctx, 0, ArtifactSessionCap)
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, session := range page.Sessions {
		messages, err := transport.SessionMessages(ctx, session.ID)
		if err != nil {
			s.log().DebugContext(ctx, "hermes artifacts session skipped", "tenant", ids.Upper(tenant), "session", session.ID, "error", Describe(err))
			continue
		}
		title := ""
		if session.Title != nil {
			title = *session.Title
		}
		stamp := session.LastActiveAt
		if stamp == nil {
			stamp = session.StartedAt
		}
		for _, r := range ExtractArtifacts(session.ID, title, stamp, messages) {
			var n int
			if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM hermes_artifacts WHERE tenant_id = $1 AND content_hash = $2`, tenant, r.ContentHash).Scan(&n); err != nil {
				return inserted, err
			}
			if n > 0 {
				continue
			}
			if _, err := s.Pool.Exec(ctx, `INSERT INTO hermes_artifacts (id, tenant_id, kind, value, href, label, session_id,
				session_title, occurred_at, content_hash, collected_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				uuid.New(), tenant, r.Kind, r.Value, r.Href, r.Label, r.SessionID, r.SessionTitle, r.OccurredAt, r.ContentHash, s.now()); err != nil {
				return inserted, err
			}
			inserted++
		}
	}
	_, err = s.Pool.Exec(ctx, `DELETE FROM hermes_artifacts WHERE tenant_id = $1 AND id NOT IN (
		SELECT id FROM hermes_artifacts WHERE tenant_id = $1 ORDER BY occurred_at DESC LIMIT $2)`, tenant, ArtifactRetainLimit)
	return inserted, err
}

// ArtifactQuery is the list filter.
type ArtifactQuery struct {
	Kind, Query, SessionID *string
	Before                 *time.Time
	Limit                  int
}

func scanArtifact(r pgx.CollectableRow) (ArtifactDTO, error) {
	var dto ArtifactDTO
	var id uuid.UUID
	var occurred time.Time
	err := r.Scan(&id, &dto.Kind, &dto.Value, &dto.Href, &dto.Label, &dto.SessionID, &dto.SessionTitle, &occurred)
	dto.ID = wire.UUID{UUID: id}
	dto.OccurredAt = wire.Time{Time: occurred}
	switch dto.Kind {
	case "image", "file", "link":
	default:
		dto.Kind = "link"
	}
	return dto, err
}

const artifactColumns = `id, kind, value, href, label, session_id, session_title, occurred_at`

// Artifacts is artifacts(tenantID:kind:query:before:limit:sessionID:).
func (s *Service) Artifacts(ctx context.Context, tenant uuid.UUID, q ArtifactQuery) (ArtifactListResponse, error) {
	bounded := max(1, min(q.Limit, 100))
	where := []string{"tenant_id = $1"}
	args := []any{tenant}
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args))))
	}
	if q.Kind != nil {
		switch *q.Kind {
		case "image", "file", "link":
			add("kind = ?", *q.Kind)
		}
	}
	if q.SessionID != nil && *q.SessionID != "" {
		add("session_id = ?", *q.SessionID)
	}
	if q.Before != nil {
		add("occurred_at < ?", *q.Before)
	}
	if q.Query != nil {
		if needle := swiftstr.TrimWhitespacesAndNewlines(*q.Query); needle != "" {
			escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(needle)
			add("(label ILIKE ? OR value ILIKE ? OR session_title ILIKE ?)", "%"+escaped+"%")
		}
	}
	args = append(args, bounded+1)
	rows, err := s.Pool.Query(ctx, `SELECT `+artifactColumns+` FROM hermes_artifacts WHERE `+strings.Join(where, " AND ")+
		` ORDER BY occurred_at DESC, id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return ArtifactListResponse{}, err
	}
	all, err := pgx.CollectRows(rows, scanArtifact)
	if err != nil {
		return ArtifactListResponse{}, err
	}
	page := all
	if len(page) > bounded {
		page = page[:bounded]
	}
	out := ArtifactListResponse{Artifacts: append([]ArtifactDTO{}, page...)}
	if len(all) > bounded {
		next := hermesdash.ISO(page[len(page)-1].OccurredAt.Time)
		out.NextCursor = &next
	}
	return out, nil
}

// Artifact is artifact(tenantID:id:).
func (s *Service) Artifact(ctx context.Context, tenant, id uuid.UUID) (ArtifactDTO, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+artifactColumns+` FROM hermes_artifacts WHERE tenant_id = $1 AND id = $2 LIMIT 1`, tenant, id)
	if err != nil {
		return ArtifactDTO{}, err
	}
	all, err := pgx.CollectRows(rows, scanArtifact)
	if err != nil {
		return ArtifactDTO{}, err
	}
	if len(all) == 0 {
		return ArtifactDTO{}, wire.NewError(http.StatusNotFound, "hermes_artifact_not_found")
	}
	return all[0], nil
}
