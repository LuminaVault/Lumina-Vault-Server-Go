package megateways

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/hermescontainers"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SidecarClient is PhotonSidecarClienting: the control surface of the central
// Photon sidecar (spectrum-ts), which streams tenants' iMessage events to the
// inbound webhook and sends their replies.
type SidecarClient interface {
	Activate(ctx context.Context, projectID, projectSecret string, tenant uuid.UUID, options map[string]string) error
	Deactivate(ctx context.Context, projectID string) error
	Send(ctx context.Context, projectID, spaceID string, text *string, attachments []SidecarAttachment) (SidecarSendResult, error)
	Health(ctx context.Context) SidecarHealth
}

// SidecarSendResult is PhotonSendResult.
type SidecarSendResult struct {
	OK        bool
	MessageID *string
}

// SidecarAttachment is PhotonAttachmentInput.
type SidecarAttachment struct {
	Path, Name, MimeType, Caption *string
	Kind                          string
}

// SidecarHealth is PhotonSidecarHealth: healthy with a project count (-1 when
// the body has none), or unhealthy with a reason.
type SidecarHealth struct {
	Healthy        bool
	ActiveProjects int
	Reason         string
}

// SidecarUnauthorizedError is PhotonSidecarError.unauthorized.
var ErrSidecarUnauthorized = errors.New("unauthorized")

// SidecarHTTPError is PhotonSidecarError.http.
type SidecarHTTPError struct {
	Status int
	Body   string
}

func (e *SidecarHTTPError) Error() string {
	return fmt.Sprintf("http(status: %d, body: %q)", e.Status, e.Body)
}

// HTTPSidecarClient is PhotonSidecarClient.
type HTTPSidecarClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Log     *slog.Logger
	// Timeout is the control-call timeout; Swift's default is 30 s.
	Timeout time.Duration
}

// NewSidecarClient is PhotonSidecarClient(baseURL:token:logger:).
func NewSidecarClient(baseURL, token string, log *slog.Logger) *HTTPSidecarClient {
	if log == nil {
		log = slog.Default()
	}
	return &HTTPSidecarClient{BaseURL: baseURL, Token: token, HTTP: &http.Client{}, Log: log, Timeout: 30 * time.Second}
}

var _ SidecarClient = (*HTTPSidecarClient)(nil)

// endpoint is URL.appendingPathComponent: one slash between base and path.
func (s *HTTPSidecarClient) endpoint(path string) string {
	return strings.TrimSuffix(s.BaseURL, "/") + "/" + path
}

func (s *HTTPSidecarClient) control(ctx context.Context, path, operation string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint(path), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lumina-Sidecar-Token", s.Token)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, nil
	}
	s.Log.WarnContext(ctx, "photon sidecar "+operation+" failed", "status", resp.StatusCode, "body", string(data))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrSidecarUnauthorized
	}
	return nil, &SidecarHTTPError{Status: resp.StatusCode, Body: string(data)}
}

// Activate is activate: the sidecar starts consuming the project's stream.
func (s *HTTPSidecarClient) Activate(ctx context.Context, projectID, projectSecret string, tenant uuid.UUID, options map[string]string) error {
	if options == nil {
		options = map[string]string{}
	}
	_, err := s.control(ctx, "control/activate", "activate", map[string]any{
		"projectId": projectID, "projectSecret": projectSecret,
		"tenantId": strings.ToLower(tenant.String()), "options": options,
	})
	if err == nil {
		s.Log.InfoContext(ctx, "photon sidecar activated project", "projectId", projectID, "tenantId", tenant)
	}
	return err
}

// Deactivate is deactivate.
func (s *HTTPSidecarClient) Deactivate(ctx context.Context, projectID string) error {
	_, err := s.control(ctx, "control/deactivate", "deactivate", map[string]string{"projectId": projectID})
	if err == nil {
		s.Log.InfoContext(ctx, "photon sidecar deactivated project", "projectId", projectID)
	}
	return err
}

