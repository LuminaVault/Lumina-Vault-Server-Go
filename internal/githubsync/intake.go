package githubsync

import (
	"context"
	"strings"
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
		case "deleted":
			if _, err := s.opts.Store.StopInstallation(ctx, d.InstallationID, CodeInstallationRemoved, now); err != nil {
				return failed(err)
			}
		case "suspend":
			if _, err := s.opts.Store.StopInstallation(ctx, d.InstallationID, CodeInstallationSuspended, now); err != nil {
				return failed(err)
			}
		case "unsuspend":
			vaults, err := s.opts.Store.ResumeInstallation(ctx, d.InstallationID, CodeInstallationSuspended, now)
			if err != nil {
				return failed(err)
			}
			for _, v := range vaults {
				s.NotifyPull(v)
			}
		default:
			return DeliveryIgnored
		}
		return DeliveryDone
	case "installation_repositories":
		if d.Action != "removed" || d.InstallationID == 0 || len(d.Removed) == 0 {
			return DeliveryIgnored
		}
		if _, err := s.opts.Store.StopRepos(ctx, d.InstallationID, d.Removed, CodeRepoAccessRemoved, now); err != nil {
			return failed(err)
		}
		return DeliveryDone
	}
	return DeliveryIgnored
}
