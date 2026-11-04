package embedding

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// MARK: EmbeddingKindAndErrorTests

func TestKindParse(t *testing.T) {
	for raw, want := range map[string]Kind{
		"openai": KindOpenAI, "OpenAI": KindOpenAI,
		"openrouter": KindOpenRouter, "OpenRouter": KindOpenRouter, "open_router": KindOpenRouter, "open-router": KindOpenRouter,
		"  nomic  ":   KindNomic,
		"hermesLocal": KindHermesLocal, "hermes_local": KindHermesLocal, "hermes-local": KindHermesLocal, "local": KindHermesLocal, "localhermes": KindHermesLocal,
		"deterministic": KindDeterministic, "stub": KindDeterministic, "dev": KindDeterministic,
	} {
		got, ok := ParseKind(raw)
		require.True(t, ok, raw)
		require.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"not-a-provider", ""} {
		_, ok := ParseKind(raw)
		require.False(t, ok, raw)
	}
}

func permanent(r PermanentReason) *Error { return &Error{Kind: FailurePermanent, Permanent: r} }

func TestRecoverabilityMatrix(t *testing.T) {
	require.True(t, (&Error{Kind: FailureTransient}).IsRecoverable())
	require.True(t, (&Error{Kind: FailureNetwork}).IsRecoverable())
	require.False(t, permanent(ReasonAuthRejected).IsRecoverable())
	require.False(t, permanent(ReasonEndpointMissing).IsRecoverable())
	require.False(t, permanent(ReasonDecodeFailed).IsRecoverable())
	require.False(t, (&Error{Kind: FailureCapExceeded}).IsRecoverable())
	require.False(t, (&Error{Kind: FailureDimMismatch, Expected: 1536, Got: 768}).IsRecoverable())
}

func TestProviderUnavailableMatrix(t *testing.T) {
	require.True(t, (&Error{Kind: FailureTransient}).IsProviderUnavailable())
	require.True(t, (&Error{Kind: FailureNetwork}).IsProviderUnavailable())
	require.True(t, permanent(ReasonPaymentRequired).IsProviderUnavailable())
	require.True(t, permanent(ReasonAuthRejected).IsProviderUnavailable())
	require.True(t, permanent(ReasonMissingAPIKey).IsProviderUnavailable())
	require.False(t, permanent(ReasonRequestRejected).IsProviderUnavailable())
	require.False(t, permanent(ReasonDecodeFailed).IsProviderUnavailable())
	require.False(t, (&Error{Kind: FailureCapExceeded}).IsProviderUnavailable())
	require.False(t, (&Error{Kind: FailureDimMismatch, Expected: 1536, Got: 768}).IsProviderUnavailable())
}

