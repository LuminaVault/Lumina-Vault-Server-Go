package workflows

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lumina/internal/ids"
	"lumina/internal/modules/workflows/db"
	"lumina/internal/secretbox"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

const maxDecimalDigits = 38

const (
	maxHookBodyBytes  = 1024 * 1024
	replayWindowSecs  = 5 * 60
	maxIdempotencyLen = 200
)

var (
	errSecretKeyRequired = wire.NewError(http.StatusServiceUnavailable, "workflow_webhooks_require_secret_master_key")
	errHooksUnavailable  = wire.NewError(http.StatusServiceUnavailable, "workflow_webhooks_unavailable")
	errHookNotFound      = wire.NewError(http.StatusNotFound, "workflow_hook_not_found")
	errHookHeaders       = wire.NewError(http.StatusUnauthorized, "invalid_workflow_webhook_headers")
	errHookSignature     = wire.NewError(http.StatusUnauthorized, "invalid_workflow_webhook_signature")
)

// signWebhook is WorkflowWebhookSignature.sign: hex HMAC-SHA256 of "<ts>.<body>".
func signWebhook(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyWebhook is WorkflowWebhookSignature.verify. The supplied signature is
// lowercased, so hex in either case passes.
func verifyWebhook(signature, secret, timestamp string, body []byte) bool {
	want := signWebhook(secret, timestamp, body)
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(signature))) == 1
}

// rotateWebhook is WorkflowController.rotateWebhook and WorkflowWebhookController.rotate.
func (m *Module) rotateWebhook(ctx context.Context, tenant, workflowID uuid.UUID) (WorkflowWebhookCredentialDTO, error) {
	var out WorkflowWebhookCredentialDTO
	if _, err := m.requireAuthorTier(ctx, tenant); err != nil {
		return out, err
	}
	if m.opts.Box == nil {
		return out, errSecretKeyRequired
	}
	workflow, err := m.requireWorkflow(ctx, tenant, workflowID)
	if err != nil {
		return out, err
	}
	def, err := decodeDefinition(workflow.DraftDefinition)
	if err != nil {
		return out, err
	}
	if def.Trigger != Webhook {
		return out, invalidError{"workflow trigger must be webhook"}
	}
	secret, err := randomSecret()
	if err != nil {
		return out, err
	}
	sealed, err := m.opts.Box.Seal(secret, tenant)
	if err != nil {
		return out, err
	}
	hookID, err := m.saveWebhook(ctx, tenant, workflowID, sealed)
	if err != nil {
		return out, err
	}
	return WorkflowWebhookCredentialDTO{
		HookID: wire.UUID{UUID: hookID},
		Path:   "/v1/workflow-hooks/" + ids.Upper(hookID),
		Secret: secret,
	}, nil
}

// saveWebhook updates the workflow's one row or creates it. Like Swift it checks
// first; losing the race on UNIQUE (workflow_id) is an error.
func (m *Module) saveWebhook(ctx context.Context, tenant, workflowID uuid.UUID, sealed secretbox.Sealed) (uuid.UUID, error) {
	now := m.now()
	id, err := m.q.WebhookIDForWorkflow(ctx, db.WebhookIDForWorkflowParams{TenantID: tenant, WorkflowID: workflowID})
	if errors.Is(err, pgx.ErrNoRows) {
		id = uuid.New()
		return id, m.q.InsertWebhook(ctx, db.InsertWebhookParams{
			ID: id, TenantID: tenant, WorkflowID: workflowID,
			SecretCiphertext: sealed.Ciphertext, SecretNonce: sealed.Nonce, Now: now,
		})
	}
	if err != nil {
		return uuid.Nil, err
	}
	return id, m.q.UpdateWebhookSecret(ctx, db.UpdateWebhookSecretParams{
		ID: id, SecretCiphertext: sealed.Ciphertext, SecretNonce: sealed.Nonce, Now: now,
	})
}

func randomSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// ingest is POST /v1/workflow-hooks/:hookID. The headers are checked before the
// body is read. It has no mapErrors in Swift, so anything enqueue refuses (a
// disabled or unpublished workflow, a tier without access, the active run limit)
// is a bare 500.
func (m *Module) ingest(c *echo.Context) error {
	if m.opts.Box == nil {
		return errHooksUnavailable
	}
	ctx := c.Request().Context()
	hookID, err := ids.Parse(c.Param("hookID"))
	if err != nil {
		return errHookNotFound
	}
	hook, err := m.q.GetWebhook(ctx, hookID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errHookNotFound
	}
	if err != nil {
		return err
	}
	timestamp, signature, idempotencyKey, ok := m.hookHeaders(c.Request())
	if !ok {
		return errHookHeaders
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxHookBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxHookBodyBytes {
		return wire.ErrBodyTooLarge
	}
	secret, err := m.opts.Box.Open(secretbox.Sealed{Ciphertext: hook.SecretCiphertext, Nonce: hook.SecretNonce}, hook.TenantID)
	if err != nil {
		return err
	}
	if !verifyWebhook(signature, secret, timestamp, body) {
		return errHookSignature
	}
	dedupe := "webhook:" + idempotencyKey
	run, err := m.enqueue(ctx, hook.TenantID, hook.WorkflowID, Webhook, flattenPayload(body), nil, &dedupe)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, run)
}

