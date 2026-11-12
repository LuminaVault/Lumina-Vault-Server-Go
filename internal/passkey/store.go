package passkey

import (
	"lumina/internal/ids"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ChallengeTTL is WebAuthnChallengeStore.ttl, for both ceremonies. The options say
// 300 s (registration) and 60 s (sign-in); the store keeps both for 300 s.
const ChallengeTTL = 300 * time.Second

type entry struct {
	challenge []byte
	expiresAt time.Time
}

// Store is WebAuthnChallengeStore: process memory, one per server, so a ceremony begun
// on one replica (or on Swift) cannot finish on another. wave-6-8-plan.md flips the
// group in one step for that reason.
//
// Swift: registrations are keyed by the authenticated user's id (uppercase uuidString),
// sign-ins by the username in the request body, verbatim. A later begin overwrites the
// pending challenge, so anyone can replace another user's pending sign-in challenge.
// Entries leave only when read after expiry or cleared after a successful finish: a
// failed finish keeps the challenge for another try, and a begin for a username that
// is never finished stays in memory until the process ends.
type Store struct {
	mu            sync.Mutex
	registrations map[string]entry
	auths         map[string]entry
	now           func() time.Time
}

// NewStore returns an empty store on the wall clock.
func NewStore() *Store {
	return NewStoreWithClock(time.Now)
}

// NewStoreWithClock returns an empty store reading now (tests move time).
func NewStoreWithClock(now func() time.Time) *Store {
	return &Store{registrations: map[string]entry{}, auths: map[string]entry{}, now: now}
}

func (s *Store) put(m map[string]entry, key string, challenge []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m[key] = entry{challenge: challenge, expiresAt: s.now().Add(ChallengeTTL)}
}

// get returns the live challenge, and drops the key when there is none (Swift sets
// it to nil whether or not it existed).
func (s *Store) get(m map[string]entry, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := m[key]
	if !ok || !e.expiresAt.After(s.now()) {
		delete(m, key)
		return nil, false
	}
	return e.challenge, true
}

func (s *Store) clear(m map[string]entry, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(m, key)
}

func (s *Store) StoreRegistration(user uuid.UUID, challenge []byte) {
	s.put(s.registrations, ids.Upper(user), challenge)
}

func (s *Store) Registration(user uuid.UUID) ([]byte, bool) {
	return s.get(s.registrations, ids.Upper(user))
}

func (s *Store) ClearRegistration(user uuid.UUID) { s.clear(s.registrations, ids.Upper(user)) }

func (s *Store) StoreAuthentication(username string, challenge []byte) {
	s.put(s.auths, username, challenge)
}

func (s *Store) Authentication(username string) ([]byte, bool) {
	return s.get(s.auths, username)
}

func (s *Store) ClearAuthentication(username string) { s.clear(s.auths, username) }
