package authservice

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/wire"
	"net/http"
	"strings"
)

// SMSSender is SMSSender: one code to one E.164 number.
type SMSSender interface {
	Send(ctx context.Context, code, phone, purpose string) error
}

// LoggingSMSSender is the dev fallback. Swift weakness, kept: the code goes to
// the log at warning level, so with SMS_KIND unset in a deployed environment
// anyone who reads the logs can complete a phone sign-in for any number.
type LoggingSMSSender struct{ Log *slog.Logger }

func (s LoggingSMSSender) Send(_ context.Context, code, phone, purpose string) error {
	s.log().Warn(fmt.Sprintf("sms OTP (dev only): purpose=%s to=%s code=%s", purpose, phone, code))
	return nil
}

func (s LoggingSMSSender) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// TwilioAPIBase is the host TwilioSMSSender hard-codes.
const TwilioAPIBase = "https://api.twilio.com"

// TwilioSMSSender posts to Twilio's Messages API with HTTP Basic auth.
type TwilioSMSSender struct {
	AccountSID, AuthToken, FromNumber string
	BaseURL                           string // TwilioAPIBase; tests point it at a stub
	Client                            *http.Client
	Log                               *slog.Logger
}

var errTwilioNotConfigured = wire.NewError(http.StatusInternalServerError, "twilio not configured")

func (s *TwilioSMSSender) Send(ctx context.Context, code, phone, purpose string) error {
	if s.AccountSID == "" || s.AuthToken == "" || s.FromNumber == "" {
		return errTwilioNotConfigured
	}
	base := s.BaseURL
	if base == "" {
		base = TwilioAPIBase
	}
	body := twilioForm([][2]string{
		{"To", phone},
		{"From", s.FromNumber},
		{"Body", "Your LuminaVault code is " + code + ". Expires in 5 min."},
	})
	// Not cancelled with the request: the challenge is already issued.
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodPost,
		base+"/2010-04-01/Accounts/"+s.AccountSID+"/Messages.json", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.AccountSID+":"+s.AuthToken)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		s.log().Error(fmt.Sprintf("twilio %d: %s", resp.StatusCode, preview))
		return wire.NewError(http.StatusBadGateway, fmt.Sprintf("twilio sms failed (%d)", resp.StatusCode))
	}
	s.log().Info(fmt.Sprintf("sms delivered via twilio: purpose=%s to=%s", purpose, phone))
	return nil
}

func (s *TwilioSMSSender) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// twilioForm is TwilioSMSSender.urlEncode: key=value joined by "&", everything
// but ASCII letters, digits and -._~ percent-encoded. Swift builds it from a
// Dictionary, so its pair order varies; Twilio reads it either way.
func twilioForm(pairs [][2]string) string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, percentEncode(p[0])+"="+percentEncode(p[1]))
	}
	return strings.Join(out, "&")
}

func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// SMSConfig is what makeSMSSender reads: sms.kind and twilio.*.
type SMSConfig struct {
	Kind, AccountSID, AuthToken, FromNumber string
}

// NewSMSSender is makeSMSSender: "twilio" (any case) sends through Twilio,
// "logging" logs, anything else warns and logs.
func NewSMSSender(cfg SMSConfig, log *slog.Logger) SMSSender {
	if log == nil {
		log = slog.Default()
	}
	switch strings.ToLower(cfg.Kind) {
	case "twilio":
		return &TwilioSMSSender{
			AccountSID: cfg.AccountSID, AuthToken: cfg.AuthToken, FromNumber: cfg.FromNumber,
			BaseURL: TwilioAPIBase, Client: &http.Client{Timeout: fetchTimeout}, Log: log,
		}
	case "logging":
	default:
		log.Warn(fmt.Sprintf("unknown sms.kind=%s; falling back to LoggingSMSSender", cfg.Kind))
	}
	return LoggingSMSSender{Log: log}
}
