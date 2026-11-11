package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lumina/internal/modules/calendar"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The skill tools that call out: weather_forecast (Open-Meteo, keyless; Swift's
// OpenMeteoClient, WeatherForecastTool and LastKnownLocationStore) and
// mail_inbox_recent (GmailClient and GmailInboxService). Their JSON goes to the
// model only, never to a client.

const (
	openMeteoEndpoint = "https://api.open-meteo.com/v1/forecast"
	dryThresholdMM    = 1.0
	gmailBase         = "https://gmail.googleapis.com/gmail/v1/users/me"
	gmailRecentQuery  = "newer_than:1d"
	maxToolBody       = 4 << 20
)

// locationFix is LocationFix.
type locationFix struct {
	lat, lng   float64
	place      *string
	capturedAt time.Time
}

// fixFromDeviceRead is LocationFix.fromDeviceReadResult: an ok result whose
// items (a JSON string, or JSON) hold a {lat,lng} in range.
func fixFromDeviceRead(result string, now time.Time) *locationFix {
	var env map[string]any
	if json.Unmarshal([]byte(result), &env) != nil || env["status"] != "ok" {
		return nil
	}
	var items []any
	switch v := env["items"].(type) {
	case string:
		if json.Unmarshal([]byte(v), &items) != nil {
			return nil
		}
	case []any:
		items = v
	default:
		return nil
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		lat, ok1 := number(first(item, "lat", "latitude"))
		lng, ok2 := number(first(item, "lng", "lon", "longitude"))
		if !ok1 || !ok2 || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
			continue
		}
		fix := &locationFix{lat: lat, lng: lng, capturedAt: now}
		if p, ok := item["place"].(string); ok && p != "" {
			fix.place = &p
		}
		if at, ok := item["at"].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(at)); err == nil {
				fix.capturedAt = t
			}
		} else if secs, ok := item["at"].(float64); ok && secs > 0 {
			fix.capturedAt = time.Unix(0, int64(secs*float64(time.Second)))
		}
		return fix
	}
	return nil
}

