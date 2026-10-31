package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"
)

// Sender delivers an invitation token: Swift's TeamInvitationSending
// (Sources/App/Teams/TeamInvitationSender.swift). The token is never returned
// to the inviter; it only travels in the email (or the dev log line).
type Sender interface {
	Send(ctx context.Context, to, teamName, inviterName, token string, expiresAt time.Time) error
}

// SenderConfig is what App+build.swift passes to makeTeamInvitationSender
// (email.kind, email.resend.apiKey, email.fromAddress, email.replyTo,
// team.invite.base.url).
type SenderConfig struct {
	Kind, APIKey, FromAddress, ReplyTo, BaseURL string
}

// NewSender is makeTeamInvitationSender: "resend" (any case) sends through
// Resend, anything else only logs.
func NewSender(cfg SenderConfig, log *slog.Logger) Sender {
	base := cfg.BaseURL
	if base == "" { // URL(string:) of "" is nil, so Swift falls back too
		base = "http://localhost:5173"
	}
	if strings.EqualFold(cfg.Kind, "resend") {
		return &ResendSender{
			APIKey: cfg.APIKey, FromAddress: cfg.FromAddress, ReplyTo: cfg.ReplyTo, BaseURL: base,
			Endpoint: "https://api.resend.com/emails", Client: http.DefaultClient, Log: log,
		}
	}
	return LoggingSender{BaseURL: base, Log: log}
}

// inviteURL is baseURL.appendingPathComponent("invitations").appendingPathComponent(token).
func inviteURL(base, token string) string {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + "invitations/" + token
}

// swiftDate is Date.description: "2026-11-07 09:30:00 +0000".
func swiftDate(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 +0000") }

// LoggingSender is LoggingTeamInvitationSender (email.kind=logging, the default).
type LoggingSender struct {
	BaseURL string
	Log     *slog.Logger
}

func (s LoggingSender) Send(_ context.Context, to, teamName, inviterName, token string, expiresAt time.Time) error {
	s.Log.Warn(fmt.Sprintf("team invitation (dev only): to=%s team=%s inviter=%s url=%s expires=%s",
		to, teamName, inviterName, inviteURL(s.BaseURL, token), swiftDate(expiresAt)))
	return nil
}

// ResendSender is ResendTeamInvitationSender.
type ResendSender struct {
	APIKey, FromAddress, ReplyTo, BaseURL string
	Endpoint                              string // https://api.resend.com/emails; tests point it at a stub
	Client                                *http.Client
	Log                                   *slog.Logger
}

var (
	errEmailNotConfigured = wire.NewError(http.StatusInternalServerError, "team invitation email is not configured")
	errEmailFailed        = wire.NewError(http.StatusBadGateway, "team invitation email failed")
)

func (s *ResendSender) Send(ctx context.Context, to, teamName, inviterName, token string, expiresAt time.Time) error {
	if s.APIKey == "" || s.FromAddress == "" {
		return errEmailNotConfigured
	}
	url := inviteURL(s.BaseURL, token)
	payload := map[string]any{
		"from":    s.FromAddress,
		"to":      []string{to},
		"subject": inviterName + " invited you to " + teamName + " on LuminaVault",
		"text":    inviterName + " invited you to collaborate in " + teamName + ". Accept before " + swiftDate(expiresAt) + ": " + url,
		"html": "<p><strong>" + escapeHTML(inviterName) + "</strong> invited you to collaborate in <strong>" +
			escapeHTML(teamName) + "</strong>.</p><p><a href=\"" + url + "\">Accept invitation</a></p><p>This invitation expires in 7 days.</p>",
	}
	if s.ReplyTo != "" {
		payload["reply_to"] = s.ReplyTo
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return err // URLSession errors are not HTTPErrors: a 500 in Swift too
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		s.Log.Error("team invitation email failed: " + string(preview))
		return errEmailFailed
	}
	return nil
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
