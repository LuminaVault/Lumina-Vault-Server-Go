package skills

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/cronexpr"
	"lumina/internal/ids"
	"lumina/internal/jobs"
	"lumina/internal/skills"
	"sync"
	"time"

	"github.com/google/uuid"
)

// CronTick and CronMaxConcurrent are CronScheduler's tickInterval and maxConcurrent.
const (
	CronTick          = 60 * time.Second
	CronMaxConcurrent = 4
)

// duePair is CronScheduler.DuePair: users joined with skills_state and the
// catalog's manifest.
type duePair struct {
	tenant             uuid.UUID
	username, timezone string
	tier               string
	manifest           skills.Manifest
	schedule           *string
	runAt, lastRunAt   *time.Time
	apnsCategory       *string
	source, name       string
}

// CronJob is CronScheduler (Skills/CronScheduler.swift): once a minute, every
// (user, enabled skill) whose cron, read in the user's timezone, matches this
// minute, or whose one-shot run_at has come, runs with trigger .cron, at most four
// at a time. last_run_at advances even on failure (next-occurrence semantics, no
// catch-up), a fired one-shot is disabled, and a successful run of a skill with an
// APNs category sends notifyCron. Single process only: two would double-fire,
// so it moves at Swift replicas 0.
func (m *Module) CronJob(log *slog.Logger) jobs.Job {
	return jobs.Job{
		Name:     "CronScheduler",
		Schedule: jobs.Every(CronTick, 0),
		Run: func(ctx context.Context) error {
			now := m.now()
			n, err := m.CronTick(ctx, now, log)
			if err != nil {
				log.Warn(fmt.Sprintf("skills.cron.tick error %v", err))
				return err
			}
			if n > 0 {
				log.Info(fmt.Sprintf("skills.cron.tick dispatched=%d at=%s", n, now.UTC().Format("2006-01-02 15:04:05 +0000")))
			}
			return nil
		},
	}
}

// CronTick is CronScheduler.tick(at:): the number of skills dispatched.
func (m *Module) CronTick(ctx context.Context, now time.Time, log *slog.Logger) (int, error) {
	if m.opt.Catalog == nil || m.opt.Runner == nil {
		return 0, nil
	}
	pairs, err := m.enabledPairs(ctx)
	if err != nil {
		return 0, err
	}
	var due []duePair
	for _, p := range pairs {
		// A one-shot takes precedence over any cron: it fires once its instant has
		// come, and a last_run_at at or after run_at guards a same-tick re-fire.
		if p.runAt != nil {
			if now.Before(*p.runAt) || (p.lastRunAt != nil && !p.lastRunAt.Before(*p.runAt)) {
				continue
			}
			due = append(due, p)
			continue
		}
		if p.schedule == nil {
			continue
		}
		expr, ok := cronexpr.Parse(*p.schedule)
		if !ok {
			log.Warn(fmt.Sprintf("skills.cron skipping bad schedule %s tenant=%s", *p.schedule, ids.Upper(p.tenant)))
			continue
		}
		zone := userZone(p.timezone)
		if !expr.Matches(now, zone) {
			continue
		}
		if p.lastRunAt != nil && sameMinute(*p.lastRunAt, now, zone) {
			continue
		}
		due = append(due, p)
	}
	if len(due) == 0 {
		return 0, nil
	}
	sem := make(chan struct{}, CronMaxConcurrent)
	var wg sync.WaitGroup
	for _, p := range due {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			m.dispatchCron(ctx, p, now, log)
		})
	}
	wg.Wait()
	return len(due), nil
}