func first(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// LastKnownLocationStore.save and .load: the fix on the users row.
func saveFix(ctx context.Context, pool *pgxpool.Pool, tenant uuid.UUID, fix locationFix) error {
	_, err := pool.Exec(ctx, `UPDATE users SET last_location_lat = $2, last_location_lng = $3,
		last_location_place = $4, last_location_at = $5 WHERE id = $1`, tenant, fix.lat, fix.lng, fix.place, fix.capturedAt)
	return err
}

func loadFix(ctx context.Context, pool *pgxpool.Pool, tenant uuid.UUID) *locationFix {
	var lat, lng *float64
	var place *string
	var at *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_location_lat, last_location_lng, last_location_place, last_location_at
		FROM users WHERE id = $1`, tenant).Scan(&lat, &lng, &place, &at); err != nil || lat == nil || lng == nil {
		return nil
	}
	fix := &locationFix{lat: *lat, lng: *lng, place: place, capturedAt: time.Unix(0, 0)}
	if at != nil {
		fix.capturedAt = *at
	}
	return fix
}

type forecastDay struct {
	date          string
	precipitation *float64
	code          *int
}

func openMeteoURL(lat, lng float64) string {
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%.4f", lat))
	q.Set("longitude", fmt.Sprintf("%.4f", lng))
	q.Set("daily", "precipitation_sum,weathercode")
	q.Set("forecast_days", "7")
	q.Set("timezone", "auto")
	return openMeteoEndpoint + "?" + q.Encode()
}

func getJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxToolBody))
	return resp.StatusCode, body, err
}

// dailyForecast is OpenMeteoClient.dailyForecast plus parse.
func dailyForecast(ctx context.Context, client *http.Client, lat, lng float64) ([]forecastDay, error) {
	status, body, err := getJSON(ctx, client, openMeteoURL(lat, lng), nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("open-meteo http %d", status)
	}
	var env struct {
		Daily map[string]json.RawMessage `json:"daily"`
	}
	if json.Unmarshal(body, &env) != nil || env.Daily == nil {
		return nil, errors.New("open-meteo malformed response")
	}
	var dates []string
	if json.Unmarshal(env.Daily["time"], &dates) != nil || dates == nil {
		return nil, errors.New("open-meteo malformed response")
	}
	var rain, codes []any
	_ = json.Unmarshal(env.Daily["precipitation_sum"], &rain)
	codeKey := "weathercode"
	if _, ok := env.Daily[codeKey]; !ok {
		codeKey = "weather_code"
	}
	_ = json.Unmarshal(env.Daily[codeKey], &codes)
	out := make([]forecastDay, len(dates))
	for i, d := range dates {
		out[i].date = d
		if i < len(rain) {
			if f, ok := rain[i].(float64); ok {
				out[i].precipitation = &f
			}
		}
		if i < len(codes) {
			if f, ok := codes[i].(float64); ok {
				c := int(f)
				out[i].code = &c
			}
		}
	}
	return out, nil
}

// weatherSummary is OpenMeteoClient.summary(for:).
func weatherSummary(code *int) string {
	if code == nil {
		return "unknown"
	}
	switch *code {
	case 0:
		return "clear"
	case 1, 2:
		return "partly cloudy"
	case 3:
		return "overcast"
	case 45, 48:
		return "fog"
	case 51, 53, 55, 56, 57:
		return "drizzle"
	case 61, 63, 65, 66, 67:
		return "rain"
	case 71, 73, 75, 77:
		return "snow"
	case 80, 81, 82:
		return "rain showers"
	case 85, 86:
		return "snow showers"
	case 95, 96, 99:
		return "thunderstorm"
	}
	return "unknown"
}

func isDry(d forecastDay) bool { return d.precipitation != nil && *d.precipitation < dryThresholdMM }

// renderForecast is WeatherForecastTool.render.
func renderForecast(days []forecastDay, fix locationFix, source string) string {
	location := map[string]any{
		"lat": fix.lat, "lng": fix.lng, "source": source,
		"captured_at": fix.capturedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if fix.place != nil {
		location["place"] = *fix.place
	}
	rendered := make([]map[string]any, len(days))
	streak, streaking := 0, true
	for i, d := range days {
		row := map[string]any{"date": d.date, "summary": weatherSummary(d.code), "dry": isDry(d)}
		if d.precipitation != nil {
			row["precipitation_mm"] = *d.precipitation
		}
		if d.code != nil {
			row["weather_code"] = *d.code
		}
		rendered[i] = row
		if streaking && isDry(d) {
			streak++
		} else {
			streaking = false
		}
	}
	return encodeToolJSON(map[string]any{"status": "ok", "location": location, "days": rendered, "dry_streak_days": streak})
}

// weather is WeatherForecastTool.run: the phone's live fix (kept for later), else
// the stored one when Location consent allows, else an error.
func (r *Runner) weather(ctx context.Context, tenant uuid.UUID) string {
	var fix *locationFix
	source := ""
	if r.device != nil {
		fix = fixFromDeviceRead(r.device.DeviceRead(ctx, tenant, "location", map[string]string{}), time.Now())
	}
	if fix != nil {
		if err := saveFix(ctx, r.pool, tenant, *fix); err != nil {
			r.log.WarnContext(ctx, fmt.Sprintf("weather: could not keep location fix tenant=%s: %v", tenant, err))
		}
		source = "device"
	} else if r.consentAllows(ctx, tenant, "location") {
		if fix = loadFix(ctx, r.pool, tenant); fix != nil {
			source = "cached"
		}
	}
	if fix == nil {
		return toolErrorJSON("no location available — the phone did not answer and no earlier location is stored (Location access must be on)")
	}
	days, err := dailyForecast(ctx, r.http, fix.lat, fix.lng)
	if err != nil {
		r.log.WarnContext(ctx, fmt.Sprintf("weather_forecast fetch failed tenant=%s: %v", tenant, err))
		return toolErrorJSON("weather service unreachable right now; try again later")
	}
	return renderForecast(days, *fix, source)
}

// consentAllows is AppleConsentController.isAllowed(...).allowed.
func (r *Runner) consentAllows(ctx context.Context, tenant uuid.UUID, domain string) bool {
	var allowed bool
	if err := r.pool.QueryRow(ctx, `SELECT allowed FROM apple_consent WHERE tenant_id = $1 AND domain = $2`,
		tenant, domain).Scan(&allowed); err != nil {
		return false
	}
	return allowed
}

// gmailMessage is GmailClient.MessageSummary.
type gmailMessage struct {
	from, subject, snippet string
	date                   *string
	unread, important      bool
}

var errGmailUnauthorized = errors.New("gmail unauthorized")

func (r *Runner) gmailGet(ctx context.Context, rawURL, token string) ([]byte, error) {
	status, body, err := getJSON(ctx, r.http, rawURL, map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"})
	if err != nil {
		return nil, err
	}
	switch {
	case status >= 200 && status < 300:
		return body, nil
	case status == http.StatusUnauthorized:
		return nil, errGmailUnauthorized
	}
	return nil, fmt.Errorf("gmail http %d", status)
}

// recentInbox is GmailClient.recentInbox.
func (r *Runner) recentInbox(ctx context.Context, token string, maxResults int) ([]gmailMessage, error) {
	q := url.Values{}
	q.Set("q", gmailRecentQuery)
	q.Set("labelIds", "INBOX")
	q.Set("maxResults", strconv.Itoa(max(1, min(maxResults, 25))))
	list, err := r.gmailGet(ctx, gmailBase+"/messages?"+q.Encode(), token)
	if err != nil {
		return nil, err
	}
	var env struct {
		Messages []struct {
			ID *string `json:"id"`
		} `json:"messages"`
	}
	if json.Unmarshal(list, &env) != nil {
		return nil, errors.New("gmail malformed response")
	}
	var out []gmailMessage
	for _, m := range env.Messages {
		if m.ID == nil {
			continue
		}
		mq := url.Values{}
		mq.Set("format", "metadata")
		mq.Add("metadataHeaders", "From")
		mq.Add("metadataHeaders", "Subject")
		mq.Add("metadataHeaders", "Date")
		data, err := r.gmailGet(ctx, gmailBase+"/messages/"+url.PathEscape(*m.ID)+"?"+mq.Encode(), token)
		if err != nil {
			return nil, err
		}
		if s, ok := parseGmailMetadata(data); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func parseGmailMetadata(data []byte) (gmailMessage, bool) {
	var env struct {
		ID       *string  `json:"id"`
		Snippet  string   `json:"snippet"`
		LabelIDs []string `json:"labelIds"`
		Payload  struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"payload"`
	}
	if json.Unmarshal(data, &env) != nil || env.ID == nil {
		return gmailMessage{}, false
	}
	header := func(name string) *string {
		for _, h := range env.Payload.Headers {
			if strings.EqualFold(h.Name, name) {
				v := h.Value
				return &v
			}
		}
		return nil
	}
	m := gmailMessage{from: "", subject: "(no subject)", snippet: decodeEntities(env.Snippet), date: header("Date")}
	if v := header("From"); v != nil {
		m.from = *v
	}
	if v := header("Subject"); v != nil {
		m.subject = *v
	}
	for _, l := range env.LabelIDs {
		m.unread = m.unread || l == "UNREAD"
		m.important = m.important || l == "IMPORTANT"
	}
	return m, true
}

