package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lumina/internal/swiftstr"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Endpoints are Google's URLs. Tests point them at a stub server; production
// uses DefaultEndpoints.
type Endpoints struct {
	Token        string
	Revoke       string
	CalendarBase string
}

// DefaultEndpoints are GoogleCalendarOAuthClient's and GoogleCalendarClient's.
//
//nolint:gosec // URLs, no credentials
var DefaultEndpoints = Endpoints{
	Token:        "https://oauth2.googleapis.com/token",
	Revoke:       "https://oauth2.googleapis.com/revoke",
	CalendarBase: "https://www.googleapis.com/calendar/v3",
}

const (
	authorizeEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	// scopeCalendarConnect is what "Connect Calendar" asks for.
	scopeCalendarConnect = "https://www.googleapis.com/auth/calendar.events openid email"
	scopeGmailReadonly   = "https://www.googleapis.com/auth/gmail.readonly"
	scopeCalendarEvents  = "https://www.googleapis.com/auth/calendar.events"
	// scopeGmailConnect relies on include_granted_scopes=true to fold in Calendar.
	scopeGmailConnect = scopeGmailReadonly + " openid email"
)

// grants is GoogleCalendarOAuthClient.grants: whether a space-separated OAuth
// scope string includes wanted, or a broader scope that implies it. A nil
// scope grants nothing.
func grants(scope *string, wanted string) bool {
	if scope == nil {
		return false
	}
	granted := map[string]bool{}
	for f := range strings.SplitSeq(*scope, " ") {
		if f != "" {
			granted[f] = true
		}
	}
	if granted[wanted] {
		return true
	}
	switch wanted {
	case scopeGmailReadonly:
		return granted["https://mail.google.com/"] || granted["https://www.googleapis.com/auth/gmail.modify"]
	case scopeCalendarEvents:
		return granted["https://www.googleapis.com/auth/calendar"]
	}
	return false
}

// removeScope is GoogleCalendarOAuthClient.removing. Order is kept and, as in
// Swift's split, runs of spaces collapse.
func removeScope(removed, scope string) string {
	var out []string
	for f := range strings.SplitSeq(scope, " ") {
		if f != "" && f != removed {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// withoutGmail is GoogleCalendarOAuthService.withoutGmail.
func withoutGmail(scope string) string {
	for _, s := range []string{scopeGmailReadonly, "https://mail.google.com/", "https://www.googleapis.com/auth/gmail.modify"} {
		scope = removeScope(s, scope)
	}
	return scope
}

// withoutCalendar is GoogleCalendarOAuthService.withoutCalendar.
func withoutCalendar(scope string) string {
	for _, s := range []string{scopeCalendarEvents, "https://www.googleapis.com/auth/calendar"} {
		scope = removeScope(s, scope)
	}
	return scope
}

// tokenResponse is GoogleCalendarOAuthClient.TokenResponse.
type tokenResponse struct {
	accessToken  string
	refreshToken *string
	expiresIn    int
	scope        *string
	idToken      *string
}

// httpStatusError is a non-2xx answer from Google. Like Swift's other Google
// errors it is never shown to the caller: handlers turn it into a bare 500.
type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("google: http %d: %s", e.status, e.body)
}

var (
	errMalformedResponse = errors.New("google: malformed response")
	errRefreshRejected   = errors.New("google: refresh token rejected")
	errUnauthorized      = errors.New("google: unauthorized")
	errRateLimited       = errors.New("google: rate limited")
	errSyncTokenExpired  = errors.New("google: sync token expired")
)

// oauthClient is GoogleCalendarOAuthClient: stateless token-endpoint calls.
type oauthClient struct {
	clientID, clientSecret, redirectURI string
	endpoints                           Endpoints
	http                                *http.Client
}

// authorizeURL builds the consent URL. Query encoding follows URLComponents'
// queryItems: `:` `/` `?` stay, spaces are %20.
func (o oauthClient) authorizeURL(state, scope string) string {
	items := [][2]string{
		{"client_id", o.clientID},
		{"redirect_uri", o.redirectURI},
		{"response_type", "code"},
		{"scope", scope},
		{"access_type", "offline"},
		{"prompt", "consent"},
		{"include_granted_scopes", "true"},
		{"state", state},
	}
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = it[0] + "=" + percentEncode(it[1], queryItemAllowed)
	}
	return authorizeEndpoint + "?" + strings.Join(parts, "&")
}

func (o oauthClient) exchangeCode(ctx context.Context, code string) (tokenResponse, error) {
	return o.postForm(ctx, url.Values{
		"code":          {code},
		"client_id":     {o.clientID},
		"client_secret": {o.clientSecret},
		"redirect_uri":  {o.redirectURI},
		"grant_type":    {"authorization_code"},
	})
}

// refresh trades a refresh token for an access token. 400/401 mean the grant
// is dead.
func (o oauthClient) refresh(ctx context.Context, refreshToken string) (tokenResponse, error) {
	t, err := o.postForm(ctx, url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {o.clientID},
		"client_secret": {o.clientSecret},
		"grant_type":    {"refresh_token"},
	})
	var he *httpStatusError
	if errors.As(err, &he) && (he.status == http.StatusBadRequest || he.status == http.StatusUnauthorized) {
		return tokenResponse{}, errRefreshRejected
	}
	return t, err
}

