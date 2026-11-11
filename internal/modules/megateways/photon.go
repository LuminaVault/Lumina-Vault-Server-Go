package megateways

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/secretbox"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// photonEvent is HermesPhotonSetupEvent, coded as {type, payload}.
type photonEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

type deviceCodePayload struct {
	VerificationURI string `json:"verificationUri"`
	UserCode        string `json:"userCode"`
	ExpiresIn       int    `json:"expiresIn"`
}

func photonStatus(s string) photonEvent { return photonEvent{Type: "status", Payload: s} }

// PhotonProvisioningError cases.
var (
	errPhotonSessionNotFound = errors.New("sessionNotFound")
	e164                     = regexp.MustCompile(`^\+[1-9]\d{6,14}$`)
	nonPhone                 = regexp.MustCompile(`[^\d+]`)
)

// invalidPhoneError is PhotonProvisioningError.invalidPhone: unmapped, so a 500.
type invalidPhoneError struct{ phone string }

func (e invalidPhoneError) Error() string { return fmt.Sprintf("invalidPhone(%q)", e.phone) }

// photonError is the NSError(domain: "photon") the Swift service throws.
type photonError struct {
	code int
	msg  string
}

func (e *photonError) Error() string {
	return fmt.Sprintf("Error Domain=photon Code=%d %q", e.code, e.msg)
}

type deviceCode struct {
	deviceCode, userCode, verificationURI string
	verificationURIComplete               *string
	expiresIn, interval                   int
}

type photonSession struct {
	code           *deviceCode
	dashboardToken *string
	phone          *string
	status         string
	lastLine       *string
	subs           map[int]chan photonEvent
	cancelPoll     context.CancelFunc
	provisioning   bool
	completed      bool
}

// photon is PhotonProvisioningService: Photon's device-code login, then a project
// with Spectrum, then a shared line for the bound phone, sealed into the photon
// gateway row, then activates the project in the central sidecar when one is
// configured. Sessions are in-memory.
type photon struct {
	pool          *pgxpool.Pool
	box           *secretbox.Box
	sidecar       SidecarClient
	http          *http.Client
	log           *slog.Logger
	dashboardHost string
	spectrumHost  string

	mu       sync.Mutex
	sessions map[uuid.UUID]*photonSession
	byTenant map[uuid.UUID]uuid.UUID
	next     int
}

const (
	photonClientID    = "photon-cli"
	photonScope       = "openid profile email"
	photonProjectName = "Lumina Agent"
)

func newPhoton(pool *pgxpool.Pool, box *secretbox.Box, sidecar SidecarClient, log *slog.Logger) *photon {
	return &photon{
		pool: pool, box: box, sidecar: sidecar, http: &http.Client{}, log: log,
		dashboardHost: "https://app.photon.codes", spectrumHost: "https://spectrum.photon.codes",
		sessions: map[uuid.UUID]*photonSession{}, byTenant: map[uuid.UUID]uuid.UUID{},
	}
}

