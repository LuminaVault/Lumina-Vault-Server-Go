package scenario

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPeerStepsGoToThePeer: `on: peer` reaches Target.Peer, carrying captured
// vars, and falls back to the target itself when no peer is set (self-tests).
func TestPeerStepsGoToThePeer(t *testing.T) {
	var (
		mu   sync.Mutex
		hits []string
	)
	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits = append(hits, name+" "+r.URL.Path+" "+r.Header.Get("X-Token"))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"token":"t1"}`)
		}
	}
	main, peer := serve(t, record("main")), serve(t, record("peer"))
	sc := scenarioOf(
		Step{Name: "a", Request: Request{Method: http.MethodGet, Path: "/a"}, Capture: map[string]string{"tok": "$.token"}},
		Step{Name: "b", On: "peer", Request: Request{Method: http.MethodGet, Path: "/b", Headers: map[string]string{"X-Token": "{{tok}}"}}},
	)
	tg := target(main.URL)
	tg.Peer = peer.URL
	_, err := Run(context.Background(), sc, tg)
	require.NoError(t, err)
	require.Equal(t, []string{"main /a ", "peer /b t1"}, hits)

	hits = nil
	_, err = Run(context.Background(), sc, target(main.URL))
	require.NoError(t, err)
	require.Equal(t, []string{"main /a ", "main /b t1"}, hits)

	bad := scenarioOf(Step{Name: "x", On: "other", Request: Request{Method: http.MethodGet, Path: "/"}})
	require.Error(t, bad.validate())
}