// revoke is best-effort: Swift ignores the status and the caller ignores errors.
func (o oauthClient) revoke(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoints.Revoke, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (o oauthClient) postForm(ctx context.Context, fields url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoints.Token, strings.NewReader(fields.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, data, err := do(o.http, req)
	if err != nil {
		return tokenResponse{}, err
	}
	if status < 200 || status >= 300 {
		return tokenResponse{}, &httpStatusError{status, string(data)}
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return tokenResponse{}, errMalformedResponse
	}
	access, ok := raw["access_token"].(string)
	if !ok {
		return tokenResponse{}, errMalformedResponse
	}
	expires := 3600
	if n, ok := raw["expires_in"].(float64); ok && n == float64(int(n)) {
		expires = int(n)
	}
	return tokenResponse{
		accessToken:  access,
		refreshToken: stringField(raw, "refresh_token"),
		expiresIn:    expires,
		scope:        stringField(raw, "scope"),
		idToken:      stringField(raw, "id_token"),
	}, nil
}

// googleAttendee is CalendarAttendees.Attendee, the JSONB shape Swift's
// CalendarSyncWorker and MeetingBriefService read.
type googleAttendee struct {
	Email          *string `json:"email,omitempty"`
	DisplayName    *string `json:"displayName,omitempty"`
	ResponseStatus *string `json:"responseStatus,omitempty"`
}

// storedAttendees is CalendarAttendees (Fluent rejects top-level JSON arrays).
type storedAttendees struct {
	List []googleAttendee `json:"list"`
}

// remoteEvent is GoogleCalendarClient.RemoteEvent.
type remoteEvent struct {
	externalID string
	title      string
	notes      *string
	location   *string
	startsAt   time.Time
	endsAt     time.Time
	allDay     bool
	cancelled  bool
	organizer  *string
	attendees  []googleAttendee
	recurrence []string
	htmlLink   *string
	etag       *string
	updatedAt  time.Time
}

type listResult struct {
	events        []remoteEvent
	nextSyncToken *string
	nextPageToken *string
}

// calendarClient is GoogleCalendarClient: Calendar v3 REST with a bearer token.
type calendarClient struct {
	base string
	http *http.Client
	now  func() time.Time
}

// listEvents takes syncToken for deltas, or a time window for the first sync;
// Google forbids mixing them.
func (g calendarClient) listEvents(ctx context.Context, accessToken string, syncToken *string, timeMin, timeMax *time.Time, pageToken *string) (listResult, error) {
	q := url.Values{"singleEvents": {"true"}, "showDeleted": {"true"}, "maxResults": {"250"}}
	if syncToken != nil {
		q.Set("syncToken", *syncToken)
	} else {
		if timeMin != nil {
			q.Set("timeMin", rfc3339(*timeMin))
		}
		if timeMax != nil {
			q.Set("timeMax", rfc3339(*timeMax))
		}
		q.Set("orderBy", "startTime")
	}
	if pageToken != nil {
		q.Set("pageToken", *pageToken)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/calendars/primary/events?"+q.Encode(), nil)
	if err != nil {
		return listResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	status, data, err := do(g.http, req)
	if err != nil {
		return listResult{}, err
	}
	switch {
	case status >= 200 && status < 300:
	case status == http.StatusUnauthorized:
		return listResult{}, errUnauthorized
	case status == http.StatusGone:
		return listResult{}, errSyncTokenExpired
	case status == http.StatusForbidden, status == http.StatusTooManyRequests:
		return listResult{}, errRateLimited
	default:
		return listResult{}, &httpStatusError{status, string(data)}
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return listResult{}, errMalformedResponse
	}
	out := listResult{nextSyncToken: stringField(raw, "nextSyncToken"), nextPageToken: stringField(raw, "nextPageToken")}
	for _, item := range objects(raw["items"]) {
		if ev, ok := g.parseEvent(item); ok {
			out.events = append(out.events, ev)
		}
	}
	return out, nil
}

// insertEvent creates an event on the primary calendar and returns it.
func (g calendarClient) insertEvent(ctx context.Context, accessToken, title string, startsAt, endsAt time.Time, location, notes *string, attendees []string) (remoteEvent, error) {
	body := map[string]any{
		"summary": title,
		"start":   map[string]string{"dateTime": rfc3339(startsAt)},
		"end":     map[string]string{"dateTime": rfc3339(endsAt)},
	}
	if location != nil {
		body["location"] = *location
	}
	if notes != nil {
		body["description"] = *notes
	}
	if len(attendees) > 0 {
		list := make([]map[string]string, len(attendees))
		for i, a := range attendees {
			list[i] = map[string]string{"email": a}
		}
		body["attendees"] = list
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return remoteEvent{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/calendars/primary/events", bytes.NewReader(encoded))
	if err != nil {
		return remoteEvent{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	status, data, err := do(g.http, req)
	if err != nil {
		return remoteEvent{}, err
	}
	switch {
	case status >= 200 && status < 300:
	case status == http.StatusUnauthorized:
		return remoteEvent{}, errUnauthorized
	case status == http.StatusForbidden, status == http.StatusTooManyRequests:
		return remoteEvent{}, errRateLimited
	default:
		return remoteEvent{}, &httpStatusError{status, string(data)}
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return remoteEvent{}, errMalformedResponse
	}
	ev, ok := g.parseEvent(raw)
	if !ok {
		return remoteEvent{}, errMalformedResponse
	}
	return ev, nil
}

// parseEvent is GoogleCalendarClient.parseEvent. Tombstones may omit
// start/end, so they default to the epoch and the row can still be cancelled.
func (g calendarClient) parseEvent(raw map[string]any) (remoteEvent, bool) {
	id, ok := raw["id"].(string)
	if !ok {
		return remoteEvent{}, false
	}
	status, _ := raw["status"].(string)
	start, startAllDay := parseEndpoint(raw["start"])
	end, _ := parseEndpoint(raw["end"])
	startsAt := time.Unix(0, 0).UTC()
	if start != nil {
		startsAt = *start
	}
	endsAt := startsAt
	if end != nil {
		endsAt = *end
	}
	ev := remoteEvent{
		externalID: id,
		title:      "(no title)",
		notes:      stringField(raw, "description"),
		location:   stringField(raw, "location"),
		startsAt:   startsAt,
		endsAt:     endsAt,
		allDay:     startAllDay,
		cancelled:  status == "cancelled",
		htmlLink:   stringField(raw, "htmlLink"),
		etag:       stringField(raw, "etag"),
		updatedAt:  g.now(),
	}
	if s, ok := raw["summary"].(string); ok {
		ev.title = s
	}
	if org, ok := raw["organizer"].(map[string]any); ok {
		ev.organizer = stringField(org, "email")
	}
	for _, a := range objects(raw["attendees"]) {
		ev.attendees = append(ev.attendees, googleAttendee{
			Email:          stringField(a, "email"),
			DisplayName:    stringField(a, "displayName"),
			ResponseStatus: stringField(a, "responseStatus"),
		})
	}
	if rules, ok := raw["recurrence"].([]any); ok {
		ev.recurrence = stringList(rules)
	}
	if s, ok := raw["updated"].(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			ev.updatedAt = t
		}
	}
	return ev, true
}

// parseEndpoint reads `dateTime` (RFC 3339) or `date` (all day, UTC midnight).
func parseEndpoint(v any) (*time.Time, bool) {
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	if s, ok := raw["dateTime"].(string); ok {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, false
		}
		return &t, false
	}
	if s, ok := raw["date"].(string); ok {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return nil, true
		}
		return &t, true
	}
	return nil, false
}

func rfc3339(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// do runs req and reads the whole answer.
func do(client *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func stringField(m map[string]any, key string) *string {
	if s, ok := m[key].(string); ok {
		return &s
	}
	return nil
}

// objects is `as? [[String: Any]]`: one element of another type voids the cast.
func objects(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil
		}
		out = append(out, m)
	}
	return out
}

// stringList is `as? [String]`: one element of another type voids the cast.
func stringList(list []any) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

const (
	alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// urlQueryAllowed is CharacterSet.urlQueryAllowed. `&` and `=` are in it,
	// so a Google `error` value is not escaped against the query it lands in.
	urlQueryAllowed = alnum + "-._~!$&'()*+,;=:@/?"
	// queryItemAllowed is what URLComponents.queryItems leaves unescaped in a value.
	queryItemAllowed = alnum + "-._~!$'()*,;:@/?"
)

func percentEncode(s, allowed string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if strings.IndexByte(allowed, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// validatedReturn is GoogleCalendarOAuthService.validatedReturn: raw when it
// is an absolute http(s) URL on an allowed origin, without credentials or a
// fragment, so the callback can never redirect off-site.
func validatedReturn(raw string, allowedOrigins []string) (string, bool) {
	if swiftstr.Count(raw) > 2048 || strings.Contains(raw, "#") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if (scheme != "https" && scheme != "http") || host == "" || u.User != nil {
		return "", false
	}
	origin := scheme + "://" + host
	if strings.Contains(host, ":") {
		origin = scheme + "://[" + host + "]"
	}
	if p := u.Port(); p != "" {
		origin += ":" + p
	}
	for _, a := range allowedOrigins {
		if strings.Trim(strings.ToLower(a), "/ ") == origin {
			return raw, true
		}
	}
	return "", false
}

// redirectTo is GoogleCalendarOAuthService.redirect: base plus status and
// reason, keeping any query base already has.
func redirectTo(base, status string, reason *string) string {
	q := "status=" + percentEncode(status, urlQueryAllowed)
	if reason != nil {
		q += "&reason=" + percentEncode(*reason, urlQueryAllowed)
	}
	if strings.Contains(base, "?") {
		return base + "&" + q
	}
	return base + "?" + q
}
