package megateways

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// photonAPIStub answers the Photon dashboard and Spectrum calls that
// PhotonProvisioningService makes, for the success path the parity run can't
// reach (Swift's hosts are fixed https URLs).
type photonAPIStub struct {
	mu    sync.Mutex
	calls []string
	polls int
	// hold, when set, keeps the device token pending until it is closed, so a
	// test can subscribe while the session still awaits approval.
	hold chan struct{}
}

func (s *photonAPIStub) handler(t *testing.T) http.Handler {
	t.Helper()
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("spec-1:secret-1"))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		reply := func(status int, v any) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		bearer := r.Header.Get("Authorization") == "Bearer dash-token"
		switch r.Method + " " + r.URL.Path {
		case "POST /api/auth/device/code":
			reply(200, map[string]any{
				"device_code": "dc", "user_code": "ABCD", "verification_uri": "https://app.photon.codes/device",
				"verification_uri_complete": "https://app.photon.codes/device?code=ABCD", "expires_in": 60, "interval": 0,
			})
		case "POST /api/auth/device/token":
			s.mu.Lock()
			s.polls++
			first := s.polls == 1
			s.mu.Unlock()
			held := false
			if s.hold != nil {
				select {
				case <-s.hold:
				default:
					held = true
				}
			}
			if first || held {
				reply(400, map[string]any{"error": "authorization_pending"})
				return
			}
			reply(200, map[string]any{"access_token": "dash-token"})
		case "GET /api/auth/get-session":
			if !bearer {
				w.WriteHeader(401)
				return
			}
			reply(200, map[string]any{"user": map[string]any{"id": "u"}})
		case "GET /api/projects":
			reply(200, map[string]any{"data": []any{}})
		case "POST /api/projects":
			reply(201, map[string]any{"id": "dash-1"})
		case "GET /api/projects/dash-1":
			reply(200, map[string]any{"id": "dash-1", "spectrum": true, "spectrumProjectId": "spec-1"})
		case "POST /api/projects/dash-1/regenerate-secret":
			reply(200, map[string]any{"projectSecret": "secret-1"})
		case "GET /projects/spec-1/users/":
			if r.Header.Get("Authorization") != basic {
				w.WriteHeader(401)
				return
			}
			reply(200, []any{})
		case "POST /projects/spec-1/users/":
			reply(201, map[string]any{"user": map[string]any{"phoneNumber": "+15555550100", "assignedPhoneNumber": "+15555550199"}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "unexpected "+r.Method+" "+r.URL.Path)
		}
	})
}

// The whole provisioning flow against stubs: device code, approval, phone,
// project, secret, Spectrum user, the sealed row and the sidecar activation.
func TestPhotonProvisioningFlow(t *testing.T) {
	v := setup(t)
	// subscribe replays deviceCode only while the session awaits approval; hold
	// approval until the test has subscribed, or a fast runner misses it.
	api := &photonAPIStub{hold: make(chan struct{})}
	apiSrv := httptest.NewServer(api.handler(t))
	defer apiSrv.Close()
	side := &sidecarStub{}
	p := newPhoton(v.pool, testBox(t), NewSidecarClient(side.server(t).URL, "tok", slog.Default()), slog.Default())
	p.dashboardHost, p.spectrumHost = apiSrv.URL, apiSrv.URL
	tenant := v.user()

	id, err := p.startSetup(context.Background(), tenant)
	require.NoError(t, err)
	events, cancel := p.subscribe(id)
	defer cancel()
	close(api.hold)
	require.NoError(t, p.submitPhone(id, "+1 (555) 555-0100"))

	var got []photonEvent
	timeout := time.After(15 * time.Second)
collect:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break collect
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out; events so far: %+v", got)
		}
	}
	require.Equal(t, "deviceCode", got[0].Type)
	require.Equal(t, "https://app.photon.codes/device?code=ABCD", got[0].Payload.(deviceCodePayload).VerificationURI)
	var kinds []string
	for _, ev := range got {
		if s, ok := ev.Payload.(string); ok {
			kinds = append(kinds, ev.Type+":"+s)
		}
	}
	require.Contains(t, kinds, "status:approved")
	require.Contains(t, kinds, "status:provisioning")
	require.Contains(t, kinds, "assignedLine:+15555550199")
	require.Contains(t, kinds, "status:done")

	var status string
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT status FROM user_hermes_gateways WHERE tenant_id = $1 AND gateway_id = 'photon'`, tenant).Scan(&status))
	require.Equal(t, "configured", status)
	act := side.last()
	require.Equal(t, "/control/activate", act.path)
	require.Equal(t, "spec-1", act.body["projectId"])
}

func TestPhotonSetupUnreachable(t *testing.T) {
	v := setup(t)
	p := newPhoton(v.pool, testBox(t), nil, slog.Default())
	p.dashboardHost = "http://127.0.0.1:1"
	_, err := p.startSetup(context.Background(), v.user())
	require.Error(t, err)
}
