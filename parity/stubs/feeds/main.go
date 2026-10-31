// Command feeds is a deterministic stand-in for the cluster's feed aggregator
// (platform/infra docs/feeds.md) in the parity environment. Swift and Go both
// point FEEDS_BASE_URL at it, so they see byte-identical upstream answers.
//
// It serves GET /v1/items?feeds=<comma-separated feed URLs>&limit=N in the
// shape NewsTickerService decodes (JSON Feed 1.1 plus the `_feeds`
// extension). Every feed yields three fixed items, so the response shows
// which feeds a server asked for, in what order, and with what limit.
//
// A feed URL whose path contains one of these words changes the answer:
//
//	/error       503 (Swift: 502 feeds_unavailable)
//	/notfound    404 (also 502)
//	/malformed   200 with a body that is not JSON
//	/missingkey  200 with an item missing its required `title`
//	/hangup      closes the connection without a response
//	/stale       the feed is listed in `_feeds.stale`
//	/noext       no top-level `_feeds` (stale false, generatedAt = now)
//	/nogenerated `_feeds` without `generated_at` (generatedAt = now)
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// base is the newest item's publish time; later items are a minute older each.
var base = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

type ext struct {
	SourceName string  `json:"source_name"`
	SourceURL  *string `json:"source_url,omitempty"`
	FeedURL    string  `json:"feed_url"`
}

type item struct {
	ID            string  `json:"id"`
	URL           *string `json:"url"`
	Title         string  `json:"title,omitempty"`
	DatePublished string  `json:"date_published"`
	Feeds         ext     `json:"_feeds"`
}

type docExt struct {
	Warming     []string `json:"warming"`
	Stale       []string `json:"stale"`
	GeneratedAt string   `json:"generated_at,omitempty"`
}

type doc struct {
	Version string  `json:"version"`
	Title   string  `json:"title"`
	Items   []item  `json:"items"`
	Feeds   *docExt `json:"_feeds,omitempty"`
}

func has(feeds []string, word string) bool {
	for _, f := range feeds {
		if strings.Contains(f, word) {
			return true
		}
	}
	return false
}

func items(feeds []string) []item {
	var out []item
	for i, f := range feeds {
		host := f
		if u, err := url.Parse(f); err == nil && u.Host != "" {
			host = u.Host
		}
		for n := 1; n <= 3; n++ {
			published := base.Add(-time.Duration(i*3+n-1) * time.Minute)
			date := published.Format(time.RFC3339)
			if n == 3 {
				date = published.Add(250 * time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00")
			}
			it := item{
				ID:            fmt.Sprintf("%s#%d", f, n),
				Title:         fmt.Sprintf("Headline %d from %s", n, f),
				DatePublished: date,
				Feeds:         ext{SourceName: host, FeedURL: f},
			}
			if n != 2 {
				link := fmt.Sprintf("https://%s/story-%d", host, n)
				it.URL = &link // n == 2 sends "url": null
			}
			if n != 3 {
				src := "https://" + host
				it.Feeds.SourceURL = &src // n == 3 omits source_url
			}
			out = append(out, it)
		}
	}
	return out
}

func handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var feeds []string
	if raw := q.Get("feeds"); raw != "" {
		feeds = strings.Split(raw, ",")
	}
	log.Printf("feeds=%q limit=%q", feeds, q.Get("limit")) //nolint:gosec // %q escapes; parity-only stub
	switch {
	case has(feeds, "/hangup"):
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	case has(feeds, "/error"):
		http.Error(w, "upstream down", http.StatusServiceUnavailable)
		return
	case has(feeds, "/notfound"):
		http.NotFound(w, r)
		return
	case has(feeds, "/malformed"):
		w.Header().Set("Content-Type", "application/feed+json")
		_, _ = w.Write([]byte(`{"items": [`))
		return
	}
	all := items(feeds)
	if has(feeds, "/missingkey") {
		all[0].Title = ""
	}
	if limit, err := strconv.Atoi(q.Get("limit")); err == nil && limit >= 0 && limit < len(all) {
		all = all[:limit]
	}
	d := doc{Version: "https://jsonfeed.org/version/1.1", Title: "Parity timeline", Items: all}
	if d.Items == nil {
		d.Items = []item{}
	}
	if !has(feeds, "/noext") {
		e := &docExt{Warming: []string{}, Stale: []string{}, GeneratedAt: "2026-10-30T12:00:00Z"}
		for _, f := range feeds {
			if strings.Contains(f, "/stale") {
				e.Stale = append(e.Stale, f)
			}
		}
		if has(feeds, "/nogenerated") {
			e.GeneratedAt = ""
		}
		d.Feeds = e
	}
	w.Header().Set("Content-Type", "application/feed+json")
	_ = json.NewEncoder(w).Encode(d)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/items", handle)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
