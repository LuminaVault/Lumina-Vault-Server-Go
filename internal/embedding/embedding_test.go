package embedding

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDeterministicMatchesSwift(t *testing.T) {
	v, err := Deterministic{}.Embed(context.Background(), "AB", uuid.Nil)
	require.NoError(t, err)
	require.Len(t, v, Dimensions)
	require.Equal(t, float32(65)/127.5-1, v[0])
	require.Equal(t, float32(66)/127.5-1, v[1])
	require.Equal(t, v[0], v[2])

	empty, err := Deterministic{}.Embed(context.Background(), "", uuid.Nil)
	require.NoError(t, err)
	require.Equal(t, make([]float32, Dimensions), empty)
}

func TestFormatVectorMatchesSwiftFloatDescription(t *testing.T) {
	require.Equal(t, "[0.0,-1.0,0.5,0.003921628,1e-05]", FormatVector([]float32{0, -1, 0.5, 0.003921628, 1e-05}))
}

func TestEstimateTokensCountsCharactersLikeSwift(t *testing.T) {
	require.Equal(t, int64(1), EstimateTokens(""))
	require.Equal(t, int64(1), EstimateTokens("AB"))
	require.Equal(t, int64(3), EstimateTokens("twelve chars"))
	// Swift's text.count: e + U+0301 is one Character, a ZWJ family is one.
	require.Equal(t, int64(1), EstimateTokens("e\u0301"))
	require.Equal(t, int64(2), EstimateTokens(strings.Repeat("\U0001F468\u200D\U0001F469\u200D\U0001F467", 5)))
}

// tracing answers each text with one float holding its index, slowly enough to
// overlap, and records how many ran at once.
type tracing struct {
	inFlight, peak atomic.Int64
	failOn         string
	cancelled      atomic.Bool
}

func (s *tracing) Embed(ctx context.Context, text string, _ uuid.UUID) ([]float32, error) {
	n := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if text == s.failOn {
		return nil, &Error{Kind: FailureTransient, Reason: "boom"}
	}
	select {
	case <-time.After(5 * time.Millisecond):
	case <-ctx.Done():
		s.cancelled.Store(true)
		return nil, ctx.Err()
	}
	return []float32{float32(len(text))}, nil
}

func TestEmbedBatchKeepsOrderAndBoundsConcurrency(t *testing.T) {
	texts := make([]string, 20)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	s := &tracing{}
	got, err := EmbedBatch(context.Background(), s, texts, uuid.New())
	require.NoError(t, err)
	require.Len(t, got, len(texts))
	for i := range texts {
		require.Equal(t, []float32{float32(i + 1)}, got[i])
	}
	require.LessOrEqual(t, s.peak.Load(), int64(4))
	require.Greater(t, s.peak.Load(), int64(1))
}

func TestEmbedBatchEmptyAndFailure(t *testing.T) {
	got, err := EmbedBatch(context.Background(), failing{context.Canceled}, nil, uuid.New())
	require.NoError(t, err)
	require.Empty(t, got)

	_, err = EmbedBatch(context.Background(), &tracing{failOn: "bad"}, []string{"a", "bad", "c", "d", "e", "f"}, uuid.New())
	require.Equal(t, "boom", asError(t, err).Reason, "one failure fails the batch with that failure")
}

func TestEmbedBatchParentCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := EmbedBatch(ctx, &tracing{}, []string{"a", "b"}, uuid.New())
	require.ErrorIs(t, err, context.Canceled)
}