// Send is send: a reply on a space from a prior inbound event. A body without a
// boolean `ok` counts as sent.
func (s *HTTPSidecarClient) Send(ctx context.Context, projectID, spaceID string, text *string, attachments []SidecarAttachment) (SidecarSendResult, error) {
	body := map[string]any{"projectId": projectID, "spaceId": spaceID}
	if text != nil {
		body["text"] = *text
	}
	if len(attachments) > 0 {
		list := make([]map[string]any, 0, len(attachments))
		for _, a := range attachments {
			list = append(list, map[string]any{"path": a.Path, "name": a.Name, "mimeType": a.MimeType, "caption": a.Caption, "kind": a.Kind})
		}
		body["attachments"] = list
	}
	data, err := s.control(ctx, "control/send", "send", body)
	if err != nil {
		return SidecarSendResult{}, err
	}
	var parsed map[string]any
	if json.Unmarshal(data, &parsed) == nil {
		if ok, isBool := parsed["ok"].(bool); isBool {
			res := SidecarSendResult{OK: ok}
			if mid, isStr := parsed["messageId"].(string); isStr {
				res.MessageID = &mid
			}
			return res, nil
		}
	}
	return SidecarSendResult{OK: true}, nil
}

// Health is health: GET /healthz with a 5 s timeout.
func (s *HTTPSidecarClient) Health(ctx context.Context) SidecarHealth {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint("healthz"), nil)
	if err != nil {
		return SidecarHealth{Reason: "unreachable"}
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		s.Log.DebugContext(ctx, fmt.Sprintf("photon sidecar health failed: %v", err))
		return SidecarHealth{Reason: "unreachable"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SidecarHealth{Reason: "bad_status"}
	}
	data, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if json.Unmarshal(data, &parsed) == nil {
		// JSONSerialization's `as? Int` takes a whole number only.
		if n, ok := parsed["activeProjects"].(float64); ok && n == float64(int(n)) {
			return SidecarHealth{Healthy: true, ActiveProjects: int(n)}
		}
	}
	return SidecarHealth{Healthy: true, ActiveProjects: -1}
}

// InboundEvent is PhotonInboundEvent, as the sidecar forwards it.
type InboundEvent struct {
	ProjectID string            `json:"projectId"`
	TenantID  uuid.UUID         `json:"tenantId"`
	SpaceID   string            `json:"spaceId"`
	SpaceName *string           `json:"spaceName,omitempty"`
	MessageID *string           `json:"messageId,omitempty"`
	Sender    map[string]string `json:"sender,omitempty"`
	Text      string            `json:"text"`
	Content   map[string]string `json:"content,omitempty"`
	Timestamp *int              `json:"timestamp,omitempty"`
}

// delivery is PhotonDeliveryService: an inbound message goes to the tenant's
// Hermes as a chat completion, and the reply goes back through the sidecar.
type delivery struct {
	client     SidecarClient
	containers hermescontainers.Containers
	http       *http.Client
	log        *slog.Logger
	// hostBase is where the tenant's published port is reached. Swift uses
	// host.docker.internal.
	hostBase string
}

func (d *delivery) deliver(ctx context.Context, ev InboundEvent) {
	if err := d.run(ctx, ev); err != nil {
		d.log.ErrorContext(ctx, "photon delivery failed", "tenant", ev.TenantID, "error", err.Error())
	}
}

func (d *delivery) run(ctx context.Context, ev InboundEvent) error {
	h, err := d.containers.EnsureRunning(ctx, ev.TenantID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"model": "hermes-3", "messages": []map[string]any{{"role": "user", "content": ev.Text}},
		"stream": false, "max_tokens": 2048,
	})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, fmt.Sprintf("%s:%d/v1/chat/completions", d.hostBase, h.Port), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.APIServerKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		d.log.WarnContext(ctx, "photon delivery: tenant hermes chat failed", "tenant", ev.TenantID, "status", resp.StatusCode)
		return nil
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &parsed) != nil || len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == nil ||
		strings.TrimSpace(*parsed.Choices[0].Message.Content) == "" {
		d.log.DebugContext(ctx, "photon delivery: no usable reply text from tenant hermes")
		return nil
	}
	reply := *parsed.Choices[0].Message.Content
	if _, err := d.client.Send(ctx, ev.ProjectID, ev.SpaceID, &reply, nil); err != nil {
		return err
	}
	d.log.InfoContext(ctx, "photon delivery: reply sent", "tenant", ev.TenantID, "spaceId", ev.SpaceID, "len", len([]rune(reply)))
	return nil
}
