package authservice_test

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/ids"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHermesProvisioningLifecycle ports HermesProvisioningLifecycleTests
// (Tests/AppTests/Auth/HermesProvisioningLifecycleTests.swift): HER-29's
// signup soft-fail with a degraded Hermes gateway.
func TestHermesProvisioningLifecycle(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	t.Run("register succeeds with a failing hermes gateway and records an error row", func(t *testing.T) {
		h := build(t, pool, harness{gateway: failingGateway{}})
		slug := "lif" + strings.ToLower(uuid.NewString()[:6])
		email := slug + "@test.luminavault"

		resp, err := h.svc.Register(ctx, email, slug, "passwordpassword12!")
		require.NoError(t, err)
		// Signup still issues tokens despite the gateway being down.
		assert.NotEmpty(t, resp.AccessToken)
		assert.NotEmpty(t, resp.RefreshToken)

		// The user row is committed (no rollback).
		u := h.user(t, "email = $1", email)
		require.NotNil(t, u)

		// The profile is left in `error` for the reconciler to heal.
		var (
			status, profileID string
			lastError         *string
		)
		require.NoError(t, pool.QueryRow(ctx, `SELECT status, last_error, hermes_profile_id FROM hermes_profiles WHERE tenant_id = $1`, u.id).
			Scan(&status, &lastError, &profileID))
		assert.Equal(t, "error", status)
		assert.NotNil(t, lastError)
		assert.True(t, strings.HasPrefix(profileID, "pending-"), profileID)

		// The access token carries no hpid.
		claims, err := auth.Verify(keys, "Bearer "+resp.AccessToken)
		require.NoError(t, err)
		assert.Empty(t, claims.HPID)
		assert.Equal(t, ids.Upper(u.id), claims.Subject)
	})
}
