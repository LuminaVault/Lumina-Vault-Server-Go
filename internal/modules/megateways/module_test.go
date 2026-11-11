package megateways

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  http.Handler
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	require.NoError(t, err)
	docker := hermescontainers.ProcessDocker{BinaryPath: "/nonexistent/docker"}
	life := &hermescontainers.Lifecycle{Manager: hermescontainers.New(pool, box), Docker: docker, Config: hermescontainers.Config{Network: "n"}}
	m := New(pool, Options{Box: box, Limits: ratelimit.NewStore(), Containers: life, Docker: docker})
	srv := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{m}})
	return env{t: t, pool: pool, srv: srv}
}

func (v env) user() uuid.UUID {
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, username, tier, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, 'trial', now(), now())`, id, id.String()+"@t", "u"+strings.ReplaceAll(id.String(), "-", "")[:12])
	require.NoError(v.t, err)
	return id
}

func (v env) do(user uuid.UUID, method, path, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := scenario.Mint(keys, user)
	require.NoError(v.t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func decodeEntry(t *testing.T, body string) CatalogEntry {
	t.Helper()
	var e CatalogEntry
	require.NoError(t, json.Unmarshal([]byte(body), &e))
	return e
}

const telegramPut = `{"config":{"bot_token":"TELEGRAM-SECRET-XYZ-123","allowed_users":"123456789"}}`

// Ported from HermesGatewaysControllerTests: list returns every catalog gateway as not configured.
func TestListAllNotConfigured(t *testing.T) {
	v := setup(t)
	code, body := v.do(v.user(), http.MethodGet, "/v1/me/hermes-gateways", "")
	require.Equal(t, http.StatusOK, code)
	var list listResponse
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	require.Len(t, list.Items, len(gatewayIDs))
	for i, item := range list.Items {
		require.Equal(t, gatewayIDs[i], item.ID)
		require.Equal(t, "not_configured", item.Status)
		require.False(t, item.HasConfig)
		require.Nil(t, item.VerifiedAt)
		require.Nil(t, item.LastFailureCode)
		if item.PairingKind == nil {
			require.NotEmpty(t, item.RequiredFields, item.ID)
		} else {
			require.Empty(t, item.RequiredFields, item.ID)
		}
	}
}

// Ported from HermesGatewaysControllerTests: PUT then GET round-trips and never echoes secret config.
func TestPutGetNeverEchoesSecret(t *testing.T) {
	v := setup(t)
	u := v.user()
	code, body := v.do(u, http.MethodPut, "/v1/me/hermes-gateways/telegram", telegramPut)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, body, "TELEGRAM-SECRET-XYZ-123")
	require.NotContains(t, body, `"config"`)
	e := decodeEntry(t, body)
	require.Equal(t, "telegram", e.ID)
	require.Equal(t, "configured", e.Status)
	require.True(t, e.HasConfig)

	code, body = v.do(u, http.MethodGet, "/v1/me/hermes-gateways/telegram", "")
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, body, "TELEGRAM-SECRET-XYZ-123")
	e = decodeEntry(t, body)
	require.Equal(t, "configured", e.Status)
}

// Ported from HermesGatewaysControllerTests: PUT rejects missing required field with stable code.
func TestPutMissingField(t *testing.T) {
	v := setup(t)
	code, body := v.do(v.user(), http.MethodPut, "/v1/me/hermes-gateways/discord", `{"config":{"bot_token":"abc"}}`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, body, "missing_field:allowed_users")
}

// Ported from HermesGatewaysControllerTests: unknown gateway id is 404 unsupported_gateway.
func TestUnsupportedGateway(t *testing.T) {
	v := setup(t)
	code, body := v.do(v.user(), http.MethodPut, "/v1/me/hermes-gateways/signal", `{"config":{}}`)
	require.Equal(t, http.StatusNotFound, code)
	require.Contains(t, body, "unsupported_gateway")
}

// Ported from HermesGatewaysControllerTests: DELETE resets to not configured.
func TestDeleteResets(t *testing.T) {
	v := setup(t)
	u := v.user()
	code, _ := v.do(u, http.MethodPut, "/v1/me/hermes-gateways/telegram", telegramPut)
	require.Equal(t, http.StatusOK, code)
	code, _ = v.do(u, http.MethodDelete, "/v1/me/hermes-gateways/telegram", "")
	require.Equal(t, http.StatusNoContent, code)
	code, body := v.do(u, http.MethodGet, "/v1/me/hermes-gateways/telegram", "")
	require.Equal(t, http.StatusOK, code)
	e := decodeEntry(t, body)
	require.Equal(t, "not_configured", e.Status)
	require.False(t, e.HasConfig)
}

// Ported from HermesGatewaysControllerTests: tenants are isolated.
func TestTenantIsolation(t *testing.T) {
	v := setup(t)
	code, _ := v.do(v.user(), http.MethodPut, "/v1/me/hermes-gateways/telegram", telegramPut)
	require.Equal(t, http.StatusOK, code)
	code, body := v.do(v.user(), http.MethodGet, "/v1/me/hermes-gateways/telegram", "")
	require.Equal(t, http.StatusOK, code)
	e := decodeEntry(t, body)
	require.Equal(t, "not_configured", e.Status)
	require.False(t, e.HasConfig)
}

// Constant segments shadow :id the way Hummingbird's trie does.
func TestRouteShadowing(t *testing.T) {
	v := setup(t)
	u := v.user()
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/me/hermes-gateways/apply"},
		{http.MethodPut, "/v1/me/hermes-gateways/apply"},
		{http.MethodDelete, "/v1/me/hermes-gateways/apply"},
		{http.MethodGet, "/v1/me/hermes-gateways/whatsapp"},
		{http.MethodPut, "/v1/me/hermes-gateways/whatsapp"},
		{http.MethodGet, "/v1/me/hermes-gateways/photon"},
		{http.MethodDelete, "/v1/me/hermes-gateways/photon"},
	} {
		code, body := v.do(u, c.method, c.path, "")
		require.Equal(t, http.StatusNotFound, code, c.method+" "+c.path)
		require.NotContains(t, body, "unsupported_gateway", c.method+" "+c.path)
	}
	code, body := v.do(u, http.MethodGet, "/v1/me/hermes-gateways/apply/not-a-uuid", "")
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, body, "invalid_job_id")
	code, body = v.do(u, http.MethodGet, "/v1/me/hermes-gateways/apply/current", "")
	require.Equal(t, http.StatusNotFound, code)
	require.Contains(t, body, "no_apply_job")
	code, body = v.do(u, http.MethodGet, "/v1/me/hermes-gateways/apply/"+uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, code)
	require.Contains(t, body, "job_not_found")
}

func runParser(lines []string) []pairEvent {
	var p pairParser
	var events []pairEvent
	for _, l := range lines {
		events = append(events, p.consume(l)...)
	}
	return append(events, p.finish()...)
}

func countType(events []pairEvent, typ string) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func hasStatus(events []pairEvent, s string) bool {
	for _, e := range events {
		if e.Type == "status" && *e.Payload == s {
			return true
		}
	}
	return false
}

// Ported from WhatsAppPairParserTests (8 cases).
func TestWhatsAppPairParser(t *testing.T) {
	t.Run("emits QR frame once block ends", func(t *testing.T) {
		rows := make([]string, 6)
		for i := range rows {
			rows[i] = "█▀▀▀█ ▄▄ █▀▀▀█"
		}
		events := runParser(append(append([]string{"Scan this QR code:", ""}, rows...), "", "Waiting for scan…"))
		var qrs []string
		for _, e := range events {
			if e.Type == "qr" {
				qrs = append(qrs, *e.Payload)
			}
		}
		require.Len(t, qrs, 1)
		require.Contains(t, qrs[0], "█▀▀▀█")
		require.Len(t, strings.Split(qrs[0], "\n"), 6)
	})
	t.Run("ignores short block noise", func(t *testing.T) {
		require.Zero(t, countType(runParser([]string{"██", "██", "done"}), "qr"))
	})
	t.Run("detects linked success", func(t *testing.T) {
		require.Equal(t, 1, countType(runParser([]string{"Device linked! You're all set."}), "linked"))
	})
	t.Run("detects expiry", func(t *testing.T) {
		require.True(t, hasStatus(runParser([]string{"The QR code expired, generating a new one…"}), "expired"))
	})
	t.Run("detects error", func(t *testing.T) {
		var msg string
		for _, e := range runParser([]string{"Error: could not connect to WhatsApp"}) {
			if e.Type == "error" {
				msg = *e.Payload
			}
		}
		require.Contains(t, strings.ToLower(msg), "could not connect")
	})
	t.Run("strips ANSI before classifying", func(t *testing.T) {
		require.Equal(t, 1, countType(runParser([]string{"\x1b[32mDevice linked successfully\x1b[0m"}), "linked"))
	})
	t.Run("ansi only and blank lines are inert", func(t *testing.T) {
		require.Empty(t, runParser([]string{"\x1b[2J", "\x1b[H", "   "}))
	})
	t.Run("emits fresh QR frame after refresh", func(t *testing.T) {
		frame := []string{"█ █ █ █ █ █", "█ █ █ █ █ █", "█ █ █ █ █ █", "█ █ █ █ █ █", "█ █ █ █ █ █"}
		lines := append(append(append(append([]string{}, frame...), "refreshing"), frame...), "scan now")
		require.Equal(t, 2, countType(runParser(lines), "qr"))
	})
}

func sidecarServe(token string, header map[string]string) int {
	m := &Module{opt: Options{SidecarToken: token, Log: slog.Default()}}
	e := echo.New()
	e.POST("/inbound", func(c *echo.Context) error { return c.NoContent(http.StatusOK) }, m.sidecarToken)
	req := httptest.NewRequest(http.MethodPost, "/inbound", strings.NewReader("{}"))
	for k, val := range header {
		req.Header.Set(k, val)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code
}

// Ported from SidecarTokenMiddlewareTests (5 cases), with Swift's stub handler.
func TestSidecarTokenMiddleware(t *testing.T) {
	require.Equal(t, http.StatusUnauthorized, sidecarServe("s3cret", nil), "missing sidecar token is rejected")
	require.Equal(t, http.StatusUnauthorized, sidecarServe("s3cret", map[string]string{"X-Lumina-Sidecar-Token": "nope"}), "wrong sidecar token is rejected")
	require.Equal(t, http.StatusOK, sidecarServe("s3cret", map[string]string{"X-Lumina-Sidecar-Token": "s3cret"}), "sidecar token header is accepted")
	require.Equal(t, http.StatusOK, sidecarServe("s3cret", map[string]string{"Authorization": "Bearer s3cret"}), "bearer sidecar token is accepted")
	require.Equal(t, http.StatusNotFound, sidecarServe("", map[string]string{"X-Lumina-Sidecar-Token": "s3cret"}), "empty configured sidecar token disables webhook")
}
