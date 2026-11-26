package githubsync

import (
	"context"
	"errors"
	"lumina/internal/githubapp"
	"strings"
	"time"
)

// zeroSHA is what GitHub sends as `after` when a branch was deleted.
const zeroSHA = "0000000000000000000000000000000000000000"

// Deliver handles one webhook delivery that was just recorded as queued
// (D9's table) and returns the status it leaves the row in. It runs after
// the response. A push that needs a pull stays queued: the pull closes it,
// and until then the sweep can queue it again.
//
// Nothing is decided by who sent the push (D12): a commit the vault's
// history already has is ignored, whoever pushed it.
func (s *Syncer) Deliver(ctx context.Context, d Delivery) string {
	status := s.deliver(ctx, d)
	if status != DeliveryQueued {
		if err := s.opts.Store.SetDeliveryStatus(context.WithoutCancel(ctx), d.ID, status, s.opts.Now()); err != nil {
			s.log.WarnContext(ctx, "github sync: closing a delivery failed", "delivery", d.ID, "error", err)
		}
	}
	return status
}

func (s *Syncer) deliver(ctx context.Context, d Delivery) string {
	now := s.opts.Now()
	failed := func(err error) string {
		s.log.WarnContext(ctx, "github sync: handling a delivery failed", "delivery", d.ID, "event", d.Event, "error", err)
		return DeliveryFailed
	}
	switch d.Event {
	case "push":
		if d.RepoID == 0 || !strings.HasPrefix(d.Ref, "refs/heads/") {
			return DeliveryIgnored
		}
		conn, err := s.opts.Store.ByRepo(ctx, d.RepoID)
		if err != nil {
			return failed(err)
		}
		if conn == nil || conn.State != StateActive || d.Ref != "refs/heads/"+conn.Branch || d.After == "" || d.After == zeroSHA {
			return DeliveryIgnored
		}
		if s.opts.Pull == nil || s.has(conn.VaultID, d.After) {
			return DeliveryIgnored // our own push coming back, or history we already merged
		}
		s.NotifyPull(conn.VaultID)
		return DeliveryQueued
	case "installation":
		if d.InstallationID == 0 {
			return DeliveryIgnored
		}
		switch d.Action {
		case "deleted", "suspend", "unsuspend":
		default:
			return DeliveryIgnored
		}
		// The delivery says what happened once; what is done is what GitHub
		// says is true now. A signed body can be sent again under a new
		// delivery id (the id is not part of what is signed), and a failed
		// delivery is handled again later, perhaps after a newer one: an old
		// "deleted" must not stop the connections of an App installed again,
		// nor an old "suspend" those of one since unsuspended.
		if err := s.settle(ctx, d.InstallationID, now); err != nil {
			return failed(err)
		}
		return DeliveryDone
	case "installation_repositories":
		if d.Action != "removed" || d.InstallationID == 0 {
			return DeliveryIgnored
		}
		if len(d.Removed) == 0 {
			// Handled again from its row, which does not hold the list (D2).
			// GitHub is asked instead, for every connection of the
			// installation: a token it will not mint for a repository says
			// the repository is gone from it. The cached token is dropped
			// first, or it would answer for GitHub.
			vaults, err := s.opts.Store.ByInstallation(ctx, d.InstallationID)
			if err != nil {
				return failed(err)
			}
			var gone []int64
			for _, v := range vaults {
				conn, err := s.opts.Store.Get(ctx, v)
				if err != nil {
					return failed(err)
				}
				if conn == nil || conn.InstallationID != d.InstallationID {
					continue
				}
				_ = s.opts.GitHub.RevokeInstallationToken(ctx, conn.InstallationID, conn.RepoID)
				_, err = s.opts.GitHub.InstallationToken(ctx, conn.InstallationID, conn.RepoID)
				switch {
				case err == nil:
				case errors.Is(err, githubapp.ErrRepoAccessRemoved):
					gone = append(gone, conn.RepoID)
				case errors.Is(err, githubapp.ErrNotFound):
					if err := s.settle(ctx, d.InstallationID, now); err != nil {
						return failed(err)
					}
					return DeliveryDone
				default:
					return failed(err)
				}
			}
			if len(gone) == 0 {
				return DeliveryDone
			}
			d.Removed = gone
		}
		if _, err := s.opts.Store.StopRepos(ctx, d.InstallationID, d.Removed, CodeRepoAccessRemoved, now); err != nil {
			return failed(err)
		}
		return DeliveryDone
	}
	return DeliveryIgnored
}

// settle makes the connections of an installation agree with what GitHub
// says of it now: gone, suspended, or neither.
func (s *Syncer) settle(ctx context.Context, installation int64, now time.Time) error {
	status, err := s.opts.GitHub.InstallationStatus(ctx, installation)
	switch {
	case errors.Is(err, githubapp.ErrNotFound):
		_, err = s.opts.Store.StopInstallation(ctx, installation, CodeInstallationRemoved, now)
		return err
	case err != nil:
		return err
	case status.Suspended:
		_, err = s.opts.Store.StopInstallation(ctx, installation, CodeInstallationSuspended, now)
		return err
	}
	vaults, err := s.opts.Store.ResumeInstallation(ctx, installation, CodeInstallationSuspended, now)
	if err != nil {
		return err
	}
	for _, v := range vaults {
		s.NotifyPull(v)
	}
	return nil
}

// redeliver handles again the deliveries that are not pushes and are still
// open a minute after they arrived (D9): queued ones whose handling a
// restart cut off, and failed ones. Oldest first; what each does is settled
// against GitHub's current answer, so the order decides nothing. One that
// fails again stays failed and is tried at the next sweep, until the purge
// takes its row.
func (s *Syncer) redeliver(ctx context.Context, before time.Time) error {
	events, err := s.opts.Store.StaleEvents(ctx, before)
	if err != nil {
		return err
	}
	for _, d := range events {
		if ctx.Err() != nil {
			break
		}
		s.Deliver(ctx, d)
	}
	return nil
}
