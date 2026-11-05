package entitlement_test

import (
	"fmt"
	"lumina/internal/entitlement"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Grok/PremiumGuardMiddlewareTests.swift, plus the tier table those two tests leave
// to the middleware's doc comment.
func TestPremiumGuard(t *testing.T) {
	g := newGateEnv(t)
	guard := entitlement.PremiumGuardMiddleware(g.pool)

	t.Run("a trial user hits 402 with the exact body", func(t *testing.T) {
		code, body := g.probe(guard, g.user("trial", "none"))
		require.Equal(t, http.StatusPaymentRequired, code)
		require.Equal(t, `{"error":"premium_required","tier":"trial","requiresXaiConnect":true}`, body)
	})
	t.Run("unauthenticated gets 401 before the guard", func(t *testing.T) {
		code, body := g.probe(guard, uuid.Nil)
		require.Equal(t, http.StatusUnauthorized, code)
		require.JSONEq(t, `{"error":{"message":"Authenticated identity is unavailable"}}`, body)
	})
	for _, tc := range []struct {
		tier, override string
		passes         bool
	}{
		{"free", "none", false},
		{"lapsed", "none", false},
		{"archived", "none", false},
		{"pro", "none", true},
		{"ultimate", "none", true},
		{"trial", "pro", true},
		{"lapsed", "ultimate", true},
		{"archived", "pro", true}, // Swift's override wins, even over archived
	} {
		t.Run(tc.tier+"/"+tc.override, func(t *testing.T) {
			code, body := g.probe(guard, g.user(tc.tier, tc.override))
			if tc.passes {
				require.Equal(t, http.StatusOK, code)
				return
			}
			require.Equal(t, http.StatusPaymentRequired, code)
			require.Equal(t, fmt.Sprintf(`{"error":"premium_required","tier":%q,"requiresXaiConnect":true}`, tc.tier), body)
		})
	}
}