func TestProviderMessageShapes(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"message":"Insufficient credits"}}`: "Insufficient credits",
		`{"error":"bad input"}`:                        "bad input",
		`{"message":"top level"}`:                      "top level",
		"  Bad\n gateway  ":                            "Bad gateway",
		"":                                             "",
		`{"detail":"x"}`:                               "",
		"Bearer abc.def rejected":                      "[redacted] [redacted] rejected",
		`[1,2]`:                                        "[1,2]",
		`null`:                                         "null",
		"\xff\xfe":                                     "",
	} {
		require.Equal(t, want, newProviderResponse(400, []byte(body)).Message, body)
	}
	require.Equal(t, "http=500", (&ProviderResponse{Status: 500}).String())
}

type failing struct{ err error }

func (f failing) Embed(context.Context, string, uuid.UUID) ([]float32, error) { return nil, f.err }

func TestWriteAndQueryDegradeToNil(t *testing.T) {
	ctx := context.Background()
	down := failing{permanent(ReasonPaymentRequired)}
	v, err := EmbedForWrite(ctx, down, "note", uuid.New(), "test")
	require.NoError(t, err)
	require.Nil(t, v)
	v, err = EmbedForQuery(ctx, down, "query", uuid.New(), "test")
	require.NoError(t, err)
	require.Nil(t, v)
	batch, err := EmbedBatchForWrite(ctx, down, []string{"a", "b", "c"}, uuid.New(), "test")
	require.NoError(t, err)
	require.Len(t, batch, 3)
	for _, vec := range batch {
		require.Nil(t, vec)
	}

	v, err = EmbedForWrite(ctx, Deterministic{}, "note", uuid.New(), "test")
	require.NoError(t, err)
	require.Len(t, v, Dimensions)
}

func TestCancelledEmbedIsNotADegradedSave(t *testing.T) {
	ctx := context.Background()
	_, err := EmbedForWrite(ctx, failing{context.Canceled}, "note", uuid.New(), "test")
	require.ErrorIs(t, err, context.Canceled)
	_, err = EmbedForQuery(ctx, failing{context.Canceled}, "q", uuid.New(), "test")
	require.ErrorIs(t, err, context.Canceled)
	_, err = EmbedBatchForWrite(ctx, failing{context.Canceled}, []string{"a"}, uuid.New(), "test")
	require.ErrorIs(t, err, context.Canceled)

	// URLSession reports a cancelled request as a network error; so does Go.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = EmbedForWrite(cancelled, failing{&Error{Kind: FailureNetwork, Reason: "x"}}, "note", uuid.New(), "test")
	require.ErrorIs(t, err, context.Canceled)
}

// MARK: the seam of Memory/EmbeddingDegradationTests. Its seven tests drive routes,
// repositories and the heal pass that don't exist here yet; this pins the same
// switchable provider against the helpers those routes call.

type switchable struct {
	mode string
}

func (s *switchable) Embed(_ context.Context, text string, _ uuid.UUID) ([]float32, error) {
	switch s.mode {
	case "paymentRequired":
		return nil, &Error{Kind: FailurePermanent, Permanent: ReasonPaymentRequired, Response: &ProviderResponse{Status: 402, Message: "Insufficient credits"}}
	case "outage":
		return nil, &Error{Kind: FailureTransient, Reason: "stub http 503", Response: &ProviderResponse{Status: 503, Message: "upstream unavailable"}}
	}
	if strings.Contains(text, "REJECT") {
		return nil, &Error{Kind: FailurePermanent, Permanent: ReasonRequestRejected, Response: &ProviderResponse{Status: 400, Message: "input rejected"}}
	}
	v := make([]float32, Dimensions)
	v[1] = 1
	return v, nil
}

func TestDegradationAcrossProviderStates(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		mode, text string
		wantVector bool
	}{
		{"paymentRequired", "Call the plumber on Thursday", false},
		{"outage", "Dentist moved to 10", false},
		{"working", "REJECT this one", false},
		{"working", "heal this one", true},
	} {
		s := &switchable{tc.mode}
		v, err := EmbedForWrite(ctx, s, tc.text, uuid.New(), "memory.upsert")
		require.NoError(t, err, tc.mode)
		require.Equal(t, tc.wantVector, v != nil, tc.mode)
		v, err = EmbedForQuery(ctx, s, tc.text, uuid.New(), "memory.search")
		require.NoError(t, err, tc.mode)
		require.Equal(t, tc.wantVector, v != nil, tc.mode)
	}
	vecs, err := EmbedBatchForWrite(ctx, &switchable{"outage"}, []string{"# Trip", "The ferry"}, uuid.New(), "chunks.index")
	require.NoError(t, err)
	require.Equal(t, [][]float32{nil, nil}, vecs)
}

func TestErrorDescriptions(t *testing.T) {
	require.Equal(t, `reason=paymentRequired http=402 msg="Insufficient credits"`,
		errorFromHTTP(402, []byte(`{"error":{"message":"Insufficient credits"}}`), "openrouter").Error())
	require.Equal(t, `transient reason="openrouter http 503" http=503 msg="down"`, errorFromHTTP(503, []byte("down"), "openrouter").Error())
	require.Nil(t, errorFromHTTP(204, nil, "openrouter"))
	require.Equal(t, ReasonRequestRejected, errorFromHTTP(404, nil, "x").Permanent)
	require.Equal(t, FailureTransient, errorFromHTTP(408, nil, "x").Kind)
}
