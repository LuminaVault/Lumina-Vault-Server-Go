package authservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"lumina/internal/wire"
)

// Sender is EmailOTPSender: one code to one address. purpose is "login",
// "reset" or "verify" here (magic links add their own in 8B).
type Sender interface {
	Send(ctx context.Context, code, to, purpose string) error
}

// SenderConfig is what App+build.swift's makeEmailSender passes to
// makeEmailOTPSender: email.kind, email.resend.apiKey, email.fromAddress,
// email.replyTo.
type SenderConfig struct {
	Kind, APIKey, FromAddress, ReplyTo string
}

// ResendEndpoint is the URL ResendEmailOTPSender hard-codes.
const ResendEndpoint = "https://api.resend.com/emails"

// resendTimeout is URLSession.shared's default request timeout.
const resendTimeout = 60 * time.Second

// NewSender is makeEmailOTPSender: "resend" (any case) sends through Resend;
// "" and "logging" log the code; anything else logs a warning and logs the code.
func NewSender(cfg SenderConfig, log *slog.Logger) Sender {
	if log == nil {
		log = slog.Default()
	}
	switch strings.ToLower(cfg.Kind) {
	case "resend":
		return &ResendSender{
			APIKey: cfg.APIKey, FromAddress: cfg.FromAddress, ReplyTo: cfg.ReplyTo,
			Endpoint: ResendEndpoint, Client: &http.Client{Timeout: resendTimeout}, Log: log,
		}
	case "", "logging":
	default:
		log.Warn(fmt.Sprintf("unknown email.kind=%s; falling back to LoggingEmailOTPSender", cfg.Kind))
	}
	return LoggingSender{Log: log}
}

// LoggingSender is LoggingEmailOTPSender, the default. Swift weakness, kept:
// the code is written to the log at warning level, so with EMAIL_KIND unset in
// a deployed environment anyone who reads the logs can complete any OTP flow.
type LoggingSender struct{ Log *slog.Logger }

func (s LoggingSender) Send(_ context.Context, code, to, purpose string) error {
	s.Log.Warn(fmt.Sprintf("email OTP (dev only): purpose=%s to=%s code=%s", purpose, to, code))
	return nil
}

// ResendSender is ResendEmailOTPSender: POST {Endpoint} with a Bearer key and
// a JSON body holding the code in both text and html.
type ResendSender struct {
	APIKey, FromAddress, ReplyTo string
	Endpoint                     string // ResendEndpoint; tests point it at a stub
	Client                       *http.Client
	Log                          *slog.Logger
}

var errResendNotConfigured = wire.NewError(http.StatusInternalServerError, "resend not configured")

func (s *ResendSender) Send(ctx context.Context, code, to, purpose string) error {
	if s.APIKey == "" || s.FromAddress == "" {
		return errResendNotConfigured
	}
	payload := map[string]any{
		"from":    s.FromAddress,
		"to":      []string{to},
		"subject": "Your LuminaVault verification code",
		"text":    "Your LuminaVault code is " + code + ". It expires in 15 minutes.",
		"html":    resendHTML(code),
	}
	if s.ReplyTo != "" {
		payload["reply_to"] = s.ReplyTo
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// Hummingbird does not cancel a handler when the client goes away, so the
	// email still goes out after the row that holds its code was written.
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: resendTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err // a URLSession error is not an HTTPError: a 500 with no body in Swift too
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		s.log().Error(fmt.Sprintf("resend %d: %s", resp.StatusCode, preview))
		return wire.NewError(http.StatusBadGateway, fmt.Sprintf("resend email failed (%d)", resp.StatusCode))
	}
	s.log().Info(fmt.Sprintf("email OTP delivered via resend: purpose=%s to=%s", purpose, to))
	return nil
}

func (s *ResendSender) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// resendHTML is ResendEmailOTPSender.htmlBody, a Swift multi-line literal.
func resendHTML(code string) string {
	return "<p>Your LuminaVault verification code:</p>\n" +
		"<p style=\"font-size:24px;font-weight:600;letter-spacing:4px;\">" + code + "</p>\n" +
		"<p>This code expires in 15 minutes. If you didn't request it, ignore this email.</p>"
}
