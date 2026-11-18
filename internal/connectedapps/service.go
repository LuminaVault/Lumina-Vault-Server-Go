package connectedapps

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const toolCacheTTL = 5 * time.Minute

// ErrToolNotAllowed means the tool name is malformed, names an unknown app or
// is not on the app's read-only allowlist.
var ErrToolNotAllowed = errors.New("connectedapps: tool not allowed")

// ReconnectError tells the user an app needs to be connected again. It matches
// ErrNotConnected and carries only the app's display name.
type ReconnectError struct{ AppName string }

func (e *ReconnectError) Error() string { return "Reconnect " + e.AppName + " in Lumina Settings." }

func (e *ReconnectError) Is(target error) bool { return target == ErrNotConnected }

// AppTool is one allowlisted remote tool, named "<app>__<tool>".
type AppTool struct {
	App    App
	Name   string
	Remote RemoteTool
}

type cacheKey struct {
	user uuid.UUID
	app  string
}

type cacheEntry struct {
	tools   []RemoteTool
	expires time.Time
}

// Service is the per-user facade over the registry, store, OAuth and MCP
// client. It never logs tokens, arguments, results or remote error text.
type Service struct {
	reg    Registry
	store  *Store
	oauth  *OAuth
	client *Client
	log    *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
}

func NewService(reg Registry, store *Store, oauth *OAuth, client *Client, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{reg: reg, store: store, oauth: oauth, client: client, log: log,
		now: time.Now, cache: map[cacheKey]cacheEntry{}}
}

// SplitName splits "<app>__<tool>" on the first "__"; both parts must be non-empty.
func SplitName(name string) (appID, tool string, ok bool) {
	appID, tool, found := strings.Cut(name, "__")
	if !found || appID == "" || tool == "" {
		return "", "", false
	}
	return appID, tool, true
}

// Forget drops the cached tool list for a user's app (call on disconnect).
func (s *Service) Forget(user uuid.UUID, appID string) {
	s.mu.Lock()
	delete(s.cache, cacheKey{user, appID})
	s.mu.Unlock()
}

func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || err.Error() == "connectedapps: request timed out"
}

// callError is what Call hands back for failures that are not typed
// sentinels: fixed text with no remote content, wrapping the cause for
// errors.Is only.
type callError struct {
	msg   string
	cause error
}

func (e *callError) Error() string { return e.msg }
func (e *callError) Unwrap() error { return e.cause }

// sanitize keeps remote error text away from the caller (and so the model).
func sanitize(app App, err error) error {
	var re *ReconnectError
	switch {
	case err == nil, errors.As(err, &re),
		errors.Is(err, ErrNotConnected), errors.Is(err, ErrToolNotAllowed), errors.Is(err, ErrUnknownApp):
		return err
	case isTimeout(err):
		return &callError{app.Name + " timed out.", err}
	}
	return &callError{app.Name + " could not answer right now.", err}
}

// errKind maps an error to a fixed string that is safe to log.
func errKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrNotConnected):
		return "not_connected"
	case isTimeout(err):
		return "timeout"
	}
	return "error"
}

// withToken runs fn with the user's token for app. On a 401 an OAuth app gets
// one forced refresh and one retry; a PAT app gets none. A rejected retry (or
// a rejected PAT) marks the row needs_reauth and returns a ReconnectError.
func (s *Service) withToken(ctx context.Context, user uuid.UUID, app App, fn func(token string) error) error {
	reconnect := &ReconnectError{AppName: app.Name}
	acct, err := s.store.Get(ctx, user, app.ID)
	if err != nil {
		return err
	}
	if acct.Status != "connected" {
		return reconnect
	}
	token := acct.AccessToken
	if app.Auth == AuthOAuth {
		if token, err = s.oauth.Fresh(ctx, user, app.ID); err != nil {
			if errors.Is(err, ErrNotConnected) {
				return reconnect
			}
			return err
		}
	}
	err = fn(token)
	if !errors.Is(err, ErrUnauthorized) {
		return err
	}
	if app.Auth == AuthOAuth {
		next, rerr := s.oauth.Refresh(ctx, user, app.ID, token)
		if rerr != nil {
			if errors.Is(rerr, ErrNotConnected) {
				return reconnect
			}
			return rerr
		}
		if err = fn(next); !errors.Is(err, ErrUnauthorized) {
			return err
		}
	}
	_ = s.store.MarkNeedsReauth(ctx, user, app.ID, "unauthorized")
	s.Forget(user, app.ID)
	return reconnect
}