func (p *photon) startSetup(ctx context.Context, tenant uuid.UUID) (uuid.UUID, error) {
	p.mu.Lock()
	old, had := p.byTenant[tenant]
	p.mu.Unlock()
	if had {
		p.teardown(old)
	}
	id := uuid.New()
	p.mu.Lock()
	p.sessions[id] = &photonSession{status: "starting", subs: map[int]chan photonEvent{}}
	p.byTenant[tenant] = id
	p.mu.Unlock()
	code, err := p.requestDeviceCode(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	p.mu.Lock()
	if s, ok := p.sessions[id]; ok {
		s.code, s.status = code, "awaitingApproval"
	}
	p.mu.Unlock()
	uri := code.verificationURI
	if code.verificationURIComplete != nil {
		uri = *code.verificationURIComplete
	}
	p.emit(id, photonEvent{Type: "deviceCode", Payload: deviceCodePayload{uri, code.userCode, code.expiresIn}})
	p.emit(id, photonStatus("awaitingApproval"))
	pctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	s, ok := p.sessions[id]
	if ok {
		s.cancelPoll = cancel
	}
	p.mu.Unlock()
	if !ok {
		cancel()
		return id, nil
	}
	go func() { defer cancel(); p.poll(pctx, id, code) }()
	p.log.InfoContext(ctx, "photon setup started", "tenantID", tenant, "sessionID", id)
	return id, nil
}

func (p *photon) submitPhone(id uuid.UUID, phone string) error {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if !ok || s.completed {
		p.mu.Unlock()
		return errPhotonSessionNotFound
	}
	normalized := nonPhone.ReplaceAllString(phone, "")
	if !e164.MatchString(normalized) {
		p.mu.Unlock()
		return invalidPhoneError{phone}
	}
	s.phone = &normalized
	ready := s.dashboardToken != nil
	p.mu.Unlock()
	if ready {
		p.startProvisioningIfReady(id)
	}
	return nil
}

func (p *photon) subscribe(id uuid.UUID) (<-chan photonEvent, func()) {
	out := make(chan photonEvent, 64)
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok {
		out <- photonEvent{Type: "error", Payload: errPhotonSessionNotFound.Error()}
		close(out)
		return out, func() {}
	}
	n := p.next
	p.next++
	s.subs[n] = out
	if s.code != nil && (s.status == "awaitingApproval" || s.status == "starting") {
		uri := s.code.verificationURI
		if s.code.verificationURIComplete != nil {
			uri = *s.code.verificationURIComplete
		}
		out <- photonEvent{Type: "deviceCode", Payload: deviceCodePayload{uri, s.code.userCode, s.code.expiresIn}}
	}
	out <- photonStatus(s.status)
	if s.lastLine != nil {
		out <- photonEvent{Type: "assignedLine", Payload: *s.lastLine}
	}
	return out, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if s, ok := p.sessions[id]; ok {
			if c, ok := s.subs[n]; ok {
				delete(s.subs, n)
				close(c)
			}
		}
	}
}

func (p *photon) teardown(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok {
		return
	}
	if s.cancelPoll != nil {
		s.cancelPoll()
	}
	for n, c := range s.subs {
		close(c)
		delete(s.subs, n)
	}
	for t, sid := range p.byTenant {
		if sid == id {
			delete(p.byTenant, t)
		}
	}
	delete(p.sessions, id)
}

func (p *photon) emit(id uuid.UUID, ev photonEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok {
		return
	}
	for _, c := range s.subs {
		select {
		case c <- ev:
		default:
		}
	}
	switch ev.Type {
	case "status":
		s.status, _ = ev.Payload.(string)
		if s.status == "done" {
			s.completed = true
		}
	case "assignedLine":
		line, _ := ev.Payload.(string)
		s.lastLine = &line
	case "error":
		s.completed, s.status = true, "failed"
	}
}

func (p *photon) poll(ctx context.Context, id uuid.UUID, code *deviceCode) {
	deadline := time.Now().Add(time.Duration(code.expiresIn) * time.Second)
	sleep := float64(code.interval)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		s, ok := p.sessions[id]
		done := ok && s.completed
		p.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(sleep * float64(time.Second))):
		}
		token, err := p.pollForDeviceToken(ctx, code)
		if err == nil && token != nil {
			token, err = p.validateDashboardToken(ctx, *token)
		}
		if err != nil {
			p.log.DebugContext(ctx, "photon device poll transient error", "error", err)
		} else if token != nil {
			p.mu.Lock()
			s, ok := p.sessions[id]
			if !ok || s.completed {
				p.mu.Unlock()
				return
			}
			s.dashboardToken, s.status = token, "approved"
			hasPhone := s.phone != nil
			p.mu.Unlock()
			p.emit(id, photonStatus("approved"))
			if hasPhone {
				p.startProvisioningIfReady(id)
			}
			return
		}
		sleep = math.Min(sleep*1.5, 15)
	}
	p.emit(id, photonEvent{Type: "error", Payload: "Photon device approval timed out"})
	p.teardown(id)
}

