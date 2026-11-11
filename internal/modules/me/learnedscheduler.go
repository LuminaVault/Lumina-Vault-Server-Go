package me

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/jobs"
	"lumina/internal/modules/me/db"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LearnedDigestScheduler is LearnedDigestScheduler (Services/LearnedDigestScheduler.swift):
// once a week, Sunday 18:00–18:59 on the user's own clock, a quiet push saying how
// many things Lumina learned. learned_digest_log (tenant_id, week_start) is the
// weekly cap, claimed with ON CONFLICT DO NOTHING, so two processes can't both send.
type LearnedDigestScheduler struct {
	Pool   *pgxpool.Pool
	Push   *apns.Service // nil sends nothing and still counts "sent", as Swift's nil push
	Logger *slog.Logger
}

const (
	learnedBudgetKind = "learned"
	learnedThreadID   = "learned"
	learnedAction     = "learned"
	learnedDeepLink   = "luminavault://learned"
	learnedTitle      = "What I learned this week"
)

// Job ticks every 60 s from boot.
func (s *LearnedDigestScheduler) Job() jobs.Job {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	return jobs.Job{Name: "LearnedDigestScheduler", Schedule: jobs.Every(time.Minute, 0), Run: func(ctx context.Context) error {
		sent, err := s.Tick(ctx, time.Now())
		if err != nil {
			log.WarnContext(ctx, "learned.digest tick error "+err.Error())
			return nil
		}
		if sent > 0 {
			log.InfoContext(ctx, "learned.digest tick sent="+strconv.Itoa(sent))
		}
		return nil
	}}
}

// LearnedPushBody is MeLearnedService.pushBody: counts only; "" when there is nothing.
func LearnedPushBody(soulChanges int, kinds []string) string {
	preferences, decisions := 0, 0
	for _, k := range kinds {
		switch k {
		case "preference":
			preferences++
		case "decision":
			decisions++
		}
	}
	aboutYou := soulChanges + preferences
	parts := []string{}
	if aboutYou > 0 {
		parts = append(parts, plural(aboutYou, "thing", "things")+" about you")
	}
	if decisions > 0 {
		parts = append(parts, plural(decisions, "decision", "decisions"))
	}
	if len(parts) == 0 {
		return ""
	}
	pronoun := "them"
	if aboutYou+decisions == 1 {
		pronoun = "it"
	}
	return strings.Join(parts, " and ") + " — keep, edit or forget " + pronoun + "."
}

// WeekStartIfSendHour is LearnedDigestScheduler.weekStartIfSendHour: the local
// Monday (yyyy-MM-dd) when now is Sunday 18:xx in timezone (UTC when unknown).
func WeekStartIfSendHour(now time.Time, timezone string) (string, bool) {
	loc, _ := zone(timezone)
	local := now.In(loc)
	if local.Weekday() != time.Sunday || local.Hour() != 18 {
		return "", false
	}
	monday := time.Date(local.Year(), local.Month(), local.Day()-6, 0, 0, 0, 0, loc)
	return monday.Format(time.DateOnly), true
}

// Tick is one pass at now; it returns how many digests were sent.
func (s *LearnedDigestScheduler) Tick(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT timezone FROM users`)
	if err != nil {
		return 0, err
	}
	zones, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	sent := 0
	for _, tz := range zones {
		weekStart, ok := WeekStartIfSendHour(now, tz)
		if !ok {
			continue
		}
		rows, err := s.Pool.Query(ctx, `SELECT u.id FROM users u
			WHERE u.timezone = $1
			  AND EXISTS (SELECT 1 FROM vaults v WHERE v.id = u.id)
			  AND NOT EXISTS (
			      SELECT 1 FROM learned_digest_log l
			      WHERE l.tenant_id = u.id AND l.week_start = $2::date
			  )`, tz, weekStart)
		if err != nil {
			return sent, err
		}
		candidates, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return sent, err
		}
		for _, id := range candidates {
			outcome, err := s.deliver(ctx, id, tz, weekStart, now)
			if err != nil {
				log.WarnContext(ctx, "learned.digest failed tenant="+strings.ToUpper(id.String())+": "+err.Error())
				continue
			}
			if outcome == "sent" {
				sent++
			}
		}
	}
	return sent, nil
}

func (s *LearnedDigestScheduler) deliver(ctx context.Context, tenant uuid.UUID, tz, weekStart string, now time.Time) (string, error) {
	q := db.New(s.Pool)
	since := now.Add(-learnedDefaultWindow)
	soul, err := q.LearnedSoulChanges(ctx, db.LearnedSoulChangesParams{TenantID: tenant, Since: since, Now: now})
	if err != nil {
		return "", err
	}
	mems, err := q.LearnedMemories(ctx, db.LearnedMemoriesParams{TenantID: tenant, Since: since, Now: now})
	if err != nil {
		return "", err
	}
	kinds := make([]string, 0, len(mems))
	for _, m := range mems {
		if m.Kind != nil {
			kinds = append(kinds, *m.Kind)
		}
	}
	body := LearnedPushBody(len(soul), kinds)
	if body == "" {
		_, err := s.claimWeek(ctx, tenant, weekStart, "empty", now)
		return "empty", err
	}
	if won, err := s.claimWeek(ctx, tenant, weekStart, "sent", now); err != nil || !won {
		return "", err
	}
	if off, err := apns.IsCategorySuppressed(ctx, s.Pool, apns.CategoryDigest, tenant); err != nil {
		return "", err
	} else if off {
		return "opted_out", s.record(ctx, tenant, weekStart, "opted_out")
	}
	if has, err := apns.HasDeviceToken(ctx, s.Pool, tenant); err != nil {
		return "", err
	} else if !has {
		return "no_device", s.record(ctx, tenant, weekStart, "no_device")
	}
	if won, err := apns.ClaimProactivePush(ctx, s.Pool, tenant, learnedBudgetKind, tz, now); err != nil {
		return "", err
	} else if !won {
		return "budget_spent", s.record(ctx, tenant, weekStart, "budget_spent")
	}
	if s.Push == nil {
		return "sent", nil
	}
	if err := s.Push.Notify(ctx, tenant, apns.Message{
		Title: learnedTitle, Body: body, Category: apns.CategoryDigest,
		Payload:           map[string]string{"deepLink": learnedDeepLink},
		InterruptionLevel: "passive", ThreadID: learnedThreadID, ActionCategory: learnedAction,
	}); err != nil {
		_ = s.record(ctx, tenant, weekStart, "failed")
		return "", err
	}
	return "sent", nil
}

func (s *LearnedDigestScheduler) claimWeek(ctx context.Context, tenant uuid.UUID, weekStart, outcome string, now time.Time) (bool, error) {
	var got string
	err := s.Pool.QueryRow(ctx, `INSERT INTO learned_digest_log (tenant_id, week_start, sent_at, outcome)
		VALUES ($1, $2::date, $3, $4)
		ON CONFLICT (tenant_id, week_start) DO NOTHING
		RETURNING outcome`, tenant, weekStart, now, outcome).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *LearnedDigestScheduler) record(ctx context.Context, tenant uuid.UUID, weekStart, outcome string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE learned_digest_log SET outcome = $3
		WHERE tenant_id = $1 AND week_start = $2::date`, tenant, weekStart, outcome)
	return err
}