// Call runs one allowlisted tool as the user. Every check happens before any
// outbound request.
func (s *Service) Call(ctx context.Context, user uuid.UUID, name string, args map[string]any) (out string, err error) {
	start := s.now()
	var logApp, logTool string
	defer func() {
		attrs := []any{"app", logApp, "tool", logTool, "ms", s.now().Sub(start).Milliseconds(), "ok", err == nil}
		if err != nil {
			attrs = append(attrs, "err", errKind(err))
		}
		s.log.Info("connected_app_call", attrs...)
	}()

	appID, tool, ok := SplitName(name)
	if !ok {
		return "", ErrToolNotAllowed
	}
	app, ok := s.reg.Get(appID)
	if !ok || !slices.Contains(app.ReadTools, tool) {
		return "", ErrToolNotAllowed
	}
	logApp, logTool = app.ID, tool
	err = s.withToken(ctx, user, app, func(token string) error {
		var cerr error
		out, cerr = s.client.CallTool(ctx, app.MCPURL, token, tool, args)
		return cerr
	})
	if err != nil {
		return "", sanitize(app, err)
	}
	return out, nil
}

// Tools lists the allowlisted tools of the user's connected apps, sorted by
// app then tool. onlyApps limits the apps when non-nil. An app that fails is
// skipped.
func (s *Service) Tools(ctx context.Context, user uuid.UUID, onlyApps []string) ([]AppTool, error) {
	rows, err := s.store.List(ctx, user)
	if err != nil {
		return nil, err
	}
	var apps []App
	for _, r := range rows {
		if r.Status != "connected" || (onlyApps != nil && !slices.Contains(onlyApps, r.AppID)) {
			continue
		}
		if app, ok := s.reg.Get(r.AppID); ok {
			apps = append(apps, app)
		}
	}
	results := make([][]AppTool, len(apps))
	var wg sync.WaitGroup
	for i, app := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote, err := s.remoteTools(ctx, user, app)
			if err != nil {
				s.log.Info("connected_app_tools_skipped", "app", app.ID, "err", errKind(err))
				return
			}
			for _, rt := range remote {
				if slices.Contains(app.ReadTools, rt.Name) {
					results[i] = append(results[i], AppTool{App: app, Name: app.ID + "__" + rt.Name, Remote: rt})
				}
			}
		}()
	}
	wg.Wait()
	var out []AppTool
	for _, r := range results {
		out = append(out, r...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App.ID != out[j].App.ID {
			return out[i].App.ID < out[j].App.ID
		}
		return out[i].Remote.Name < out[j].Remote.Name
	})
	return out, nil
}

func (s *Service) remoteTools(ctx context.Context, user uuid.UUID, app App) ([]RemoteTool, error) {
	key := cacheKey{user, app.ID}
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && s.now().Before(e.expires) {
		s.mu.Unlock()
		return e.tools, nil
	}
	s.mu.Unlock()
	var tools []RemoteTool
	err := s.withToken(ctx, user, app, func(token string) error {
		var lerr error
		tools, lerr = s.client.ListTools(ctx, app.MCPURL, token)
		return lerr
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[key] = cacheEntry{tools: tools, expires: s.now().Add(toolCacheTTL)}
	s.mu.Unlock()
	return tools, nil
}
