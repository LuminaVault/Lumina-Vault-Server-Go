#!/usr/bin/env bash
# HER-131 — nightly encrypted backup job.
#
# Runs inside the `backup` compose service (driven by crond) or standalone for
# a manual/CI run. Streams everything age-encrypted straight to the rclone
# remote — no plaintext copy ever touches local disk.
#
#   pg_dump -Fc        | age -r <recipient> | rclone rcat  remote/daily/<date>/pg.dump.age
#   tar luminavault    | age -r <recipient> | rclone rcat  remote/daily/<date>/luminavault.tar.age
#   tar hermes         | age -r <recipient> | rclone rcat  remote/daily/<date>/hermes.tar.age
#
# Then promotes to weekly/monthly tiers on the configured days and prunes each
# tier to its retain count (defaults 7 daily / 4 weekly / 6 monthly).
#
# Required env:
#   BACKUP_RCLONE_REMOTE   rclone remote base, e.g. b2:bucket/luminavault
#   BACKUP_AGE_RECIPIENT   age recipient public key (age1...)
#   POSTGRES_HOST POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DATABASE
# Optional env:
#   POSTGRES_PORT          (default 5432)
#   BACKUP_SRC_ROOT        dir holding luminavault/ and hermes/ (default /backup/src)
#   BACKUP_WEEKLY_DOW      ISO day-of-week to promote to weekly (default 7 = Sun)
#   BACKUP_RETAIN_DAILY|WEEKLY|MONTHLY  retain counts (defaults 7/4/6)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/backup-lib.sh
source "${SCRIPT_DIR}/backup-lib.sh"

require_env

STAMP="$(date -u +%F)"          # YYYY-MM-DD (UTC)
DOW="$(date -u +%u)"            # 1..7 (Mon..Sun)
DOM="$(date -u +%d)"           # 01..31
REMOTE="$(rclone_remote)"
DAILY="${REMOTE}/daily/${STAMP}"
SRC_ROOT="${BACKUP_SRC_ROOT:-/backup/src}"
PG_PORT="${POSTGRES_PORT:-5432}"

# Alert on any failure (ERR fires on the first failing command under set -e).
# STAMP is defined above so the message names the snapshot in flight.
trap 'rc=$?; notify "FAILED" "snapshot ${STAMP} aborted (exit ${rc}); check ${BACKUP_LOG_DIR}/backup.log"; exit ${rc}' ERR

log "backup: starting ${STAMP} → ${DAILY}"

# --- Postgres (logical dump, custom format) ---------------------------------
log "backup: pg_dump ${POSTGRES_DATABASE}@${POSTGRES_HOST}:${PG_PORT} → pg.dump.age"
PGPASSWORD="${POSTGRES_PASSWORD}" pg_dump \
  -h "${POSTGRES_HOST}" -p "${PG_PORT}" \
  -U "${POSTGRES_USER}" -d "${POSTGRES_DATABASE}" \
  --format=custom --no-owner --no-privileges \
  | age -r "${BACKUP_AGE_RECIPIENT}" \
  | rclone rcat "${DAILY}/pg.dump.age"

# --- Vault + Hermes data dirs (tar, then encrypt) ----------------------------
for comp in luminavault hermes; do
  if [ -d "${SRC_ROOT}/${comp}" ]; then
    log "backup: tar ${comp} → ${comp}.tar.age"
    tar -C "${SRC_ROOT}" -cf - "${comp}" \
      | age -r "${BACKUP_AGE_RECIPIENT}" \
      | rclone rcat "${DAILY}/${comp}.tar.age"
  else
    log "WARN: skipping ${comp} — no directory at ${SRC_ROOT}/${comp}"
  fi
done

# --- Manifest (written last so it is excluded from its own checksums) --------
# restore.sh verifies the .age artifacts against the sha256 lines below before
# decrypting. `rclone hashsum` is computed remote-side; supported by local, B2,
# and S3 backends.
log "backup: writing MANIFEST.txt"
{
  echo "stamp: ${STAMP}"
  echo "created_utc: $(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  echo "pg_dump_version: $(pg_dump --version | head -1)"
  echo "--- artifacts (rclone lsl) ---"
  rclone lsl "${DAILY}"
  echo "--- sha256 ---"
  rclone hashsum sha256 "${DAILY}" 2>/dev/null || echo "(sha256 unsupported on this remote)"
} | rclone rcat "${DAILY}/MANIFEST.txt"

# --- Promote to weekly / monthly tiers --------------------------------------
if [ "${DOW}" = "${BACKUP_WEEKLY_DOW:-7}" ]; then
  log "promote: daily/${STAMP} → weekly/${STAMP}"
  rclone copy "${DAILY}" "${REMOTE}/weekly/${STAMP}"
fi
if [ "${DOM}" = "01" ]; then
  log "promote: daily/${STAMP} → monthly/${STAMP}"
  rclone copy "${DAILY}" "${REMOTE}/monthly/${STAMP}"
fi

# --- Retention --------------------------------------------------------------
prune_tier daily   "${BACKUP_RETAIN_DAILY:-7}"
prune_tier weekly  "${BACKUP_RETAIN_WEEKLY:-4}"
prune_tier monthly "${BACKUP_RETAIN_MONTHLY:-6}"

log "backup: completed ${STAMP}"
if [ "${BACKUP_ALERT_ON_SUCCESS:-false}" = "true" ]; then
  notify "OK" "snapshot ${STAMP} completed"
fi
