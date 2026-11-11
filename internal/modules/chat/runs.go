package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RunStart is what an escalated turn asks for: HermesRunsService.start's arguments.
type RunStart struct {
	Tenant         uuid.UUID
	ConversationID uuid.UUID
	Prompt         string
	// SessionKey is what Swift's chat passes: the vault id. The bridge can't pass it
	// (see Bridge).
	SessionKey string
	// Authorization and HermesProfile are the caller's headers, for the bridge.
	Authorization string
	HermesProfile string
}

// RunRef is the part of HermesRunDTO a ChatHermesRunRefDTO carries.
type RunRef struct {
	ID        uuid.UUID
	SessionID *string
	StartedAt time.Time
}

// HermesRuns starts an agent run for an escalated chat turn. It is the seam between
// chat and HermesRunsService (wave 6C, blocker B12): Swift's chat calls the one
// in-process service. Until 6C ports it, Bridge stands in; 6C replaces it with the
// Go service's start, and chat needs no other change.
type HermesRuns interface {
	Start(ctx context.Context, req RunStart) (RunRef, error)
}

// Bridge is owner decision 1, option A: an escalated turn's run is started by Swift's
// POST /v1/hermes/runs with the caller's own JWT, so Swift's watcher follows it and
// /v1/hermes/* stays on Swift. Known differences from Swift's in-process start:
//   - the run's Hermes session key is Swift's activeHermesSessionKey
//     ("<hermesProfileID>:<slug>") rather than the vault id;
//   - the run route counts one more conversationByUser hit and checks the .chat
//     entitlement as well.
type Bridge struct {
	BaseURL *url.URL
	HTTP    *http.Client
}

// NewBridge parses base; nil when it is empty or doesn't parse, which leaves chat
// without a runs service, as Swift without a SecretBox.
func NewBridge(base string) *Bridge {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return &Bridge{BaseURL: u, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type runStartBody struct {
	Prompt         string    `json:"prompt"`
	ConversationID wire.UUID `json:"conversationID"`
}

type runDTO struct {
	ID        wire.UUID `json:"id"`
	SessionID *string   `json:"sessionID"`
	StartedAt wire.Time `json:"startedAt"`
}

// Start posts to Swift and reads the 202's run. Anything else is an error, and the
// caller answers the turn the classic way, as Swift does when start throws.
func (b *Bridge) Start(ctx context.Context, req RunStart) (RunRef, error) {
	payload, err := json.Marshal(runStartBody{Prompt: req.Prompt, ConversationID: wire.UUID{UUID: req.ConversationID}})
	if err != nil {
		return RunRef{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL.String()+"/v1/hermes/runs", bytes.NewReader(payload))
	if err != nil {
		return RunRef{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", req.Authorization)
	if req.HermesProfile != "" {
		httpReq.Header.Set("X-Hermes-Profile", req.HermesProfile)
	}
	// The vault the chat turn ran in; Swift's run route ignores it.
	httpReq.Header.Set("X-LV-Chat-Session-Key", req.SessionKey)
	resp, err := b.HTTP.Do(httpReq)
	if err != nil {
		return RunRef{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return RunRef{}, err
	}
	if resp.StatusCode != http.StatusAccepted {
		return RunRef{}, fmt.Errorf("hermes runs bridge: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var run runDTO
	if err := json.Unmarshal(body, &run); err != nil {
		return RunRef{}, fmt.Errorf("hermes runs bridge: %w", err)
	}
	return RunRef{ID: run.ID.UUID, SessionID: run.SessionID, StartedAt: run.StartedAt.Time}, nil
}
