package embedding

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// MARK: EmbeddingFallbackServiceTests

type scripted struct {
	vec   []float32
	err   error
	calls int
}

func (s *scripted) Embed(context.Context, string, uuid.UUID) ([]float32, error) {
	s.calls++
	return s.vec, s.err
}

func chainOf(primary, fallback Service) *fallbackChain {
	return &fallbackChain{chain: []namedService{{KindOpenAI, primary}, {KindDeterministic, fallback}}}
}

func TestFallbackPermanentRethrowsWithoutFallback(t *testing.T) {
	primary := &scripted{err: permanent(ReasonAuthRejected)}
	fallback := &scripted{vec: make([]float32, Dimensions)}
	_, err := chainOf(primary, fallback).Embed(context.Background(), "hi", uuid.New())
	require.Equal(t, ReasonAuthRejected, asError(t, err).Permanent)
	require.Equal(t, 1, primary.calls)
	require.Zero(t, fallback.calls)
}

func TestFallbackTransientAdvances(t *testing.T) {
	primary := &scripted{err: &Error{Kind: FailureTransient, Reason: "503"}}
	fallback := &scripted{vec: make([]float32, Dimensions)}
	v, err := chainOf(primary, fallback).Embed(context.Background(), "hi", uuid.New())
	require.NoError(t, err)
	require.Len(t, v, Dimensions)
	require.Equal(t, 1, primary.calls)
	require.Equal(t, 1, fallback.calls)
}

func TestFallbackExhaustedRethrowsLast(t *testing.T) {
	primary := &scripted{err: &Error{Kind: FailureTransient, Reason: "p"}}
	fallback := &scripted{err: &Error{Kind: FailureNetwork, Reason: "f"}}
	_, err := chainOf(primary, fallback).Embed(context.Background(), "hi", uuid.New())
	require.Equal(t, "f", asError(t, err).Reason)
	require.Equal(t, 1, primary.calls)
	require.Equal(t, 1, fallback.calls)
}

// Swift's untyped catch also advances, and cap and width failures stop the chain.
func TestFallbackOtherErrors(t *testing.T) {
	boom := errors.New("boom")
	fallback := &scripted{vec: []float32{1}}
	v, err := chainOf(&scripted{err: boom}, fallback).Embed(context.Background(), "hi", uuid.New())
	require.NoError(t, err)
	require.Equal(t, []float32{1}, v)

	fallback = &scripted{vec: []float32{1}}
	_, err = chainOf(&scripted{err: &Error{Kind: FailureDimMismatch, Expected: 1536, Got: 3}}, fallback).Embed(context.Background(), "hi", uuid.New())
	require.Equal(t, FailureDimMismatch, asError(t, err).Kind)
	require.Zero(t, fallback.calls)

	_, err = (&fallbackChain{}).Embed(context.Background(), "hi", uuid.New())
	require.Equal(t, ReasonAllProvidersFailed, asError(t, err).Permanent)
}
