package githubsync

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"github.com/google/uuid"
)

// randomHandle is 32 random bytes, base64url: a state, a completion handle or
// a picker session id.
func randomHandle() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// onceStore is connected apps' one-shot store: in memory, per process, a
// take removes the entry, entries are pruned after ttl. It is bounded: at
// most perOwner live entries for one owner and max in all, and a put over
// either limit drops the oldest, so a client that starts sign-ins in a loop
// only ever replaces its own.
type onceStore[V any] struct {
	mu       sync.Mutex
	items    map[string]stamped[V]
	ttl      time.Duration
	now      func() time.Time
	perOwner int
	max      int
	seq      uint64
}

type stamped[V any] struct {
	v     V
	at    time.Time
	owner string
	seq   uint64
}

func newOnceStore[V any](ttl time.Duration, now func() time.Time, perOwner, maxItems int) *onceStore[V] {
	return &onceStore[V]{items: map[string]stamped[V]{}, ttl: ttl, now: now, perOwner: perOwner, max: maxItems}
}

func (s *onceStore[V]) put(key, owner string, v V) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	for s.count(owner) >= s.perOwner {
		s.dropOldest(owner, true)
	}
	for len(s.items) >= s.max {
		s.dropOldest("", false)
	}
	s.seq++
	s.items[key] = stamped[V]{v: v, at: s.now(), owner: owner, seq: s.seq}
}

func (s *onceStore[V]) count(owner string) int {
	n := 0
	for _, it := range s.items {
		if it.owner == owner {
			n++
		}
	}
	return n
}

// dropOldest removes the oldest entry, of one owner or of all.
func (s *onceStore[V]) dropOldest(owner string, sameOwner bool) {
	var (
		oldest string
		seq    uint64
		found  bool
	)
	for k, it := range s.items {
		if sameOwner && it.owner != owner {
			continue
		}
		if !found || it.seq < seq {
			oldest, seq, found = k, it.seq, true
		}
	}
	delete(s.items, oldest)
}

func (s *onceStore[V]) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

func (s *onceStore[V]) take(key string) (V, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	it, ok := s.items[key]
	delete(s.items, key)
	return it.v, ok
}

func (s *onceStore[V]) prune() {
	cutoff := s.now().Add(-s.ttl)
	for k, v := range s.items {
		if !v.at.After(cutoff) {
			delete(s.items, k)
		}
	}
}

// flow is a started sign-in: who started it, for which vault, and where the
// browser goes back to.
type flow struct {
	user, vault uuid.UUID
	returnTo    *string
}

// parked is a callback's code waiting for the signed-in client (D3 step 2).
type parked struct {
	flow flow
	code string
}

// picker is a picker session (D3 step 3): the GitHub user token, held in
// memory only, bound to one Lumina user and one vault.
type picker struct {
	user, vault uuid.UUID
	token       string
	login       string
	expires     time.Time
}

// pickers holds picker sessions. An expired or dropped session's user token
// is handed to revoke (D4), outside the lock.
type pickers struct {
	mu     sync.Mutex
	items  map[string]picker
	ttl    time.Duration
	now    func() time.Time
	revoke func(token string)
}

func newPickers(ttl time.Duration, now func() time.Time, revoke func(string)) *pickers {
	return &pickers{items: map[string]picker{}, ttl: ttl, now: now, revoke: revoke}
}

// put stores a session and returns its id. A timer prunes it at expiry, so a
// session nobody comes back to still has its token revoked.
func (p *pickers) put(s picker) (string, time.Time, error) {
	id, err := randomHandle()
	if err != nil {
		return "", time.Time{}, err
	}
	s.expires = p.now().Add(p.ttl)
	p.mu.Lock()
	p.items[id] = s
	p.mu.Unlock()
	time.AfterFunc(p.ttl+time.Second, p.prune)
	return id, s.expires, nil
}

// get returns the live session id names, only for the user and vault it was
// made for. Any other caller sees no session.
func (p *pickers) get(id string, user, vault uuid.UUID) (picker, bool) {
	p.prune()
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.items[id]
	if !ok || s.user != user || s.vault != vault {
		return picker{}, false
	}
	return s, true
}

// drop ends a session and revokes its token.
func (p *pickers) drop(id string) {
	p.mu.Lock()
	s, ok := p.items[id]
	delete(p.items, id)
	p.mu.Unlock()
	if ok {
		p.revoke(s.token)
	}
}

// prune ends every expired session and revokes its token.
func (p *pickers) prune() {
	now := p.now()
	var expired []string
	p.mu.Lock()
	for id, s := range p.items {
		if !now.Before(s.expires) {
			expired = append(expired, s.token)
			delete(p.items, id)
		}
	}
	p.mu.Unlock()
	for _, token := range expired {
		p.revoke(token)
	}
}