func (p *photon) startProvisioningIfReady(id uuid.UUID) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if !ok || s.dashboardToken == nil || s.phone == nil || s.completed || s.provisioning {
		p.mu.Unlock()
		return
	}
	s.provisioning = true
	token, phone := *s.dashboardToken, *s.phone
	p.mu.Unlock()
	go p.provision(context.Background(), id, token, phone)
}

func (p *photon) provision(ctx context.Context, id uuid.UUID, token, phone string) {
	p.emit(id, photonStatus("provisioning"))
	err := func() error {
		projectID, err := p.findOrCreateProject(ctx, token)
		if err != nil {
			return err
		}
		proj, err := p.ensureSpectrumEnabled(ctx, token, projectID)
		if err != nil {
			return err
		}
		spectrumID, ok := proj["spectrumProjectId"].(string)
		if !ok {
			return &photonError{1, "no spectrumProjectId"}
		}
		secret, err := p.regenerateSecret(ctx, token, projectID)
		if err != nil {
			return err
		}
		user, err := p.registerUserIfAbsent(ctx, spectrumID, secret, phone)
		if err != nil {
			return err
		}
		var line *string
		if v, ok := user["assignedPhoneNumber"].(string); ok {
			line = &v
		}
		if err := p.persist(ctx, id, spectrumID, secret, projectID, phone, line); err != nil {
			return err
		}
		if line != nil {
			p.emit(id, photonEvent{Type: "assignedLine", Payload: *line})
		}
		p.emit(id, photonStatus("done"))
		p.emit(id, photonStatus("done"))
		p.log.InfoContext(ctx, "photon provisioning complete", "sessionID", id, "spectrumProjectId", spectrumID)
		time.Sleep(2 * time.Second)
		p.teardown(id)
		return nil
	}()
	if err != nil {
		p.log.ErrorContext(ctx, "photon provisioning failed", "sessionID", id, "error", err)
		p.emit(id, photonEvent{Type: "error", Payload: fmt.Sprintf("Provisioning failed: %v", err)})
		p.teardown(id)
	}
}

// do sends one Photon request and answers its status and body.
func (p *photon) do(ctx context.Context, method, url, auth string, body any) (int, http.Header, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data, err
}

func checkStatus(status int, data []byte, action string) error {
	if status >= 400 {
		text := []rune(string(data))
		if len(text) > 300 {
			text = text[:300]
		}
		return &photonError{status, fmt.Sprintf("Photon %s failed: HTTP %d: %s", action, status, string(text))}
	}
	return nil
}

