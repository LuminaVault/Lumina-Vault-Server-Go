#!/usr/bin/env bash
# HER-131 — restore from an encrypted backup snapshot. Disaster recovery + the
# CI restore drill both run this.
#
#   restore.sh <YYYY-MM-DD> [--component pg|vault|hermes|all]
#                           [--tier daily|weekly|monthly]
#                           [--force]
#
# DESTRUCTIVE: --component pg runs `pg_restore --clean` (drops + recreates
# objects); vault/hermes extract a tar OVER the live bind-mount targets. The
# stack should be stopped first for vault/hermes restores. Without --force the
# script prints exactly what it will overwrite and waits for an interactive
# `yes`.
#
# Required env:
#   BACKUP_RCLONE_REMOTE       rclone remote base (same as backup.sh)
#   BACKUP_AGE_IDENTITY_PATH   path to the age PRIVATE identity (age-keygen output)
#   POSTGRES_HOST POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DATABASE
# Optional env:
#   POSTGRES_PORT      (default 5432)
#   BACKUP_SRC_ROOT    extract target root for vault/hermes (default /backup/src)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/backup-lib.sh
source "${SCRIPT_DIR}/backup-lib.sh"

# --- Arg parsing -------------------------------------------------------------
DATE=""
COMPONENT="all"
TIER=""
FORCE=0

usage() {
  sed -n '2,18p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --component) COMPONENT="$2"; shift 2 ;;
    --tier)      TIER="$2"; shift 2 ;;
    --force)     FORCE=1; shift ;;
    -h|--help)   usage 0 ;;
    -*)          die "unknown flag: $1" ;;
    *)           [ -z "${DATE}" ] && DATE="$1" && shift || die "unexpected arg: $1" ;;
  esac
done

[ -n "${DATE}" ] || usage 1
case "${COMPONENT}" in pg|vault|hermes|all) ;; *) die "invalid --component: ${COMPONENT}" ;; esac

# require_env checks the recipient too; restore also needs the private identity.
require_env
IDENTITY="${BACKUP_AGE_IDENTITY_PATH:?BACKUP_AGE_IDENTITY_PATH is unset (path to age private identity)}"
[ -f "${IDENTITY}" ] || die "age identity not found at ${IDENTITY}"

REMOTE="$(rclone_remote)"
SRC_ROOT="${BACKUP_SRC_ROOT:-/backup/src}"
PG_PORT="${POSTGRES_PORT:-5432}"

# --- Locate the snapshot -----------------------------------------------------
# Explicit --tier wins; otherwise search daily → weekly → monthly.
resolve_tier() {
  local t
  for t in daily weekly monthly; do
    if rclone lsf --dirs-only "${REMOTE}/${t}/" 2>/dev/null | sed 's:/*$::' | grep -qx "${DATE}"; then
      printf '%s' "${t}"; return 0
    fi
  done
  return 1
}

if [ -z "${TIER}" ]; then
  TIER="$(resolve_tier)" || die "no snapshot for ${DATE} in any tier under ${REMOTE}"
fi
SRC="${REMOTE}/${TIER}/${DATE}"
rclone lsf "${SRC}/" >/dev/null 2>&1 || die "snapshot not found: ${SRC}"

log "restore: source ${SRC} (tier=${TIER}) component=${COMPONENT}"

# --- Confirmation ------------------------------------------------------------
if [ "${FORCE}" -ne 1 ]; then
  echo "About to RESTORE from ${SRC}" >&2
  echo "This is DESTRUCTIVE:" >&2
  case "${COMPONENT}" in
    pg|all)     echo "  - pg_restore --clean into DB ${POSTGRES_DATABASE}@${POSTGRES_HOST}:${PG_PORT} (drops + recreates objects)" >&2 ;;
  esac
  case "${COMPONENT}" in
    vault|all)  echo "  - extract luminavault.tar OVER ${SRC_ROOT}/luminavault" >&2 ;;
  esac
  case "${COMPONENT}" in
    hermes|all) echo "  - extract hermes.tar OVER ${SRC_ROOT}/hermes" >&2 ;;
  esac
  printf 'Type "yes" to proceed: ' >&2
  read -r reply
  [ "${reply}" = "yes" ] || die "aborted by user"
fi

# --- Stage + verify ----------------------------------------------------------
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

want_artifact() {
  case "$1" in
    pg.dump.age)        [ "${COMPONENT}" = "pg" ] || [ "${COMPONENT}" = "all" ] ;;
    luminavault.tar.age)[ "${COMPONENT}" = "vault" ] || [ "${COMPONENT}" = "all" ] ;;
    hermes.tar.age)     [ "${COMPONENT}" = "hermes" ] || [ "${COMPONENT}" = "all" ] ;;
    *) return 1 ;;
  esac
}

log "restore: downloading snapshot to staging"
rclone copy "${SRC}" "${WORK}"

# Verify each staged .age against the MANIFEST sha256 lines (if present).
if [ -f "${WORK}/MANIFEST.txt" ] && grep -q '^--- sha256 ---' "${WORK}/MANIFEST.txt"; then
  log "restore: verifying checksums against MANIFEST"
  # MANIFEST sha256 block lines look like: "<hash>  <name>"
  sed -n '/^--- sha256 ---/,$p' "${WORK}/MANIFEST.txt" | tail -n +2 \
    | while read -r hash name; do
        [ -n "${name}" ] || continue
        name="$(basename "${name}")"
        [ -f "${WORK}/${name}" ] || continue
        actual="$(sha256sum "${WORK}/${name}" | awk '{print $1}')"
        if [ "${actual}" != "${hash}" ]; then
          die "checksum mismatch for ${name}: manifest=${hash} actual=${actual}"
        fi
      done
else
  log "WARN: no sha256 block in MANIFEST — skipping checksum verification"
fi

# --- Apply -------------------------------------------------------------------
if want_artifact pg.dump.age; then
  [ -f "${WORK}/pg.dump.age" ] || die "pg.dump.age missing from snapshot"
  log "restore: pg_restore → ${POSTGRES_DATABASE}@${POSTGRES_HOST}:${PG_PORT}"
  age -d -i "${IDENTITY}" "${WORK}/pg.dump.age" \
    | PGPASSWORD="${POSTGRES_PASSWORD}" pg_restore \
        -h "${POSTGRES_HOST}" -p "${PG_PORT}" \
        -U "${POSTGRES_USER}" -d "${POSTGRES_DATABASE}" \
        --clean --if-exists --no-owner --no-privileges
fi

for comp in luminavault hermes; do
  case "${comp}" in
    luminavault) want_artifact luminavault.tar.age || continue ;;
    hermes)      want_artifact hermes.tar.age || continue ;;
  esac
  art="${WORK}/${comp}.tar.age"
  [ -f "${art}" ] || { log "WARN: ${comp}.tar.age missing from snapshot — skipping"; continue; }
  log "restore: extracting ${comp} → ${SRC_ROOT}/${comp}"
  mkdir -p "${SRC_ROOT}"
  age -d -i "${IDENTITY}" "${art}" | tar -C "${SRC_ROOT}" -xf -
done

log "restore: completed from ${SRC}"
