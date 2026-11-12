package authservice_test

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
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

// recordingSMS is the Swift suite's RecordingSMSSender.
type recordingSMS struct {
	mu      sync.Mutex
	history []sentSMS
}

type sentSMS struct{ code, to, purpose string }

func (r *recordingSMS) Send(_ context.Context, code, phone, purpose string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history = append(r.history, sentSMS{code, phone, purpose})
	return nil
}

func (r *recordingSMS) last() *sentSMS {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.history) == 0 {
		return nil
	}
	return &r.history[len(r.history)-1]
}

type twilioRequest struct {
	method, path, auth, contentType, body string
}

// twilioStub stands in for api.twilio.com: it records the request and answers
// with status and body.
func twilioStub(t *testing.T, status int, body string) (*httptest.Server, *twilioRequest) {
	t.Helper()
	got := &twilioRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = twilioRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// TestSMSSender ports SMSSenderTests
// (Tests/AppTests/Auth/SMS/SMSSenderTests.swift), all 5 cases, with an
// httptest server where Swift's URLProtocol stub stood: nothing reaches Twilio.
func TestSMSSender(t *testing.T) {
	ctx := context.Background()

	t.Run("logging sender does not throw", func(t *testing.T) {
		require.NoError(t, authservice.LoggingSMSSender{Log: slog.Default()}.Send(ctx, "123456", "+15555550100", "login"))
	})

	t.Run("recording sender captures every call", func(t *testing.T) {
		r := &recordingSMS{}
		require.NoError(t, r.Send(ctx, "111111", "+15555550101", "login"))
		require.NoError(t, r.Send(ctx, "222222", "+15555550102", "verify"))
		assert.Equal(t, []sentSMS{{"111111", "+15555550101", "login"}, {"222222", "+15555550102", "verify"}}, r.history)
		assert.Equal(t, "222222", r.last().code)
	})

	t.Run("twilio sender posts correct request", func(t *testing.T) {
		srv, got := twilioStub(t, http.StatusCreated, "")
		s := &authservice.TwilioSMSSender{AccountSID: "ACfake", AuthToken: "secret", FromNumber: "+15555550000", BaseURL: srv.URL, Client: srv.Client()}
		require.NoError(t, s.Send(ctx, "424242", "+15555550199", "login"))

		assert.Equal(t, http.MethodPost, got.method)
		assert.Equal(t, "/2010-04-01/Accounts/ACfake/Messages.json", got.path)
		assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("ACfake:secret")), got.auth)
		assert.Equal(t, "application/x-www-form-urlencoded", got.contentType)
		pairs := strings.Split(got.body, "&")
		assert.Contains(t, pairs, "To=%2B15555550199")
		assert.Contains(t, pairs, "From=%2B15555550000")
		assert.Contains(t, pairs, "Body=Your%20LuminaVault%20code%20is%20424242.%20Expires%20in%205%20min.")
	})

	t.Run("twilio sender throws when unconfigured", func(t *testing.T) {
		srv, got := twilioStub(t, http.StatusCreated, "")
		s := &authservice.TwilioSMSSender{BaseURL: srv.URL, Client: srv.Client()}
		err := s.Send(ctx, "1", "+15555550000", "login")
		assert.Equal(t, wire.NewError(http.StatusInternalServerError, "twilio not configured"), err)
		assert.Empty(t, got.method, "nothing is sent")
	})

	t.Run("twilio sender throws on non 2 xx", func(t *testing.T) {
		srv, _ := twilioStub(t, http.StatusUnauthorized, `{"message":"bad creds"}`)
		s := &authservice.TwilioSMSSender{AccountSID: "ACfake", AuthToken: "wrong", FromNumber: "+15555550000", BaseURL: srv.URL, Client: srv.Client()}
		err := s.Send(ctx, "1", "+15555550199", "login")
		assert.Equal(t, wire.NewError(http.StatusBadGateway, "twilio sms failed (401)"), err)
	})
}

// TestNewSMSSender is makeSMSSender's switch.
func TestNewSMSSender(t *testing.T) {
	_, ok := authservice.NewSMSSender(authservice.SMSConfig{Kind: "TWILIO"}, nil).(*authservice.TwilioSMSSender)
	assert.True(t, ok)
	for _, kind := range []string{"logging", "", "carrier-pigeon"} {
		_, ok := authservice.NewSMSSender(authservice.SMSConfig{Kind: kind}, nil).(authservice.LoggingSMSSender)
		assert.True(t, ok, kind)
	}
}
