package connectedapps

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SetNow swaps the clock for tests in the external test package.
func (s *Service) SetNow(f func() time.Time) { s.now = f }

// Pool is the OAuth flow's database pool, for tests that break a table.
func (o *OAuth) Pool() *pgxpool.Pool { return o.pool }
