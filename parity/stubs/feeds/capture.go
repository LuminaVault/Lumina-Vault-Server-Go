package main

import (
	"net/http"
	"time"
)

// Pages for capture's enrichment, reached at http://feeds:8080/capture/*. The
// generic enricher fetches them for real from both servers. r.jina.ai is blocked in
// docker-compose.parity.yml, so its tier-2 step fails the same way on both.

const articlePage = `<!doctype html><html><head>
<title>Ignored for og:title</title>
<meta property="og:title" content="Parity &quot;Article&quot; &amp; more">
<meta content="An article about parity." property="og:description">
<meta property='og:image' content='https://img.parity.test/a.png'>
<meta name="x" property="og:site_name" content="Parity Site">
</head><body>Body text is not read.</body></html>`

// captureSlowDelay holds a page long enough for a scenario to read the placeholder
// before enrichment rewrites it. Swift's fetch timeout is 10 s.
const captureSlowDelay = 6 * time.Second

func html(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func registerCapture(mux *http.ServeMux) {
	mux.HandleFunc("GET /capture/article", func(w http.ResponseWriter, _ *http.Request) { html(w, http.StatusOK, articlePage) })
	mux.HandleFunc("GET /capture/plain", func(w http.ResponseWriter, _ *http.Request) {
		html(w, http.StatusOK, "<html><head><TITLE lang=\"en\">\n  Plain &#39;title&#39;\n</TITLE>\n<meta content=\"Plain description\" name=\"description\"></head></html>")
	})
	mux.HandleFunc("GET /capture/entities", func(w http.ResponseWriter, _ *http.Request) {
		html(w, http.StatusOK, `<meta property="og:title" content="A &amp;lt;b&amp;gt; &lt;i&gt; &#39;q&#39; &copy;"><meta property="og:description" content="  it&#39;s  ">`)
	})
	mux.HandleFunc("GET /capture/latin1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<title>caf\xe9 latin</title>"))
	})
	mux.HandleFunc("GET /capture/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(captureSlowDelay):
		case <-r.Context().Done():
			return
		}
		html(w, http.StatusOK, articlePage)
	})
	mux.HandleFunc("GET /capture/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/capture/article", http.StatusFound)
	})
	// The enrichers don't read the status, so an error page's tags count.
	mux.HandleFunc("GET /capture/error-page", func(w http.ResponseWriter, _ *http.Request) {
		html(w, http.StatusInternalServerError, `<meta property="og:title" content="Error page title">`)
	})
	mux.HandleFunc("GET /capture/empty", func(w http.ResponseWriter, _ *http.Request) { html(w, http.StatusOK, "<p>no tags</p>") })
}
