package githubsync

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"lumina/internal/githubsync"
	"lumina/internal/ratelimit"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
)

// MARK: POST /v1/webhooks/github

// webhookPayload is the little of a GitHub webhook body this server reads.
// Commit messages, file names and author emails are never decoded (D2).
type webhookPayload struct {
	Ref          string `json:"ref"`
	After        string `json:"after"`
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		ID int64 `json:"id"`
	} `json:"repository"`
	RepositoriesRemoved []struct {
		ID int64 `json:"id"`
	} `json:"repositories_removed"`
}

// signatureOf reads X-Hub-Signature-256: "sha256=" and the 32 bytes of an
// HMAC-SHA256 in hex. Anything else is not a signature.
func signatureOf(header string) ([]byte, bool) {
	given, ok := strings.CutPrefix(header, "sha256=")
	if !ok || len(given) != 2*sha256.Size {
		return nil, false
	}
	sent, err := hex.DecodeString(given)
	return sent, err == nil
}

// validSignature checks sent against the HMAC-SHA256 of the raw body, in
// constant time.
func validSignature(secret string, body, sent []byte) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return subtle.ConstantTimeCompare(mac.Sum(nil), sent) == 1
}

// unsigned answers a delivery that did not prove it comes from GitHub: 401,
// or 429 once its address has sent more of those than the limit. Only these
// are counted: a flood without the secret can then never have a signed
// delivery refused, wherever it comes from (GitHub's deliveries and anyone
// else's share an address behind some proxies).
func (m *Module) unsigned(c *echo.Context) error {
	if limited, retry := m.limits.Hit(ratelimit.GitHubWebhookByIP, ratelimit.IPKey(c.Request())); limited {
		c.Response().Header().Set("Retry-After", strconv.Itoa(retry))
		return c.NoContent(http.StatusTooManyRequests)
	}
	m.log.Warn("github sync: webhook with a missing or wrong signature")
	return c.NoContent(http.StatusUnauthorized)
}

// webhook is GitHub's delivery endpoint (D9). In order: a request without a
// well-formed signature header is refused before a byte of its body is read;
// the body is read, at most 5 MiB of it; its signature is checked; only then
// is the delivery recorded, so nobody without the secret can use up a
// delivery id; a second delivery of an id answers 200 and does nothing; a
// new one answers 202, and everything it causes happens after the response.
func (m *Module) webhook(c *echo.Context) error {
	if m.secret == "" {
		m.log.Error("github sync: webhook secret not configured")
		return c.NoContent(http.StatusInternalServerError)
	}
	req := c.Request()
	sent, ok := signatureOf(req.Header.Get("X-Hub-Signature-256"))
	if !ok {
		return m.unsigned(c)
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxWebhookBody+1))
	if err != nil {
		return c.NoContent(http.StatusBadRequest)
	}
	if len(body) > maxWebhookBody {
		return c.NoContent(http.StatusRequestEntityTooLarge)
	}
	if !validSignature(m.secret, body, sent) {
		return m.unsigned(c)
	}
	id, event := strings.TrimSpace(req.Header.Get("X-GitHub-Delivery")), strings.TrimSpace(req.Header.Get("X-GitHub-Event"))
	if id == "" || event == "" || len(id) > 200 || len(event) > 100 {
		return c.NoContent(http.StatusBadRequest)
	}
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return c.NoContent(http.StatusBadRequest)
	}
	d := githubsync.Delivery{
		ID: id, Event: event, Action: payload.Action, InstallationID: payload.Installation.ID,
		RepoID: payload.Repository.ID, Ref: payload.Ref, After: payload.After,
	}
	for _, r := range payload.RepositoriesRemoved {
		d.Removed = append(d.Removed, r.ID)
	}
	ctx := req.Context()
	inserted, err := m.store.InsertDelivery(ctx, d, m.now())
	if err != nil {
		return err
	}
	if !inserted {
		return c.NoContent(http.StatusOK)
	}
	if m.sync != nil {
		background := context.WithoutCancel(ctx)
		m.deliveries.Go(func() { m.sync.Deliver(background, d) })
	}
	return c.NoContent(http.StatusAccepted)
}
