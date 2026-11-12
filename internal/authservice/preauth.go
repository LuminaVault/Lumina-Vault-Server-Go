package authservice

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// ConsumeOutcome is Swift's ConsumeOutcome: phone verify answers 410 for
// Expired and 401 for the rest.
type ConsumeOutcome int

const (
	ConsumeOK ConsumeOutcome = iota
	ConsumeNotFound
	ConsumeExpired
	ConsumeLockedOut
	ConsumeWrongCode
)

// Pre-auth challenge defaults: five minutes, five attempts.
const (
	PreAuthLifetime    = 5 * time.Minute
	PreAuthMaxAttempts = 5
)

type preAuthEntry struct {
	codeHash    string
	attempts    int
	channel     string
	destination string
	purpose     string
	expiresAt   time.Time
}

// PreAuthChallengeStore is Swift's PreAuthChallengeStore actor: OTP challenges
// issued before a user exists (phone and magic link share one), in process
// memory, so lost on restart and not shared between replicas.
//
// Swift weaknesses, kept: a challenge is keyed by destination alone, whatever
// its channel or purpose; nothing prunes an expired one that is never consumed
// or reissued, so the maps grow with every distinct destination (only the
// per-IP start limits bound them); and the code hash is compared with ==.
type PreAuthChallengeStore struct {
	mu          sync.Mutex
	byID        map[uuid.UUID]*preAuthEntry
	latest      map[string]uuid.UUID
	lifetime    time.Duration
	maxAttempts int
	now         func() time.Time
}

// NewPreAuthChallengeStore returns a store. A negative lifetime gives
// challenges that are born expired, as the Swift tests use.
func NewPreAuthChallengeStore(lifetime time.Duration, maxAttempts int) *PreAuthChallengeStore {
	return &PreAuthChallengeStore{
		byID: map[uuid.UUID]*preAuthEntry{}, latest: map[string]uuid.UUID{},
		lifetime: lifetime, maxAttempts: maxAttempts, now: time.Now,
	}
}

// Issue burns the destination's outstanding challenge and starts a new one.
func (s *PreAuthChallengeStore) Issue(channel, destination, purpose, code string) (uuid.UUID, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.latest[destination]; ok {
		delete(s.byID, old)
	}
	id := uuid.New()
	expiresAt := s.now().Add(s.lifetime)
	s.byID[id] = &preAuthEntry{
		codeHash: sha256Hex(code), channel: channel, destination: destination, purpose: purpose, expiresAt: expiresAt,
	}
	s.latest[destination] = id
	return id, expiresAt
}

// Consume is the optional-returning consume the magic link uses.
func (s *PreAuthChallengeStore) Consume(destination, code string) (dest, purpose string, ok bool) {
	outcome, dest, purpose := s.ConsumeTyped(destination, code)
	return dest, purpose, outcome == ConsumeOK
}

// ConsumeTyped checks code against the destination's latest challenge,
// counting a wrong one and burning the challenge on success, expiry or the
// fifth failure.
func (s *PreAuthChallengeStore) ConsumeTyped(destination, code string) (outcome ConsumeOutcome, dest, purpose string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.latest[destination]
	if !ok {
		return ConsumeNotFound, "", ""
	}
	e, ok := s.byID[id]
	if !ok {
		return ConsumeNotFound, "", ""
	}
	burn := func() {
		delete(s.byID, id)
		delete(s.latest, destination)
	}
	if e.expiresAt.Before(s.now()) {
		burn()
		return ConsumeExpired, "", ""
	}
	if e.attempts >= s.maxAttempts {
		burn()
		return ConsumeLockedOut, "", ""
	}
	if e.codeHash != sha256Hex(code) {
		e.attempts++
		if e.attempts >= s.maxAttempts {
			burn()
			return ConsumeLockedOut, "", ""
		}
		return ConsumeWrongCode, "", ""
	}
	burn()
	return ConsumeOK, e.destination, e.purpose
}
