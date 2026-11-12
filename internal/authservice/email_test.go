package authservice_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"lumina/internal/authservice"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturedRequest struct {
	method, path, auth, contentType string
	body                            map[string]any
}

// resendStub is ResendStubURLProtocol: an httptest server that records the
// request and answers status with body. No real API key or host is used.
func resendStub(t *testing.T, status int, body string) (*httptest.Server, func() *capturedRequest) {
	t.Helper()
	var (
		mu  sync.Mutex
		got *capturedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := &capturedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type")}
		_ = json.Unmarshal(raw, &c.body)
		mu.Lock()
		got = c
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() *capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func sender(srv *httptest.Server, apiKey, from, replyTo string) *authservice.ResendSender {
	s := &authservice.ResendSender{APIKey: apiKey, FromAddress: from, ReplyTo: replyTo, Client: http.DefaultClient}
	if srv != nil {
		s.Endpoint = srv.URL + "/emails"
	}
	return s
}

// TestResendEmailOTPSender ports ResendEmailOTPSenderTests
// (Tests/AppTests/Auth/MFA/ResendEmailOTPSenderTests.swift), all 5 cases, on a stub server.
func TestResendEmailOTPSender(t *testing.T) {
	ctx := context.Background()

	t.Run("posts JSON body with code, from, to, subject", func(t *testing.T) {
		srv, got := resendStub(t, 200, `{"id":"em_abc"}`)
		s := sender(srv, "re_test_key", "LuminaVault <auth@lumina.app>", "support@lumina.app")
		require.NoError(t, s.Send(ctx, "424242", "user@example.com", "login"))

		req := got()
		require.NotNil(t, req)
		assert.Equal(t, "/emails", req.path)
		assert.Equal(t, http.MethodPost, req.method)
		assert.Equal(t, "Bearer re_test_key", req.auth)
		assert.Equal(t, "application/json", req.contentType)
		assert.Equal(t, "LuminaVault <auth@lumina.app>", req.body["from"])
		assert.Equal(t, "support@lumina.app", req.body["reply_to"])
		assert.Equal(t, []any{"user@example.com"}, req.body["to"])
		assert.Contains(t, req.body["subject"], "LuminaVault")
		// The code must appear in both text and html.
		assert.Contains(t, req.body["text"], "424242")
		assert.Contains(t, req.body["html"], "424242")
		// Swift's literals, verbatim.
		assert.Equal(t, "Your LuminaVault verification code", req.body["subject"])
		assert.Equal(t, "Your LuminaVault code is 424242. It expires in 15 minutes.", req.body["text"])
		assert.Equal(t, "<p>Your LuminaVault verification code:</p>\n<p style=\"font-size:24px;font-weight:600;letter-spacing:4px;\">424242</p>\n"+
			"<p>This code expires in 15 minutes. If you didn't request it, ignore this email.</p>", req.body["html"])
	})

	t.Run("omits reply_to when not configured", func(t *testing.T) {
		srv, got := resendStub(t, 200, `{"id":"em_abc"}`)
		require.NoError(t, sender(srv, "re_test_key", "auth@lumina.app", "").Send(ctx, "111111", "user@example.com", "verify"))
		_, present := got().body["reply_to"]
		assert.False(t, present)
	})

	t.Run("throws when API key empty", func(t *testing.T) {
		err := sender(nil, "", "auth@lumina.app", "").Send(ctx, "1", "user@example.com", "login")
		var we *wire.Error
		require.True(t, errors.As(err, &we))
		assert.Equal(t, 500, we.Status)
		assert.Equal(t, "resend not configured", we.Message)
	})

	t.Run("throws when from address empty", func(t *testing.T) {
		err := sender(nil, "re_test_key", "", "").Send(ctx, "1", "user@example.com", "login")
		require.Error(t, err)
	})

	t.Run("throws on non 2xx response", func(t *testing.T) {
		srv, _ := resendStub(t, 422, `{"name":"validation_error","message":"unverified sender"}`)
		err := sender(srv, "re_test_key", "auth@lumina.app", "").Send(ctx, "1", "user@example.com", "login")
		var we *wire.Error
		require.True(t, errors.As(err, &we))
		assert.Equal(t, http.StatusBadGateway, we.Status)
		assert.Equal(t, "resend email failed (422)", we.Message)
	})
}

// TestNewSender is makeEmailOTPSender's switch.
func TestNewSender(t *testing.T) {
	s, ok := authservice.NewSender(authservice.SenderConfig{Kind: "ReSend", APIKey: "k", FromAddress: "f"}, nil).(*authservice.ResendSender)
	require.True(t, ok)
	assert.Equal(t, authservice.ResendEndpoint, s.Endpoint)
	for _, kind := range []string{"", "logging", "smtp"} {
		_, ok := authservice.NewSender(authservice.SenderConfig{Kind: kind}, nil).(authservice.LoggingSender)
		assert.True(t, ok, kind)
	}
	assert.True(t, strings.HasPrefix(authservice.ResendEndpoint, "https://api.resend.com/"))
}
