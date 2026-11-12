#!/usr/bin/env bash
# HER-131 — shared helpers for the encrypted backup + restore scripts.
#
# Sourced by scripts/backup.sh and scripts/restore.sh. Holds nothing that
# runs on its own; just logging, env validation, remote-path normalization,
# and tiered retention pruning. Keep this POSIX-ish but bash is assumed
# (mapfile / arrays) — the backup image installs bash.

# --- Logging -----------------------------------------------------------------
# Timestamped lines to stderr and, when writable, to $BACKUP_LOG_DIR/backup.log.
# stderr (not stdout) so a script can still pipe structured data on stdout.
BACKUP_LOG_DIR="${BACKUP_LOG_DIR:-/var/log/backup}"

log() {
  local line
  line="$(date -u +'%Y-%m-%dT%H:%M:%SZ') $*"
  echo "${line}" >&2
  if mkdir -p "${BACKUP_LOG_DIR}" 2>/dev/null && [ -w "${BACKUP_LOG_DIR}" ]; then
    echo "${line}" >>"${BACKUP_LOG_DIR}/backup.log" 2>/dev/null || true
  fi
}

die() {
  log "FATAL: $*"
  exit 1
}

# --- Alerting ----------------------------------------------------------------
# notify <status> <message>
# Logs always; POSTs a Slack-compatible JSON payload when BACKUP_ALERT_WEBHOOK
# is set. Best-effort — a failed webhook never masks the original exit status.
notify() {
  local status="$1" msg="$2"
  log "${status}: ${msg}"
  [ -n "${BACKUP_ALERT_WEBHOOK:-}" ] || return 0
  command -v curl >/dev/null 2>&1 || { log "WARN: curl missing — cannot send alert"; return 0; }
  local host; host="$(hostname 2>/dev/null || echo unknown)"
  curl -fsS -m 10 -X POST -H 'Content-type: application/json' \
    --data "{\"text\":\"[LuminaVault backup @ ${host}] ${status}: ${msg}\"}" \
    "${BACKUP_ALERT_WEBHOOK}" >/dev/null 2>&1 \
    || log "WARN: alert webhook POST failed"
}

# --- Env validation ----------------------------------------------------------
# Both scripts need a remote + an age recipient (backup) or identity (restore);
# the recipient check lives here, the identity check stays in restore.sh.
require_env() {
  [ -n "${BACKUP_RCLONE_REMOTE:-}" ] || die "BACKUP_RCLONE_REMOTE is unset (e.g. b2:my-bucket/luminavault)"
  [ -n "${BACKUP_AGE_RECIPIENT:-}" ] || die "BACKUP_AGE_RECIPIENT is unset (age recipient public key, age1...)"
  command -v rclone >/dev/null 2>&1 || die "rclone not found on PATH"
  command -v age >/dev/null 2>&1 || die "age not found on PATH"
}

# --- Remote path -------------------------------------------------------------
# Echo the configured remote base with any trailing slash stripped so callers
# can append "/daily/<date>" cleanly.
rclone_remote() {
  printf '%s' "${BACKUP_RCLONE_REMOTE%/}"
}

# --- Retention ---------------------------------------------------------------
# prune_tier <tier> <keep>
# Lists the dated snapshot dirs under <remote>/<tier>/, sorts newest-first by
# name (ISO dates sort lexicographically == chronologically), and purges every
# dir past the keep count. No-ops cleanly when the tier does not exist yet.
prune_tier() {
  local tier="$1" keep="$2"
  local base
  base="$(rclone_remote)/${tier}"

  local dirs=()
  # `|| true`: an absent tier dir makes lsf exit non-zero under `set -e`.
  mapfile -t dirs < <(rclone lsf --dirs-only "${base}" 2>/dev/null | sed 's:/*$::' | sort -r || true)

  local i=0 d
  for d in "${dirs[@]}"; do
    [ -n "${d}" ] || continue
    i=$((i + 1))
    if [ "${i}" -gt "${keep}" ]; then
      log "prune: removing ${tier}/${d} (keep=${keep})"
      rclone purge "${base}/${d}" || log "WARN: failed to purge ${base}/${d}"
    fi
  done
  log "prune: ${tier} retained $([ "${i}" -lt "${keep}" ] && echo "${i}" || echo "${keep}") of ${i}"
}
