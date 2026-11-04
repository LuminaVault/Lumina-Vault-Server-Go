#!/usr/bin/env bash
# Compares Swift and Go in the states a config switch hides from the main run.
# Needs scripts/parity-up.sh first (Postgres, parity_template, the Go image).
# Scenarios live outside parity/scenarios so the main compare skips them:
#   parity/scenarios-switched/<state>/<module>/*.yaml
#
#   community-off   COMMUNITY_ENABLED=false
#   posting-off     COMMUNITY_POSTING_ENABLED=false, and today's daily post published
#   unconfigured    ADMIN_TOKEN empty, plugin signing key too short
set -euo pipefail
cd "$(dirname "$0")/.."

: "${PARITY_SWIFT_IMAGE:=lv-swift:fdbed9b0}"
export PARITY_SWIFT_IMAGE
COMPOSE=(docker compose -f docker-compose.parity.yml -p lvparity --profile servers --profile switched)

run_state() {
  local state=$1
  shift
  local db
  "${COMPOSE[@]}" stop swift-switched go-switched >/dev/null 2>&1 || true
  for db in parity_swift_switched parity_go_switched; do
    "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d postgres -c "DROP DATABASE IF EXISTS $db WITH (FORCE)"
    "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d postgres -c "CREATE DATABASE $db TEMPLATE parity_template"
    if [ -s "parity/seed-switched/$state.sql" ]; then
      "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d "$db" < "parity/seed-switched/$state.sql"
    fi
  done
  env "$@" "${COMPOSE[@]}" up -d --force-recreate swift-switched go-switched
  local port
  for port in 18083 18084; do
    for _ in $(seq 1 60); do
      curl -fsS "http://localhost:$port/health" >/dev/null 2>&1 && break
      sleep 1
    done
    curl -fsS "http://localhost:$port/health" >/dev/null || { echo "server on :$port did not become healthy"; "${COMPOSE[@]}" logs --tail 40 swift-switched go-switched; exit 1; }
  done
  echo "==> state $state"
  go run ./parity/cmd/parity compare \
    --swift http://localhost:18083 --swift-db postgres://lv:lv@localhost:15432/parity_swift_switched \
    --go http://localhost:18084 --go-db postgres://lv:lv@localhost:15432/parity_go_switched \
    --scenarios "parity/scenarios-switched/$state"
}

run_state community-off SWITCH_COMMUNITY_ENABLED=false
run_state posting-off SWITCH_COMMUNITY_POSTING_ENABLED=false
run_state unconfigured SWITCH_ADMIN_TOKEN= SWITCH_SIGNING_KEY=too-short
"${COMPOSE[@]}" stop swift-switched go-switched >/dev/null 2>&1 || true
