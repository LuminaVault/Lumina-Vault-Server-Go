package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// HermesAudioTokenService has no Swift tests; these check that a minted token is
// what AudioJWTAuthenticator accepts and JWTAuthenticator refuses, and that its
// fractional iat stays strictly after an epoch written a moment earlier.
func TestAudioTokensMint(t *testing.T) {
	keys := NewKeyset(Key{Kid: "a", Secret: []byte(secretA)})
	user := uuid.New()
	epoch := time.Now()
	minted := AudioTokens{Keys: keys, Now: func() time.Time { return epoch.Add(time.Millisecond) }}
	tok, err := minted.Mint(context.Background(), user)
	require.NoError(t, err)

	code, seen := through(t, AudioIdentify(keys, fakeAudioUsers{found: true, epoch: &epoch}), tok)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, user, seen)

	code, _ = through(t, Identify(keys, fakeAudioUsers{found: true}), tok)
	require.Equal(t, http.StatusUnauthorized, code)

	later := epoch.Add(time.Second)
	code, _ = through(t, AudioIdentify(keys, fakeAudioUsers{found: true, epoch: &later}), tok)
	require.Equal(t, http.StatusUnauthorized, code, "a revoke after the mint refuses it")
}
