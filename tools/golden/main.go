// Command golden captures wire fixtures from a running Swift server (Phase 0
// Task 0.3) into testdata/golden. Everything comes from the real binary:
// the JWT is the one Swift minted at login, the bcrypt hash is the row Swift
// wrote, and the SecretBox value is a column Swift sealed. The Go tests then
// prove Go reads and writes the same bytes.
//
//	go run ./tools/golden --swift http://localhost:18080 --db postgres://lv:lv@localhost:15432/golden \
//	  --email golden@example.test --password 'correct horse battery staple' \
//	  --jwt-secrets g1:golden-secret-golden-secret-0000 --master-key <base64>
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type exchange struct {
	Name        string          `json:"name"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	RequestBody json.RawMessage `json:"requestBody,omitempty"`
	Status      int             `json:"status"`
	ContentType string          `json:"contentType"`
	Body        string          `json:"body"`
}

type capturer struct {
	base   string
	token  string
	client *http.Client
}

func (c *capturer) do(ctx context.Context, name, method, path, body string, auth bool) (exchange, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return exchange{}, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return exchange{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return exchange{}, err
	}
	ex := exchange{Name: name, Method: method, Path: path, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: string(raw)}
	if body != "" && json.Valid([]byte(body)) {
		ex.RequestBody = json.RawMessage(body)
	}
	fmt.Printf("  %-22s %s %s → %d\n", name, method, path, resp.StatusCode)
	return ex, nil
}

func main() {
	swift := flag.String("swift", "http://localhost:18080", "Swift base URL")
	dsn := flag.String("db", "", "the Swift server's database")
	email := flag.String("email", "golden@example.test", "bootstrap admin email")
	password := flag.String("password", "correct horse battery staple", "bootstrap admin password")
	secrets := flag.String("jwt-secrets", "g1:golden-secret-golden-secret-0000", "JWT_HMAC_SECRETS the server runs with")
	masterKey := flag.String("master-key", "", "LV_SECRET_MASTER_KEY the server runs with")
	out := flag.String("out", "testdata/golden", "output directory")
	flag.Parse()
	if err := run(context.Background(), *swift, *dsn, *email, *password, *secrets, *masterKey, *out); err != nil {
		fmt.Fprintln(os.Stderr, "golden:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, swift, dsn, email, password, secrets, masterKey, out string) error {
	if dsn == "" || masterKey == "" {
		return errors.New("--db and --master-key are required")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	c := &capturer{base: strings.TrimRight(swift, "/"), client: &http.Client{Timeout: 30 * time.Second}}

	// 1. Log in as the bootstrap admin: the token is Swift-minted.
	login, err := c.do(ctx, "login", http.MethodPost, "/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), false)
	if err != nil {
		return err
	}
	if login.Status != http.StatusOK {
		return fmt.Errorf("login: %d %s", login.Status, login.Body)
	}
	var auth struct {
		UserID      string `json:"userId"`
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal([]byte(login.Body), &auth); err != nil {
		return err
	}
	c.token = auth.AccessToken
	payload, err := jwtPayload(auth.AccessToken)
	if err != nil {
		return err
	}
	if err := write(out, "jwt.json", map[string]any{
		"hmacSecrets": secrets,
		"token":       auth.AccessToken,
		"claims":      payload,
		"header":      jwtHeader(auth.AccessToken),
	}); err != nil {
		return err
	}

	// 2. bcrypt: the hash Swift stored for that password.
	var hash string
	if err := conn.QueryRow(ctx, `SELECT password_hash FROM users WHERE email = $1`, email).Scan(&hash); err != nil {
		return fmt.Errorf("bcrypt hash: %w", err)
	}
	if err := write(out, "bcrypt.json", map[string]string{"password": password, "hash": hash}); err != nil {
		return err
	}

	// 3. SecretBox: store a BYO key, then read back the columns Swift sealed.
	const plaintext = "sk-golden"
	put, err := c.do(ctx, "byo-key-put", http.MethodPut, "/v1/me/providers/openrouter",
		fmt.Sprintf(`{"kind":"apiKey","apiKey":%q}`, plaintext), true)
	if err != nil {
		return err
	}
	if put.Status >= 300 {
		return fmt.Errorf("byo key put: %d %s", put.Status, put.Body)
	}
	table, ct, nonce, err := sealedColumns(ctx, conn)
	if err != nil {
		return err
	}
	if err := write(out, "secretbox.json", map[string]string{
		"masterKeyB64":  masterKey,
		"tenantID":      auth.UserID,
		"plaintext":     plaintext,
		"ciphertextB64": base64.StdEncoding.EncodeToString(ct),
		"nonceB64":      base64.StdEncoding.EncodeToString(nonce),
		"table":         table,
	}); err != nil {
		return err
	}

	// 4. Todos round trip.
	var todos []exchange
	add := func(ex exchange, err error) (exchange, error) {
		if err == nil {
			todos = append(todos, ex)
		}
		return ex, err
	}
	if _, err := add(c.do(ctx, "list-empty", http.MethodGet, "/v1/todos", "", true)); err != nil {
		return err
	}
	created, err := add(c.do(ctx, "create", http.MethodPost, "/v1/todos", `{"title":"Buy milk","dueAt":"2026-11-01T09:00:00Z"}`, true))
	if err != nil {
		return err
	}
	var todo struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created.Body), &todo); err != nil || todo.ID == "" {
		return fmt.Errorf("create todo: %d %s", created.Status, created.Body)
	}
	for _, step := range []struct{ name, method, path, body string }{
		{"create-undated", http.MethodPost, "/v1/todos", `{"title":"Call mum"}`},
		{"list", http.MethodGet, "/v1/todos", ""},
		{"patch-done", http.MethodPatch, "/v1/todos/" + todo.ID, `{"done":true}`},
		{"patch-title", http.MethodPatch, "/v1/todos/" + todo.ID, `{"title":"Buy oat milk"}`},
		{"list-after-patch", http.MethodGet, "/v1/todos", ""},
		{"list-done-filter", http.MethodGet, "/v1/todos?done=true", ""},
		{"delete", http.MethodDelete, "/v1/todos/" + todo.ID, ""},
		{"list-after-delete", http.MethodGet, "/v1/todos", ""},
	} {
		if _, err := add(c.do(ctx, step.name, step.method, step.path, step.body, true)); err != nil {
			return err
		}
	}
	if err := write(out, "todos-http.json", todos); err != nil {
		return err
	}

	// 4b. Projects round trip (the pilot module), including a todoCount > 0.
	var projects []exchange
	addP := func(ex exchange, err error) (exchange, error) {
		if err == nil {
			projects = append(projects, ex)
		}
		return ex, err
	}
	if _, err := addP(c.do(ctx, "list-empty", http.MethodGet, "/v1/projects", "", true)); err != nil {
		return err
	}
	pc, err := addP(c.do(ctx, "create", http.MethodPost, "/v1/projects", `{"name":"  Home  ","description":"chores"}`, true))
	if err != nil {
		return err
	}
	var project struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(pc.Body), &project); err != nil || project.ID == "" {
		return fmt.Errorf("create project: %d %s", pc.Status, pc.Body)
	}
	for _, step := range []struct{ name, method, path, body string }{
		{"create-todo-in-project", http.MethodPost, "/v1/todos", fmt.Sprintf(`{"title":"Fix tap","projectID":%q}`, project.ID)},
		{"create-second", http.MethodPost, "/v1/projects", `{"name":"Work"}`},
		{"list", http.MethodGet, "/v1/projects", ""},
		{"list-limit", http.MethodGet, "/v1/projects?limit=1", ""},
		{"patch-rename", http.MethodPatch, "/v1/projects/" + project.ID, `{"name":"House"}`},
		{"patch-clear-description", http.MethodPatch, "/v1/projects/" + project.ID, `{"description":""}`},
		{"patch-archive", http.MethodPatch, "/v1/projects/" + project.ID, `{"archived":true}`},
		{"patch-empty-name", http.MethodPatch, "/v1/projects/" + project.ID, `{"name":"   "}`},
		{"create-empty-name", http.MethodPost, "/v1/projects", `{"name":" "}`},
		{"patch-missing", http.MethodPatch, "/v1/projects/00000000-0000-4000-8000-000000000000", `{"name":"x"}`},
		{"patch-bad-id", http.MethodPatch, "/v1/projects/nope", `{"name":"x"}`},
		{"delete", http.MethodDelete, "/v1/projects/" + project.ID, ""},
		{"delete-again", http.MethodDelete, "/v1/projects/" + project.ID, ""},
		{"list-after-delete", http.MethodGet, "/v1/projects", ""},
	} {
		if _, err := addP(c.do(ctx, step.name, step.method, step.path, step.body, true)); err != nil {
			return err
		}
	}
	if err := write(out, "projects-http.json", projects); err != nil {
		return err
	}

	// 5. Error shapes.
	var errs []exchange
	for _, e := range []struct {
		name, method, path, body string
		auth                     bool
	}{
		{"no-token", http.MethodGet, "/v1/todos", "", false},
		{"bad-token", http.MethodGet, "/v1/todos", "", false},
		{"unknown-route", http.MethodGet, "/v1/does-not-exist", "", true},
		{"malformed-json", http.MethodPost, "/v1/todos", `{"title":`, true},
		{"wrong-type", http.MethodPost, "/v1/todos", `{"title":42}`, true},
		{"fractional-date", http.MethodPost, "/v1/todos", `{"title":"x","dueAt":"2026-11-01T09:00:00.500Z"}`, true},
		{"patch-missing", http.MethodPatch, "/v1/todos/00000000-0000-4000-8000-000000000000", `{"done":true}`, true},
		{"patch-bad-id", http.MethodPatch, "/v1/todos/not-a-uuid", `{"done":true}`, true},
		{"delete-missing", http.MethodDelete, "/v1/todos/" + todo.ID, "", true},
	} {
		ex, err := c.do(ctx, e.name, e.method, e.path, e.body, e.auth)
		if err != nil {
			return err
		}
		errs = append(errs, ex)
	}
	return write(out, "errors-http.json", errs)
}

// sealedColumns finds the row the BYO key PUT wrote: a table with a
// *ciphertext and a *nonce bytea column holding exactly one row.
func sealedColumns(ctx context.Context, conn *pgx.Conn) (table string, ct, nonce []byte, err error) {
	rows, err := conn.Query(ctx, `
		SELECT c.table_name, c.column_name, n.column_name
		FROM information_schema.columns c
		JOIN information_schema.columns n
		  ON n.table_name = c.table_name AND n.table_schema = c.table_schema
		 AND n.data_type = 'bytea' AND n.column_name = replace(c.column_name, 'ciphertext', 'nonce')
		WHERE c.table_schema = 'public' AND c.data_type = 'bytea' AND c.column_name LIKE '%ciphertext'`)
	if err != nil {
		return "", nil, nil, err
	}
	type cand struct{ table, ctCol, nonceCol string }
	var cands []cand
	for rows.Next() {
		var x cand
		if err := rows.Scan(&x.table, &x.ctCol, &x.nonceCol); err != nil {
			return "", nil, nil, err
		}
		cands = append(cands, x)
	}
	rows.Close()
	for _, x := range cands {
		q := fmt.Sprintf(`SELECT %s, %s FROM %s WHERE %s IS NOT NULL`,
			pgx.Identifier{x.ctCol}.Sanitize(), pgx.Identifier{x.nonceCol}.Sanitize(), pgx.Identifier{x.table}.Sanitize(), pgx.Identifier{x.ctCol}.Sanitize())
		var found [][2][]byte
		r, err := conn.Query(ctx, q)
		if err != nil {
			return "", nil, nil, err
		}
		for r.Next() {
			var a, b []byte
			if err := r.Scan(&a, &b); err != nil {
				return "", nil, nil, err
			}
			found = append(found, [2][]byte{a, b})
		}
		r.Close()
		if len(found) == 1 {
			fmt.Printf("  sealed value found in %s.%s/%s\n", x.table, x.ctCol, x.nonceCol)
			return x.table, found[0][0], found[0][1], nil
		}
	}
	return "", nil, nil, errors.New("no table holds exactly one sealed value after the BYO key PUT")
}

func jwtPayload(token string) (json.RawMessage, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("access token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	return json.RawMessage(raw), err
}

func jwtHeader(token string) json.RawMessage {
	raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	return json.RawMessage(raw)
}

func write(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600)
}
