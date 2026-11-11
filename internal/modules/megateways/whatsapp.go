package megateways

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/hermescontainers"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// pairEvent is HermesWhatsAppPairEvent, coded as {type, payload}.
type pairEvent struct {
	Type    string  `json:"type"`
	Payload *string `json:"payload,omitempty"`
}

func qrEvent(art string) pairEvent   { return pairEvent{Type: "qr", Payload: &art} }
func pairStatus(s string) pairEvent  { return pairEvent{Type: "status", Payload: &s} }
func pairError(msg string) pairEvent { return pairEvent{Type: "error", Payload: &msg} }

var linkedEvent = pairEvent{Type: "linked"}

var (
	linkedMarkers  = []string{"linked", "connected", "successfully paired", "pairing complete", "logged in", "device linked", "you're all set"}
	expiredMarkers = []string{"qr expired", "code expired", "expired", "timed out", "timeout"}
	errorMarkers   = []string{"error", "failed", "could not", "unable to", "disconnected"}
	blockChars     = "█▀▄▐▌░▒▓◼◻  "
)

// pairParser is WhatsAppPairParser: it turns `hermes whatsapp` output into events.
// A run of five or more QR-block lines is one frame, emitted when the run ends.
type pairParser struct{ qr []string }

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func (p *pairParser) consume(raw string) []pairEvent {
	line := stripANSI(raw)
	if isQRLine(line) {
		p.qr = append(p.qr, line)
		return nil
	}
	var events []pairEvent
	if qr, ok := p.flush(); ok {
		events = append(events, qr)
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return events
	}
	lower := strings.ToLower(trimmed)
	switch {
	case containsAny(lower, errorMarkers) && !containsAny(lower, linkedMarkers):
		events = append(events, pairError(trimmed))
	case containsAny(lower, linkedMarkers):
		events = append(events, linkedEvent)
	case containsAny(lower, expiredMarkers):
		events = append(events, pairStatus("expired"))
	}
	return events
}

func (p *pairParser) finish() []pairEvent {
	if qr, ok := p.flush(); ok {
		return []pairEvent{qr}
	}
	return nil
}

func (p *pairParser) flush() (pairEvent, bool) {
	defer func() { p.qr = p.qr[:0] }()
	if len(p.qr) < 5 {
		return pairEvent{}, false
	}
	return qrEvent(strings.Join(p.qr, "\n")), true
}

func isQRLine(line string) bool {
	nonSpace, blocks, total := 0, 0, 0
	for _, r := range line {
		total++
		if !unicode.IsSpace(r) {
			nonSpace++
		}
		if strings.ContainsRune(blockChars, r) {
			blocks++
		}
	}
	if nonSpace < 6 {
		return false
	}
	return float64(blocks)/float64(total) >= 0.8
}

// stripANSI drops ESC sequences: ESC, an optional '[', then up to and including
// the first letter, '~' or '@'.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1B) {
		return s
	}
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		if rs[i] != 0x1B {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i + 1
		if j < len(rs) && rs[j] == '[' {
			j++
		}
		for j < len(rs) && !unicode.IsLetter(rs[j]) && rs[j] != '~' && rs[j] != '@' {
			j++
		}
		if j < len(rs) {
			j++
		}
		i = j
	}
	return b.String()
}

// pairBackend is WhatsAppPairingBackend.
type pairBackend interface {
	StartSession(ctx context.Context, h *hermescontainers.Handle) (hermescontainers.Streaming, error)
	IsPaired(ctx context.Context, h *hermescontainers.Handle) bool
	Unlink(ctx context.Context, h *hermescontainers.Handle) (bool, error)
}

