package megateways

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/secretbox"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type sidecarCall struct {
	path, token string
	body        map[string]any
}

type sidecarStub struct {
	mu     sync.Mutex
	calls  []sidecarCall
	status int
	reply  string
}

func (s *sidecarStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		s.mu.Lock()
		s.calls = append(s.calls, sidecarCall{r.URL.Path, r.Header.Get("X-Lumina-Sidecar-Token"), body})
		status, reply := s.status, s.reply
		s.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *sidecarStub) last() sidecarCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

func TestSidecarClientActivateDeactivate(t *testing.T) {
	stub := &sidecarStub{}
	srv := stub.server(t)
	c := NewSidecarClient(srv.URL+"/", "tok", slog.Default())
	tenant := uuid.MustParse("6A7E0000-0000-4000-8000-0000000000A1")
	require.NoError(t, c.Activate(context.Background(), "proj", "sec", tenant, nil))
	got := stub.last()
	require.Equal(t, "/control/activate", got.path)
	require.Equal(t, "tok", got.token)
	require.Equal(t, map[string]any{"projectId": "proj", "projectSecret": "sec", "tenantId": "6a7e0000-0000-4000-8000-0000000000a1", "options": map[string]any{}}, got.body)

	require.NoError(t, c.Deactivate(context.Background(), "proj"))
	require.Equal(t, "/control/deactivate", stub.last().path)
	require.Equal(t, map[string]any{"projectId": "proj"}, stub.last().body)
}

func TestSidecarClientErrors(t *testing.T) {
	stub := &sidecarStub{status: http.StatusForbidden}
	c := NewSidecarClient(stub.server(t).URL, "tok", slog.Default())
	require.ErrorIs(t, c.Activate(context.Background(), "p", "s", uuid.New(), nil), ErrSidecarUnauthorized)
	stub.status, stub.reply = http.StatusBadGateway, "upstream down"
	var httpErr *SidecarHTTPError
	require.ErrorAs(t, c.Deactivate(context.Background(), "p"), &httpErr)
	require.Equal(t, 502, httpErr.Status)
	require.Equal(t, "upstream down", httpErr.Body)
}

func TestSidecarClientSend(t *testing.T) {
	stub := &sidecarStub{reply: `{"ok":false,"messageId":"m1"}`}
	c := NewSidecarClient(stub.server(t).URL, "tok", slog.Default())
	text := "hi"
	name := "a.png"
	res, err := c.Send(context.Background(), "p", "space", &text, []SidecarAttachment{{Name: &name, Kind: "attachment"}})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Equal(t, "m1", *res.MessageID)
	body := stub.last().body
	require.Equal(t, "hi", body["text"])
	att := body["attachments"].([]any)[0].(map[string]any)
	require.Equal(t, "a.png", att["name"])
	require.Nil(t, att["path"])
	require.Equal(t, "attachment", att["kind"])

	stub.reply = "not json"
	res, err = c.Send(context.Background(), "p", "space", nil, nil)
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Nil(t, res.MessageID)
	_, hasText := stub.last().body["text"]
	require.False(t, hasText)
}

func TestSidecarClientHealth(t *testing.T) {
	stub := &sidecarStub{reply: `{"activeProjects":3}`}
	c := NewSidecarClient(stub.server(t).URL, "tok", slog.Default())
	require.Equal(t, SidecarHealth{Healthy: true, ActiveProjects: 3}, c.Health(context.Background()))
	stub.reply = `{}`
	require.Equal(t, SidecarHealth{Healthy: true, ActiveProjects: -1}, c.Health(context.Background()))
	stub.status = 500
	require.Equal(t, SidecarHealth{Reason: "bad_status"}, c.Health(context.Background()))
	dead := NewSidecarClient("http://127.0.0.1:1", "tok", slog.Default())
	require.Equal(t, SidecarHealth{Reason: "unreachable"}, dead.Health(context.Background()))
}

type fixedContainers struct {
	hermescontainers.Containers
	port int
}

func (f fixedContainers) EnsureRunning(_ context.Context, tenant uuid.UUID) (*hermescontainers.Handle, error) {
	return &hermescontainers.Handle{TenantID: tenant, Port: f.port, APIServerKey: "k"}, nil
}

func TestDeliverySendsReplyBack(t *testing.T) {
	hermes := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msg := body["messages"].([]any)[0].(map[string]any)["content"]
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"echo: `+msg.(string)+`"}}]}`)
	}))
	defer hermes.Close()
	host, portStr, _ := strings.Cut(strings.TrimPrefix(hermes.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	stub := &sidecarStub{reply: `{"ok":true}`}
	d := &delivery{
		client: NewSidecarClient(stub.server(t).URL, "tok", slog.Default()), containers: fixedContainers{port: port},
		http: &http.Client{Timeout: 5 * time.Second}, log: slog.Default(), hostBase: "http://" + host,
	}
	d.deliver(context.Background(), InboundEvent{ProjectID: "p", TenantID: uuid.New(), SpaceID: "sp", Text: "hello"})
	got := stub.last()
	require.Equal(t, "/control/send", got.path)
	require.Equal(t, "echo: hello", got.body["text"])
	require.Equal(t, "sp", got.body["spaceId"])
}

func TestPhotonInboundWithSidecar(t *testing.T) {
	stub := &sidecarStub{}
	m := New(nil, Options{
		SidecarToken: "s3cret", Sidecar: NewSidecarClient(stub.server(t).URL, "s3cret", slog.Default()),
		Containers: fixedContainers{port: 1}, DeliveryHost: "http://127.0.0.1",
	})
	require.NotNil(t, m.delivery)
	srv := echoFor(m)
	do := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/gateways/photon/inbound", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Lumina-Sidecar-Token", "s3cret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, http.StatusBadRequest, do(`{"projectId":"p"}`))
	// Hermes is unreachable: delivery logs and the webhook still answers 200.
	require.Equal(t, http.StatusOK, do(`{"projectId":"p","tenantId":"`+uuid.NewString()+`","spaceId":"s","text":"hi"}`))
}

// Activation runs after the photon row is persisted.
func TestPhotonPersistActivatesSidecar(t *testing.T) {
	v := setup(t)
	stub := &sidecarStub{}
	p := newPhoton(v.pool, testBox(t), NewSidecarClient(stub.server(t).URL, "tok", slog.Default()), slog.Default())
	tenant := v.user()
	id := uuid.New()
	p.byTenant[tenant] = id
	require.NoError(t, p.persist(context.Background(), id, "spec-1", "secret-1", "dash-1", "+15555550100", nil))
	got := stub.last()
	require.Equal(t, "/control/activate", got.path)
	require.Equal(t, "spec-1", got.body["projectId"])
	require.Equal(t, "secret-1", got.body["projectSecret"])
	var status string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT status FROM user_hermes_gateways WHERE tenant_id = $1 AND gateway_id = 'photon'`, tenant).Scan(&status))
	require.Equal(t, "configured", status)
}

func echoFor(m *Module) http.Handler {
	return httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, nil), Modules: []httpserver.Module{m}})
}

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	require.NoError(t, err)
	return box
}
