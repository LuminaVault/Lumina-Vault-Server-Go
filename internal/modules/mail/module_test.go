package mail_test

import (
	"lumina/internal/config"
	"lumina/internal/httpserver"
	"lumina/internal/modules/mail"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The routes run against Postgres and a stub Google in the calendar package's
// tests: Mail is a thin view over its Google grant.
func TestWithoutGoogleNoRouteExists(t *testing.T) {
	e := httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{mail.New(nil, nil)}})
	for _, route := range [][2]string{
		{http.MethodGet, "/v1/mail/gmail/status"},
		{http.MethodPost, "/v1/mail/gmail/connect"},
		{http.MethodPost, "/v1/mail/gmail/disconnect"},
	} {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(route[0], route[1], nil))
		require.Equal(t, http.StatusNotFound, rec.Code, route[1])
	}
}
