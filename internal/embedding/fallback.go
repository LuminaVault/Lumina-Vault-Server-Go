package embedding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

type namedService struct {
	kind Kind
	svc  Service
}

// fallbackChain is EmbeddingFallbackService: walk the chain on a recoverable
// failure, stop at a permanent one, and when every provider failed return the
// last error. An error that isn't an *Error also advances.
type fallbackChain struct {
	chain []namedService
}

func (f *fallbackChain) Embed(ctx context.Context, text string, tenant uuid.UUID) ([]float32, error) {
	var last error
	for _, p := range f.chain {
		vec, err := p.svc.Embed(ctx, text, tenant)
		if err == nil {
			if last != nil {
				slog.InfoContext(ctx, "embedding recovered via fallback: "+string(p.kind))
			}
			return vec, nil
		}
		last = err
		var pe *Error
		if !errors.As(err, &pe) {
			slog.WarnContext(ctx, fmt.Sprintf("embedding unexpected failure: provider=%s err=%v", p.kind, err))
			continue
		}
		if pe.IsRecoverable() {
			slog.WarnContext(ctx, fmt.Sprintf("embedding recoverable failure: provider=%s %v", p.kind, pe))
			continue
		}
		msg := fmt.Sprintf("embedding permanent failure: provider=%s %v", p.kind, pe)
		// An empty balance fails every embed until someone tops up, so it is an
		// error (the Loki error-rate alert sees it), not one more warning.
		if pe.Kind == FailurePermanent && pe.Permanent == ReasonPaymentRequired {
			slog.ErrorContext(ctx, msg)
		} else {
			slog.WarnContext(ctx, msg)
		}
		return nil, err
	}
	if last != nil {
		return nil, last
	}
	return nil, &Error{Kind: FailurePermanent, Permanent: ReasonAllProvidersFailed}
}