// livePairBackend is LiveWhatsAppPairingBackend.
type livePairBackend struct {
	docker     hermescontainers.Docker
	sessionDir string
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (b livePairBackend) StartSession(ctx context.Context, h *hermescontainers.Handle) (hermescontainers.Streaming, error) {
	inner := shellQuote("/opt/hermes/.venv/bin/hermes") + " whatsapp"
	return b.docker.ExecStreaming(ctx, h.ContainerName, []string{"script", "-q", "-c", inner, "/dev/null"})
}

func (b livePairBackend) IsPaired(ctx context.Context, h *hermescontainers.Handle) bool {
	res, err := b.docker.Exec(ctx, h.ContainerName, []string{"test", "-d", b.sessionDir}, nil)
	return err == nil && res.OK()
}

func (b livePairBackend) Unlink(ctx context.Context, h *hermescontainers.Handle) (bool, error) {
	res, err := b.docker.Exec(ctx, h.ContainerName, []string{"rm", "-rf", b.sessionDir}, nil)
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// errPairSessionNotFound is WhatsAppPairingError.sessionNotFound.
var errPairSessionNotFound = errors.New("sessionNotFound")

// pairing is WhatsAppPairingService. Sessions and their subscribers are
// in-memory: the stream must reach the process that started the pairing.
type pairing struct {
	containers hermescontainers.Containers
	backend    pairBackend
	log        *slog.Logger

	mu       sync.Mutex
	sessions map[uuid.UUID]*pairSession
	byTenant map[uuid.UUID]uuid.UUID
}

type pairSession struct {
	handle   hermescontainers.Streaming
	lastQR   *string
	status   string
	terminal *pairEvent
	subs     map[int]chan pairEvent
	nextSub  int
}

func newPairing(containers hermescontainers.Containers, backend pairBackend, log *slog.Logger) *pairing {
	return &pairing{containers: containers, backend: backend, log: log, sessions: map[uuid.UUID]*pairSession{}, byTenant: map[uuid.UUID]uuid.UUID{}}
}

func (p *pairing) startPairing(ctx context.Context, tenant uuid.UUID) (uuid.UUID, error) {
	p.mu.Lock()
	old, had := p.byTenant[tenant]
	p.mu.Unlock()
	if had {
		p.teardown(old)
	}
	h, err := p.containers.EnsureRunning(ctx, tenant)
	if err != nil {
		return uuid.Nil, err
	}
	exec, err := p.backend.StartSession(ctx, h)
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	p.mu.Lock()
	p.sessions[id] = &pairSession{handle: exec, status: "starting", subs: map[int]chan pairEvent{}}
	p.byTenant[tenant] = id
	p.mu.Unlock()
	go p.pump(id, exec) //nolint:gosec // the pairing session outlives the request that starts it
	p.log.Info("whatsapp pairing started", "tenantID", tenant, "sessionID", id)
	return id, nil
}

// subscribe replays the session's state, then follows it. The channel closes at
// the terminal event; an unknown session yields only the not-found error.
func (p *pairing) subscribe(id uuid.UUID) (<-chan pairEvent, func()) {
	out := make(chan pairEvent, 64)
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok {
		out <- pairError(errPairSessionNotFound.Error())
		close(out)
		return out, func() {}
	}
	out <- pairStatus(s.status)
	if s.lastQR != nil {
		out <- qrEvent(*s.lastQR)
	}
	if s.terminal != nil {
		out <- *s.terminal
		close(out)
		return out, func() {}
	}
	n := s.nextSub
	s.nextSub++
	s.subs[n] = out
	return out, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if s, ok := p.sessions[id]; ok {
			if c, ok := s.subs[n]; ok {
				delete(s.subs, n)
				close(c)
			}
		}
	}
}

func (p *pairing) isPaired(ctx context.Context, tenant uuid.UUID) bool {
	h, err := p.containers.Handle(ctx, tenant)
	if err != nil || h == nil {
		return false
	}
	return p.backend.IsPaired(ctx, h)
}

func (p *pairing) unlink(ctx context.Context, tenant uuid.UUID) (bool, error) {
	p.mu.Lock()
	sid, had := p.byTenant[tenant]
	p.mu.Unlock()
	if had {
		p.teardown(sid)
	}
	h, err := p.containers.Handle(ctx, tenant)
	if err != nil || h == nil {
		return true, nil
	}
	ok, err := p.backend.Unlink(ctx, h)
	if err != nil {
		return false, err
	}
	_ = p.containers.ApplyGatewayConfig(ctx, tenant)
	return ok, nil
}

func (p *pairing) pump(id uuid.UUID, exec hermescontainers.Streaming) {
	var parser pairParser
	for line := range exec.Lines() {
		for _, ev := range parser.consume(line) {
			p.handle(id, ev)
		}
		if p.terminal(id) {
			break
		}
	}
	for _, ev := range parser.finish() {
		p.handle(id, ev)
	}
	if !p.terminal(id) {
		code, err := exec.Wait(context.Background())
		if err == nil && code == 0 {
			p.handle(id, linkedEvent)
		} else {
			p.handle(id, pairError("WhatsApp pairing ended before linking. Please try again."))
		}
	}
}

func (p *pairing) terminal(id uuid.UUID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	return ok && s.terminal != nil
}

func (p *pairing) handle(id uuid.UUID, ev pairEvent) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if !ok {
		p.mu.Unlock()
		return
	}
	switch ev.Type {
	case "qr":
		s.lastQR = ev.Payload
		if s.status == "starting" {
			s.status = "awaitingScan"
		}
	case "status":
		s.status = *ev.Payload
	case "linked":
		s.terminal, s.status = &linkedEvent, "linked"
	case "error":
		e := ev
		s.terminal, s.status = &e, "failed"
	}
	for _, c := range s.subs {
		select {
		case c <- ev:
		default:
		}
	}
	done := s.terminal != nil
	if done {
		for n, c := range s.subs {
			close(c)
			delete(s.subs, n)
		}
	}
	p.mu.Unlock()
	if done {
		time.AfterFunc(3*time.Second, func() { p.teardown(id) })
	}
}

func (p *pairing) teardown(id uuid.UUID) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if !ok {
		p.mu.Unlock()
		return
	}
	delete(p.sessions, id)
	for n, c := range s.subs {
		close(c)
		delete(s.subs, n)
	}
	for t, sid := range p.byTenant {
		if sid == id {
			delete(p.byTenant, t)
		}
	}
	p.mu.Unlock()
	if s.handle != nil {
		s.handle.Cancel()
	}
}