func jsonObject(data []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func jsonInt(v any, def int) int {
	if f, ok := v.(float64); ok && f == math.Trunc(f) {
		return int(f)
	}
	return def
}

func (p *photon) requestDeviceCode(ctx context.Context) (*deviceCode, error) {
	status, _, data, err := p.do(ctx, http.MethodPost, p.dashboardHost+"/api/auth/device/code", "",
		map[string]any{"client_id": photonClientID, "scope": photonScope})
	if err != nil {
		return nil, err
	}
	if err := checkStatus(status, data, "device/code"); err != nil {
		return nil, err
	}
	j := jsonObject(data)
	str := func(k string) string { s, _ := j[k].(string); return s }
	c := &deviceCode{
		deviceCode: str("device_code"), userCode: str("user_code"), verificationURI: str("verification_uri"),
		expiresIn: jsonInt(j["expires_in"], 1800), interval: jsonInt(j["interval"], 5),
	}
	if v, ok := j["verification_uri_complete"].(string); ok {
		c.verificationURIComplete = &v
	}
	return c, nil
}

func (p *photon) pollForDeviceToken(ctx context.Context, code *deviceCode) (*string, error) {
	status, header, data, err := p.do(ctx, http.MethodPost, p.dashboardHost+"/api/auth/device/token", "", map[string]any{
		"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": code.deviceCode, "client_id": photonClientID,
	})
	if err != nil {
		return nil, err
	}
	j := jsonObject(data)
	switch status {
	case http.StatusOK:
		return extractTokenCandidate(j, header), nil
	case http.StatusBadRequest:
		e, _ := j["error"].(string)
		if e == "expired_token" || e == "access_denied" {
			return nil, &photonError{400, e}
		}
	}
	return nil, nil
}

func (p *photon) validateDashboardToken(ctx context.Context, token string) (*string, error) {
	status, _, _, err := p.do(ctx, http.MethodGet, p.dashboardHost+"/api/auth/get-session", "Bearer "+token, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		return &token, nil
	}
	return nil, &photonError{401, "dashboard token rejected by get-session"}
}

func unwrapList(data []byte) []map[string]any {
	var arr []map[string]any
	if json.Unmarshal(data, &arr) == nil {
		return arr
	}
	var dict map[string]json.RawMessage
	if json.Unmarshal(data, &dict) == nil {
		for _, k := range []string{"data", "projects", "users", "items"} {
			if raw, ok := dict[k]; ok && json.Unmarshal(raw, &arr) == nil && arr != nil {
				return arr
			}
		}
	}
	return nil
}

func (p *photon) findOrCreateProject(ctx context.Context, token string) (string, error) {
	status, _, data, err := p.do(ctx, http.MethodGet, p.dashboardHost+"/api/projects", "Bearer "+token, nil)
	if err != nil {
		return "", err
	}
	if err := checkStatus(status, data, "list projects"); err != nil {
		return "", err
	}
	for _, pr := range unwrapList(data) {
		if name, _ := pr["name"].(string); strings.EqualFold(name, photonProjectName) {
			if id, ok := pr["id"].(string); ok {
				return id, nil
			}
		}
	}
	status, _, data, err = p.do(ctx, http.MethodPost, p.dashboardHost+"/api/projects", "Bearer "+token, map[string]any{
		"name": photonProjectName, "location": "United States", "spectrum": true, "template": false, "observability": false,
	})
	if err != nil {
		return "", err
	}
	if err := checkStatus(status, data, "create project"); err != nil {
		return "", err
	}
	if id, ok := jsonObject(data)["id"].(string); ok {
		return id, nil
	}
	return "", &photonError{0, "no project id returned"}
}

func (p *photon) getProject(ctx context.Context, token, id string) (map[string]any, error) {
	status, _, data, err := p.do(ctx, http.MethodGet, p.dashboardHost+"/api/projects/"+id, "Bearer "+token, nil)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(status, data, "get project"); err != nil {
		return nil, err
	}
	return jsonObject(data), nil
}

func (p *photon) ensureSpectrumEnabled(ctx context.Context, token, id string) (map[string]any, error) {
	proj, err := p.getProject(ctx, token, id)
	if err != nil {
		return nil, err
	}
	if on, _ := proj["spectrum"].(bool); on {
		return proj, nil
	}
	status, _, _, err := p.do(ctx, http.MethodPost, p.dashboardHost+"/api/projects/"+id+"/spectrum/toggle", "Bearer "+token, nil)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(status, nil, "spectrum toggle"); err != nil {
		return nil, err
	}
	return p.getProject(ctx, token, id)
}

func (p *photon) regenerateSecret(ctx context.Context, token, id string) (string, error) {
	status, _, data, err := p.do(ctx, http.MethodPost, p.dashboardHost+"/api/projects/"+id+"/regenerate-secret", "Bearer "+token, nil)
	if err != nil {
		return "", err
	}
	if err := checkStatus(status, data, "regenerate secret"); err != nil {
		return "", err
	}
	if s, ok := jsonObject(data)["projectSecret"].(string); ok {
		return s, nil
	}
	return "", &photonError{0, "no projectSecret"}
}

func (p *photon) registerUserIfAbsent(ctx context.Context, projectID, secret, phone string) (map[string]any, error) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(projectID+":"+secret))
	url := p.spectrumHost + "/projects/" + projectID + "/users/"
	status, _, data, err := p.do(ctx, http.MethodGet, url, basic, nil)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(status, data, "list spectrum users"); err != nil {
		return nil, err
	}
	target := nonPhone.ReplaceAllString(phone, "")
	for _, u := range unwrapList(data) {
		if n, _ := u["phoneNumber"].(string); nonPhone.ReplaceAllString(n, "") == target {
			return u, nil
		}
	}
	status, _, data, err = p.do(ctx, http.MethodPost, url, basic, map[string]any{"type": "shared", "phoneNumber": phone})
	if err != nil {
		return nil, err
	}
	if err := checkStatus(status, data, "create spectrum user"); err != nil {
		return nil, err
	}
	j := jsonObject(data)
	if u, ok := j["user"].(map[string]any); ok {
		return u, nil
	}
	if u, ok := j["data"].(map[string]any); ok {
		return u, nil
	}
	return j, nil
}

