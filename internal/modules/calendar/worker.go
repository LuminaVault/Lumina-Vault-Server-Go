package calendar

import (
	"context"
	"sync"
)

// Configured reports that oauth.googleCalendar.clientId, clientSecret and redirectUri
// are all set, which is Swift's gate for CalendarSyncWorker.
func (g *Google) Configured() bool { return g.configured }

// SyncConnected is CalendarSyncWorker.tick (Sources/App/Workers/CalendarSyncWorker.swift):
// sync every connected account, four at a time, and return how many it processed.
// A tenant's sync error is dropped (Swift's `try?`); sync itself skips accounts
// without the Calendar scope or needing reauth. The worker claims nothing, so it
// must never run in both servers (jobs.md).
func (g *Google) SyncConnected(ctx context.Context, maxConcurrent int) (int, error) {
	tenants, err := g.q.ConnectedAccountTenants(ctx)
	if err != nil {
		return 0, err
	}
	maxConcurrent = max(1, maxConcurrent)
	processed := 0
	for start := 0; start < len(tenants); start += maxConcurrent {
		batch := tenants[start:min(start+maxConcurrent, len(tenants))]
		var wg sync.WaitGroup
		for _, tenant := range batch {
			wg.Go(func() { _, _ = g.sync.sync(ctx, tenant) })
		}
		wg.Wait()
		processed += len(batch)
	}
	return processed, nil
}
