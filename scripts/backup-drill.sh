#!/usr/bin/env bash
# HER-131 — backup → restore round-trip drill.
#
# Proves backup.sh + restore.sh actually work end-to-end WITHOUT any cloud
# credentials: it uses an ephemeral age keypair and rclone's on-the-fly
# `:local:` backend (a temp dir) as the "remote". Seeds a sentinel row + files,
# backs up, wipes them, restores, and asserts everything came back.
#
# Used by `make backup-drill` (against the local/dev Postgres) and by
# .github/workflows/restore-drill.yml (against the CI Postgres service).
#
# Postgres connection comes from env, defaulting to the dev compose values:
#   POSTGRES_HOST(127.0.0.1) POSTGRES_PORT(5432) POSTGRES_USER(hermes)
#   POSTGRES_PASSWORD(luminavault) POSTGRES_DATABASE(hermes_db)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for bin in age age-keygen rclone pg_dump pg_restore psql; do
  command -v "${bin}" >/dev/null 2>&1 || { echo "drill: missing dependency: ${bin}" >&2; exit 1; }
done

export POSTGRES_HOST="${POSTGRES_HOST:-127.0.0.1}"
export POSTGRES_PORT="${POSTGRES_PORT:-5432}"
export POSTGRES_USER="${POSTGRES_USER:-hermes}"
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-luminavault}"
export POSTGRES_DATABASE="${POSTGRES_DATABASE:-hermes_db}"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# Ephemeral age keypair (never persisted).
age-keygen -o "${WORK}/key.txt" 2>/dev/null
RECIPIENT="$(grep -oE 'age1[0-9a-z]+' "${WORK}/key.txt" | head -1)"
[ -n "${RECIPIENT}" ] || { echo "drill: failed to derive age recipient" >&2; exit 1; }

# Wire the scripts at an on-the-fly local rclone remote + temp src root.
export BACKUP_RCLONE_REMOTE=":local:${WORK}/remote"
export BACKUP_AGE_RECIPIENT="${RECIPIENT}"
export BACKUP_AGE_IDENTITY_PATH="${WORK}/key.txt"
export BACKUP_SRC_ROOT="${WORK}/src"
export BACKUP_LOG_DIR="${WORK}/logs"

SENTINEL="her131-drill-$(date -u +%s)"
STAMP="$(date -u +%F)"

echo "drill: seeding Postgres sentinel + data files"
export PGPASSWORD="${POSTGRES_PASSWORD}"
psql -h "${POSTGRES_HOST}" -p "${POSTGRES_PORT}" -U "${POSTGRES_USER}" -d "${POSTGRES_DATABASE}" -v ON_ERROR_STOP=1 -q <<SQL
DROP TABLE IF EXISTS her131_drill;
CREATE TABLE her131_drill (token text NOT NULL);
INSERT INTO her131_drill (token) VALUES ('${SENTINEL}');
SQL

mkdir -p "${BACKUP_SRC_ROOT}/luminavault" "${BACKUP_SRC_ROOT}/hermes"
echo "${SENTINEL}" >"${BACKUP_SRC_ROOT}/luminavault/sentinel.txt"
echo "${SENTINEL}" >"${BACKUP_SRC_ROOT}/hermes/sentinel.txt"

echo "drill: running backup.sh"
"${SCRIPT_DIR}/backup.sh"

echo "drill: asserting snapshot artifacts exist"
for art in pg.dump.age luminavault.tar.age hermes.tar.age MANIFEST.txt; do
  rclone lsf "${BACKUP_RCLONE_REMOTE}/daily/${STAMP}/${art}" >/dev/null 2>&1 \
    || { echo "drill: FAIL — missing artifact ${art}" >&2; exit 1; }
done

echo "drill: simulating data loss (drop table + remove files)"
psql -h "${POSTGRES_HOST}" -p "${POSTGRES_PORT}" -U "${POSTGRES_USER}" -d "${POSTGRES_DATABASE}" -v ON_ERROR_STOP=1 -q \
  -c "DROP TABLE her131_drill;"
rm -rf "${BACKUP_SRC_ROOT}/luminavault" "${BACKUP_SRC_ROOT}/hermes"

echo "drill: running restore.sh ${STAMP} --force"
"${SCRIPT_DIR}/restore.sh" "${STAMP}" --component all --force

echo "drill: asserting round-trip"
GOT_DB="$(psql -h "${POSTGRES_HOST}" -p "${POSTGRES_PORT}" -U "${POSTGRES_USER}" -d "${POSTGRES_DATABASE}" -tA \
  -c "SELECT token FROM her131_drill LIMIT 1;")"
[ "${GOT_DB}" = "${SENTINEL}" ] || { echo "drill: FAIL — pg sentinel '${GOT_DB}' != '${SENTINEL}'" >&2; exit 1; }

for comp in luminavault hermes; do
  GOT_FILE="$(cat "${BACKUP_SRC_ROOT}/${comp}/sentinel.txt" 2>/dev/null || true)"
  [ "${GOT_FILE}" = "${SENTINEL}" ] || { echo "drill: FAIL — ${comp} sentinel '${GOT_FILE}' != '${SENTINEL}'" >&2; exit 1; }
done

# Clean up the drill table so we don't leave it behind in a real DB.
psql -h "${POSTGRES_HOST}" -p "${POSTGRES_PORT}" -U "${POSTGRES_USER}" -d "${POSTGRES_DATABASE}" -q \
  -c "DROP TABLE IF EXISTS her131_drill;" >/dev/null 2>&1 || true

echo "drill: PASS — backup + restore round-trip verified (pg + vault + hermes)"