func (p *photon) persist(ctx context.Context, id uuid.UUID, spectrumID, secret, projectID, phone string, line *string) error {
	p.mu.Lock()
	var tenant uuid.UUID
	found := false
	for t, sid := range p.byTenant {
		if sid == id {
			tenant, found = t, true
		}
	}
	p.mu.Unlock()
	if !found {
		return errPhotonSessionNotFound
	}
	assigned := ""
	if line != nil {
		assigned = *line
	}
	// JSONEncoder without sortedKeys: a dictionary's key order is unspecified in
	// Swift; Go writes it sorted.
	plain, err := json.Marshal(map[string]string{
		"spectrum_project_id": spectrumID, "project_secret": secret, "dashboard_project_id": projectID,
		"bound_phone": phone, "assigned_line": assigned,
	})
	if err != nil {
		return err
	}
	sealed, err := p.box.Seal(string(plain), tenant)
	if err != nil {
		return err
	}
	if err := upsertGatewayRow(ctx, p.pool, tenant, "photon", sealed); err != nil {
		return err
	}
	// Activate the central sidecar with the new credentials so inbound flows.
	if p.sidecar != nil {
		if err := p.sidecar.Activate(ctx, spectrumID, secret, tenant, nil); err != nil {
			p.log.WarnContext(ctx, "photon sidecar activate after provisioning failed (will retry on apply)", "error", err.Error())
		} else {
			p.log.InfoContext(ctx, "photon sidecar auto-activated after provisioning", "tenantID", tenant)
		}
	}
	return nil
}

// extractTokenCandidate looks for the access token where Photon has put it.
func extractTokenCandidate(body map[string]any, header http.Header) *string {
	str := func(m map[string]any, k string) (string, bool) { s, ok := m[k].(string); return s, ok }
	if t, ok := str(body, "access_token"); ok {
		return cleanBearer(t)
	}
	if t, ok := str(body, "accessToken"); ok {
		return cleanBearer(t)
	}
	if s, ok := body["session"].(map[string]any); ok {
		if t, ok := str(s, "access_token"); ok {
			return cleanBearer(t)
		}
	}
	if d, ok := body["data"].(map[string]any); ok {
		if t, ok := str(d, "access_token"); ok {
			return cleanBearer(t)
		}
		if t, ok := str(d, "accessToken"); ok {
			return cleanBearer(t)
		}
	}
	if t := header.Get("Set-Auth-Token"); t != "" {
		return cleanBearer(t)
	}
	return nil
}

func cleanBearer(v string) *string {
	t := strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		t = strings.TrimSpace(t[7:])
	}
	return &t
}