// hookHeaders reads the three headers, all or nothing: one 401 covers any of them
// being absent or bad, and a timestamp outside five minutes either way.
func (m *Module) hookHeaders(r *http.Request) (timestamp, signature, idempotencyKey string, ok bool) {
	timestamp, ok = wire.HeaderValue(r, "x-lumina-timestamp")
	if !ok {
		return "", "", "", false
	}
	seconds, ok := wire.ParseDouble(timestamp)
	if !ok {
		return "", "", "", false
	}
	now := float64(m.now().UnixNano()) / 1e9
	// NaN fails this too, as `abs(...) <= window` does.
	if !(math.Abs(now-seconds) <= replayWindowSecs) {
		return "", "", "", false
	}
	signature, ok = wire.HeaderValue(r, "x-lumina-signature")
	if !ok {
		return "", "", "", false
	}
	idempotencyKey, ok = wire.HeaderValue(r, "idempotency-key")
	if !ok || idempotencyKey == "" || swiftstr.Count(idempotencyKey) > maxIdempotencyLen {
		return "", "", "", false
	}
	return timestamp, signature, idempotencyKey, true
}

// flattenPayload is WorkflowWebhookController.payload: the run input is string to
// string, so "raw" holds the body and a JSON object's top-level keys join it, a
// "raw" key among them replacing the body.
func flattenPayload(body []byte) StringMap {
	result := StringMap{"raw": decodeUTF8(body)}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var object map[string]any
	if err := dec.Decode(&object); err != nil || object == nil {
		return result
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return result
	}
	for key, value := range object {
		switch v := value.(type) {
		case string:
			result[key] = v
		case json.Number:
			result[key] = numberString(v)
		case bool:
			result[key] = boolString(v)
		case nil:
			result[key] = ""
		default:
			if data, err := serializeJSON(value); err == nil {
				result[key] = data
			}
		}
	}
	return result
}

// decodeUTF8 is String(decoding:as: UTF8.self): an ill-formed sequence becomes U+FFFD.
func decodeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// boolString is NSNumber(value: Bool).stringValue.
func boolString(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// numberString is NSNumber.stringValue for what corelibs' JSONSerialization parses a
// number to: an Int64 or UInt64 if it has no "." or "e", then a Decimal if it has
// more digits than a Double holds and its exponent fits one, then a Double.
func numberString(n json.Number) string {
	raw := n.String()
	expAt := strings.IndexByte(raw, 'e')
	digits := len(raw)
	if expAt >= 0 {
		digits = expAt
	}
	if expAt < 0 && !strings.Contains(raw, ".") {
		if strings.HasPrefix(raw, "-") {
			if v, err := strconv.ParseInt(raw, 10, 64); digits <= 19 && err == nil {
				return strconv.FormatInt(v, 10)
			}
		} else if v, err := strconv.ParseUint(raw, 10, 64); digits <= 20 && err == nil {
			return strconv.FormatUint(v, 10)
		}
	}
	exponent := 0
	if expAt >= 0 {
		// The JSON grammar has already checked this.
		exponent, _ = strconv.Atoi(raw[expAt+1:])
	}
	if digits > 17 && exponent >= -128 && exponent <= 127 {
		return decimalString(raw[:digits], exponent)
	}
	f, err := n.Float64()
	if err != nil {
		return raw
	}
	return swiftDouble(f)
}

// decimalString is Decimal(string:).description: no exponent, no trailing zeros
// after the point, and at most Decimal's 38 significant digits.
func decimalString(mantissa string, exponent int) string {
	negative := strings.HasPrefix(mantissa, "-")
	mantissa = strings.TrimPrefix(mantissa, "-")
	whole, fraction, _ := strings.Cut(strings.ToUpper(mantissa), ".")
	if at := strings.IndexByte(fraction, 'E'); at >= 0 {
		fraction = fraction[:at]
	}
	digits := whole + fraction
	point := len(whole) + exponent
	trimmed := strings.TrimLeft(digits, "0")
	point -= len(digits) - len(trimmed)
	digits = trimmed
	if len(digits) > maxDecimalDigits {
		digits = digits[:maxDecimalDigits]
	}
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		return "0"
	}
	var out string
	switch {
	case point <= 0:
		out = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		out = digits + strings.Repeat("0", point-len(digits))
	default:
		out = digits[:point] + "." + digits[point:]
	}
	if negative {
		return "-" + out
	}
	return out
}

// swiftDouble is Double.description: shortest digits that round-trip, as a plain
// decimal with ".0" unless the value is above 2^53 or below 0.0001.
func swiftDouble(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	scientific := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, power, _ := strings.Cut(scientific, "e")
	exponent, _ := strconv.Atoi(power)
	if exponent < -4 || f > 1<<53 {
		return fmt.Sprintf("%s%se%+03d", sign, mantissa, exponent)
	}
	plain := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(plain, ".") {
		plain += ".0"
	}
	return sign + plain
}

// serializeJSON is JSONSerialization.data(withJSONObject:) over a nested value:
// compact, with "/" escaped. Swift's key order follows its hashed dictionary, so
// it differs per process; here it is sorted.
func serializeJSON(value any) (string, error) {
	var buf strings.Builder
	if err := writeJSON(&buf, value); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeJSON(buf *strings.Builder, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(v))
	case json.Number:
		buf.WriteString(numberString(v))
	case string:
		encoded, err := wire.MarshalFoundation(v)
		if err != nil {
			return err
		}
		buf.Write(encoded)
	case []any:
		buf.WriteByte('[')
		for i, element := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, element); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeJSON(buf, v[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	}
	return nil
}
