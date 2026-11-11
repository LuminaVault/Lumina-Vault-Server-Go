package main

import "net/http"

// pluginFeed is the RSS/Atom document the rss plugin's sync reads (6F): two RSS
// item links (one repeated), an Atom href, and a link that is not http.
const pluginFeed = `<?xml version="1.0"?>
<rss version="2.0"><channel>
  <title>Parity feed</title>
  <item><title>A</title><link>http://feeds:8080/capture/article</link></item>
  <item><title>A again</title><link>http://feeds:8080/capture/article</link></item>
  <item><title>B</title><link> http://feeds:8080/capture/plain </link></item>
  <entry><link rel="alternate" href="http://feeds:8080/capture/entities"/></entry>
  <item><title>C</title><link>ftp://feeds/nope</link></item>
</channel></rss>`

func registerPlugins(mux *http.ServeMux) {
	mux.HandleFunc("GET /plugins/feed.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(pluginFeed))
	})
	mux.HandleFunc("GET /plugins/broken.xml", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	})
}
