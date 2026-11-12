package authroutes

import (
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// PairingController: QR-from-mobile web sign-in. The browser starts a pairing
// and polls it; the signed-in app approves it with the code, which mints that
// account's tokens for the browser.
//
// Records live in process memory (decisions.md D-ratelimit-memory: Swift keeps
// them in the rate-limit driver, which is memory), so a restart forgets them.
//
// Swift weaknesses, kept: the pairingId is the only secret on the poll, which
// is unauthenticated and unlimited and hands the tokens out on every poll until
// the record expires, not once; approve has no attempt limit on the six-digit
// code and compares it with ==; two concurrent approves can both mint.

// pairingTTL is how long a pending pairing stays scannable and how long an
// approved one lingers for the browser.
const pairingTTL = 120 * time.Second

var (
	errPairingUnknown  = wire.NewError(http.StatusNotFound, "pairing expired or unknown")
	errPairingApproved = wire.NewError(http.StatusConflict, "pairing already approved")
	errPairingMismatch = wire.NewError(http.StatusUnauthorized, "pairing code mismatch")
)

type pairingAuth struct {
	UserID           uuid.UUID
	Email            string
	AccessToken      string
	RefreshToken     string
	ExpiresIn        int
	VaultInitialized bool
}

// pairingRecord is PairingRecord.
type pairingRecord struct {
	code        string
	expiresAtMs int64
	approved    bool
	auth        *pairingAuth
}

type pairingItem struct {
	rec     pairingRecord
	expires time.Time
}

// pairingStore is MemoryPersistDriver for the "pairing:<id>" keys: set with a
// TTL, get returns nothing once it has passed.
type pairingStore struct {
	mu    sync.Mutex
	items map[string]pairingItem
	now   func() time.Time
}

func newPairingStore(now func() time.Time) *pairingStore {
	return &pairingStore{items: map[string]pairingItem{}, now: now}
}

func (s *pairingStore) set(id string, rec pairingRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, it := range s.items {
		if now.After(it.expires) {
			delete(s.items, k)
		}
	}
	s.items[id] = pairingItem{rec: rec, expires: now.Add(pairingTTL)}
}

func (s *pairingStore) get(id string) (pairingRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok || s.now().After(it.expires) {
		return pairingRecord{}, false
	}
	return it.rec, true
}

// pairingStartResponse is PairingStartResponse; expiresAt is epoch ms.
type pairingStartResponse struct {
	PairingID string `json:"pairingId"`
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
}

// pairingPollResponse is PairingPollResponse: the optionals are left out while
// the pairing is pending.
type pairingPollResponse struct {
	Approved         bool       `json:"approved"`
	UserID           *wire.UUID `json:"userId,omitempty"`
	Email            *string    `json:"email,omitempty"`
	AccessToken      *string    `json:"accessToken,omitempty"`
	RefreshToken     *string    `json:"refreshToken,omitempty"`
	ExpiresIn        *int       `json:"expiresIn,omitempty"`
	VaultInitialized *bool      `json:"vaultInitialized,omitempty"`
}

type pairingApproveRequest struct {
	Code string `json:"code"`
}

// pairingStart mints a pending pairing; the body is never read.
func (m *Module) pairingStart(c *echo.Context) error {
	pid := ids.Upper(uuid.New()) // UUID().uuidString
	code := m.opt.PairingCodes.Generate()
	expiresAtMs := m.opt.Now().Add(pairingTTL).UnixMilli()
	m.pairings.set(pid, pairingRecord{code: code, expiresAtMs: expiresAtMs})
	return writeJSON(c, pairingStartResponse{PairingID: pid, Code: code, ExpiresAt: expiresAtMs})
}

// pairingParam is ctx.parameters.get("pairingId"). Hummingbird resolves the
// path before the method, so GET /v1/auth/pairing/start lands on the POST-only
// literal and is a bare 404; Echo would fall through to :pairingId.
func pairingParam(c *echo.Context) (string, error) {
	id := c.Param("pairingId")
	if c.Request().Method == http.MethodGet && id == "start" {
		return "", echo.ErrNotFound
	}
	return id, nil
}

// pairingPoll is unauthenticated: pending is {"approved":false}, approved is
// the token pair, unknown or expired is 404.
func (m *Module) pairingPoll(c *echo.Context) error {
	id, err := pairingParam(c)
	if err != nil {
		return err
	}
	rec, ok := m.pairings.get(id)
	if !ok {
		return errPairingUnknown
	}
	if !rec.approved || rec.auth == nil {
		return writeJSON(c, pairingPollResponse{})
	}
	a := rec.auth
	return writeJSON(c, pairingPollResponse{
		Approved: true, UserID: &wire.UUID{UUID: a.UserID}, Email: &a.Email,
		AccessToken: &a.AccessToken, RefreshToken: &a.RefreshToken,
		ExpiresIn: &a.ExpiresIn, VaultInitialized: &a.VaultInitialized,
	})
}

// pairingApprove mints tokens for the authenticated user and stores them on
// the pairing, whose TTL starts again.
func (m *Module) pairingApprove(c *echo.Context) error {
	userID, ok := auth.Identity(c)
	if !ok {
		return auth.ErrUnauthorized
	}
	id := c.Param("pairingId")
	body, err := wire.Bind[pairingApproveRequest](c)
	if err != nil {
		return err
	}
	rec, ok := m.pairings.get(id)
	if !ok {
		return errPairingUnknown
	}
	if rec.approved {
		return errPairingApproved
	}
	if rec.code != body.Code {
		return errPairingMismatch
	}
	ctx := finish(c)
	u, err := m.opt.Service.FindUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return auth.ErrUnauthorized
	}
	resp, err := m.opt.Service.IssueTokens(ctx, u)
	if err != nil {
		return err
	}
	rec.approved = true
	rec.auth = &pairingAuth{
		UserID: resp.UserID.UUID, Email: resp.Email, AccessToken: resp.AccessToken,
		RefreshToken: resp.RefreshToken, ExpiresIn: resp.ExpiresIn, VaultInitialized: resp.VaultInitialized,
	}
	m.pairings.set(id, rec)
	return c.NoContent(http.StatusNoContent)
}
