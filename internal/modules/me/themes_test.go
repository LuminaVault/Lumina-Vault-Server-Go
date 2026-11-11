package me_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/llm"
	"lumina/internal/modules/me"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The themes worker and generator of Me/PeriodRecapLogicTests.swift and
// Me/PeriodRecapControllerTests.swift, against a stub transport (never a real key).

type stubRecapTransport struct {
	mu      sync.Mutex
	calls   int
	content string
	fail    bool
	hang    bool
	payload []byte
}

func (s *stubRecapTransport) ChatCompletions(ctx context.Context, payload []byte, _, _ string) (llm.Metadata, error) {
	s.mu.Lock()
	s.calls++
	s.payload = payload
	s.mu.Unlock()
	switch {
	case s.hang:
		<-ctx.Done()
		return llm.Metadata{}, ctx.Err()
	case s.fail:
		return llm.Metadata{}, errors.New("upstream 500")
	}
	return llm.Metadata{Data: completion(s.content)}, nil
}

func (s *stubRecapTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(func(llm.Chunk, error) bool) {}
}

func completion(content string) []byte {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}}})
	return b
}

func strp(s string) *string { return &s }

var themesInput = me.ThemesInput{
	PeriodName: "2026", Kind: "year", Memories: 160, Asked: 80,
	TopTags: []me.RecapCountDTO{{Label: "swift", Count: 30}}, TopSpace: strp("Work"),
	Persona: strp("nightOwl"), Busiest: strp("2026-03"), Snippets: []string{"Ship on Tuesdays"},
}

func TestThemesPayloadSendsNoSamplingKnobsAndEndsOnTheUsersTurn(t *testing.T) {
	data, err := me.ThemesPayload("m", themesInput)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(data, &body))
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		require.NotContains(t, body, k)
	}
	require.InDelta(t, 400.0, body["max_tokens"], 0)
	require.Equal(t, map[string]any{"type": "json_object"}, body["response_format"])
	msgs, _ := body["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	require.Equal(t, "user", last["role"])
	user, _ := last["content"].(string)
	require.Contains(t, user, "swift (30)")
	require.Contains(t, user, "Ship on Tuesdays")
	require.Contains(t, user, "Rhythm: nightOwl")
}

func TestThemesAGoodReplyIsTrimmedAndCapped(t *testing.T) {
	long := strings.Repeat("a", 300)
	w := me.ParseThemes(completion(`{"headline":"  Night owl coder  ","sentence":"` + long + `","themes":["swift","running","coffee","extra"]}`))
	require.NotNil(t, w)
	require.Equal(t, "Night owl coder", w.Headline)
	require.Len(t, []rune(w.Sentence), 200)
	require.Equal(t, []string{"swift", "running", "coffee"}, w.Themes)
}

func TestThemesAMalformedOrEmptyReplyIsNil(t *testing.T) {
	require.Nil(t, me.ParseThemes(completion("not json")))
	require.Nil(t, me.ParseThemes(completion(`{"headline":"","sentence":"s"}`)))
	require.Nil(t, me.ParseThemes([]byte("{}")))
}

func TestThemesTheGeneratorWritesAndFallsBackOnAnErrorOrATimeout(t *testing.T) {
	pool := startDB(t)
	v := newEnv(t, pool, me.Options{RecapThemesEnabled: true})
	u := v.user("UTC")
	run := func(tr *stubRecapTransport, timeout time.Duration) (string, *string) {
		t.Helper()
		key := "2026-0" + string(rune('1'+tr.calls))
		v.exec(`INSERT INTO recap_themes (user_id, period_key, status, attempts, requested_at) VALUES ($1, $2, 'pending', 1, now())
			ON CONFLICT (user_id, period_key) DO UPDATE SET status = 'pending', headline = NULL`, u, key)
		w := &me.ThemesWorker{Pool: pool, Transport: tr, Model: "test-model", Timeout: timeout}
		w.Process(context.Background(), me.ThemesJob{UserID: u, PeriodKey: key, Start: time.Now().Add(-time.Hour), End: time.Now(), Input: themesInput})
		var status string
		var headline *string
		require.NoError(t, pool.QueryRow(context.Background(), `SELECT status, headline FROM recap_themes WHERE user_id = $1 AND period_key = $2`, u, key).Scan(&status, &headline))
		return status, headline
	}
	good := &stubRecapTransport{content: `{"headline":"H","sentence":"S","themes":[]}`}
	status, headline := run(good, 0)
	require.Equal(t, "ready", status)
	require.Equal(t, "H", *headline)
	require.Equal(t, 1, good.calls)

	status, _ = run(&stubRecapTransport{fail: true}, 0)
	require.Equal(t, "fallback", status)
	status, _ = run(&stubRecapTransport{hang: true}, 200*time.Millisecond)
	require.Equal(t, "fallback", status)
}

// `the worker writes the themes the next view shows, and a failed write falls back`,
// through the route's claim and the worker's inbox.
func TestThemesTheWorkerWritesTheThemesTheNextViewShows(t *testing.T) {
	pool := startDB(t)
	tr := &stubRecapTransport{content: `{"headline":"Early bird builder","sentence":"You wrote about Swift before breakfast.","themes":["swift"]}`}
	worker := &me.ThemesWorker{Pool: pool, Transport: tr, Model: "test"}
	v := newEnv(t, pool, me.Options{RecapThemesEnabled: true, Themes: worker})
	u := v.user("UTC")
	seedMarch(t, v, u)
	const path = "/v1/me/recap/period?period=month&year=2026&month=3"
	first := decode[me.PeriodRecapResponse](t, v.get(u, path))
	require.Equal(t, me.RecapThemesStatus("pending"), first.Themes.Status)

	worker.Drain(context.Background())
	require.Equal(t, 1, tr.calls)
	var sent struct {
		Messages []llm.ChatMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(tr.payload, &sent))
	user := sent.Messages[1].Content
	for _, want := range []string{
		"month in review: March 2026.", "Memories saved: 11", "Questions asked: 2", "swift (9)",
		"Busiest Space: Work", "Busiest stretch: 2026-03-03", "Rhythm: earlyBird", "- Note",
	} {
		require.Contains(t, user, want)
	}

	r := v.get(u, path)
	themes := decode[me.PeriodRecapResponse](t, r).Themes
	require.Equal(t, me.RecapThemesStatus("ready"), themes.Status)
	require.Equal(t, "Early bird builder", *themes.Headline)
	require.Equal(t, []string{"swift"}, themes.Themes)
	require.Equal(t, "private, max-age=3600", r.header.Get("Cache-Control"))

	// A different period whose write fails shows the counted sentence.
	tr.fail = true
	feb := "/v1/me/recap/period?period=month&year=2026&month=2"
	decode[me.PeriodRecapResponse](t, v.get(u, feb))
	worker.Drain(context.Background())
	again := decode[me.PeriodRecapResponse](t, v.get(u, feb))
	if again.Themes != nil {
		require.Equal(t, me.RecapThemesStatus("fallback"), again.Themes.Status)
	}
}