func decodeEntities(s string) string {
	for _, p := range [][2]string{{"&#39;", "'"}, {"&quot;", "\""}, {"&lt;", "<"}, {"&gt;", ">"}, {"&nbsp;", " "}, {"&amp;", "&"}} {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	return s
}

// gmailInbox is GmailInboxService.recentInboxJSON.
func (r *Runner) gmailInbox(ctx context.Context, tenant uuid.UUID, limit int) string {
	if r.google == nil {
		return toolErrorJSON("Gmail is not available on this server")
	}
	if !r.google.GrantsGmail(ctx, tenant) {
		return toolErrorJSON("Gmail is not connected — the user can connect it in Settings")
	}
	token, err := r.google.AccessToken(ctx, tenant)
	switch {
	case errors.Is(err, calendar.ErrNeedsReauth):
		return toolErrorJSON("Gmail needs to be reconnected in Settings")
	case errors.Is(err, calendar.ErrNotConnected):
		return toolErrorJSON("Gmail is not connected — the user can connect it in Settings")
	case err != nil:
		r.log.WarnContext(ctx, fmt.Sprintf("mail_inbox_recent failed tenant=%s: %v", tenant, err))
		return toolErrorJSON("could not read Gmail right now")
	}
	messages, err := r.recentInbox(ctx, token, limit)
	if err != nil {
		r.log.WarnContext(ctx, fmt.Sprintf("mail_inbox_recent failed tenant=%s: %v", tenant, err))
		return toolErrorJSON("could not read Gmail right now")
	}
	items := make([]map[string]any, len(messages))
	for i, m := range messages {
		row := map[string]any{"from": m.from, "subject": m.subject, "snippet": m.snippet, "unread": m.unread, "important": m.important}
		if m.date != nil {
			row["date"] = *m.date
		}
		items[i] = row
	}
	return encodeToolJSON(map[string]any{"status": "ok", "window": "last 24h", "count": len(items), "messages": items})
}