// enabledPairs is loadEnabledPairs. As in Swift, only enabled skills_state rows are
// read, so a skill whose row is disabled is treated like one with no row: enabled,
// on its manifest schedule.
func (m *Module) enabledPairs(ctx context.Context) ([]duePair, error) {
	type user struct {
		id                       uuid.UUID
		username, timezone, tier string
	}
	rows, err := m.pool.Query(ctx, `SELECT id, username, timezone, tier FROM users`)
	if err != nil {
		// Swift's loader answers no pairs when the users query fails.
		return nil, nil //nolint:nilerr // parity: a failed users read is an empty tick
	}
	var users []user
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.id, &u.username, &u.timezone, &u.tier); err != nil {
			rows.Close()
			return nil, nil //nolint:nilerr // as above
		}
		users = append(users, u)
	}
	rows.Close()

	type state struct {
		scheduleOverride, apnsCategory *string
		runAt, lastRunAt               *time.Time
		workflowID                     *uuid.UUID
	}
	var pairs []duePair
	for _, u := range users {
		manifests := m.opt.Catalog.Manifests(u.id)
		if len(manifests) == 0 {
			continue
		}
		srows, err := m.pool.Query(ctx, `
			SELECT source, name, schedule_override, run_at, last_run_at, apns_category, workflow_id
			FROM skills_state WHERE tenant_id = $1 AND enabled = true`, u.id)
		if err != nil {
			return nil, err
		}
		byKey := map[string]state{}
		for srows.Next() {
			var source, name string
			var st state
			if err := srows.Scan(&source, &name, &st.scheduleOverride, &st.runAt, &st.lastRunAt, &st.apnsCategory, &st.workflowID); err != nil {
				srows.Close()
				return nil, err
			}
			byKey[source+":"+name] = st
		}
		srows.Close()
		if err := srows.Err(); err != nil {
			return nil, err
		}
		for _, mf := range manifests {
			st, hasRow := byKey[string(mf.Source)+":"+mf.Name]
			// A legacy job linked to a workflow is WorkflowScheduler's to fire.
			if hasRow && st.workflowID != nil {
				continue
			}
			schedule := mf.Schedule
			if hasRow && st.scheduleOverride != nil {
				schedule = st.scheduleOverride
			}
			if schedule == nil && st.runAt == nil {
				continue
			}
			pairs = append(pairs, duePair{
				tenant: u.id, username: u.username, timezone: u.timezone, tier: u.tier, manifest: mf,
				schedule: schedule, runAt: st.runAt, lastRunAt: st.lastRunAt, apnsCategory: st.apnsCategory,
				source: string(mf.Source), name: mf.Name,
			})
		}
	}
	return pairs, nil
}

// dispatchCron is dispatch(pair:now:). Nothing here fails the tick.
func (m *Module) dispatchCron(ctx context.Context, p duePair, now time.Time, log *slog.Logger) {
	label := fmt.Sprintf("skill=%s tenant=%s", p.name, ids.Upper(p.tenant))
	status := "ok"
	var errText *string
	if _, err := m.opt.Runner.Run(ctx, p.manifest,
		runUser{tenant: p.tenant, tier: p.tier, username: p.username, trigger: "cron"}, nil, map[string]string{}); err != nil {
		status = "error"
		e := err.Error()
		errText = &e
		log.Warn(fmt.Sprintf("skills.cron %s failed: %v", label, err))
	}
	if _, err := m.pool.Exec(ctx, `
		INSERT INTO skills_state (tenant_id, source, name, enabled, last_run_at, last_status, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, source, name) DO UPDATE SET
			last_run_at = EXCLUDED.last_run_at, last_status = EXCLUDED.last_status,
			last_error = EXCLUDED.last_error,
			enabled = CASE WHEN $8 THEN false ELSE skills_state.enabled END`,
		p.tenant, p.source, p.name, p.runAt == nil, now, status, errText, p.runAt != nil); err != nil {
		log.Warn(fmt.Sprintf("skills.cron %s state persist failed: %v", label, err))
	}
	if status == "ok" && p.apnsCategory != nil && m.opt.Runner.push != nil {
		if err := m.opt.Runner.push.NotifyCron(ctx, p.tenant, p.name, "Your "+p.name+" job just ran."); err != nil {
			log.Warn(fmt.Sprintf("skills.cron %s push failed: %v", label, err))
		}
	}
}

// userZone is TimeZone(identifier:) ?? .gmt.
func userZone(identifier string) *time.Location {
	if z, err := time.LoadLocation(identifier); err == nil && identifier != "" && identifier != "Local" {
		return z
	}
	return time.UTC
}

// sameMinute compares year through minute on the user's wall clock.
func sameMinute(a, b time.Time, zone *time.Location) bool {
	const minute = "2006-01-02T15:04"
	return a.In(zone).Format(minute) == b.In(zone).Format(minute)
}
