#!/usr/bin/env bash
# Compares Swift and Go in the states a config switch hides from the main run.
# Needs scripts/parity-up.sh first (Postgres, parity_template, the Go image).
# Scenarios live outside parity/scenarios so the main compare skips them:
#   parity/scenarios-switched/<state>/<module>/*.yaml
#
#   community-off   COMMUNITY_ENABLED=false
#   posting-off     COMMUNITY_POSTING_ENABLED=false, and today's daily post published
#   unconfigured    ADMIN_TOKEN empty, plugin signing key too short, no runner token
#                   (a short key with a runner token refuses to boot, in both servers)
#   runner-off      no runner token
#   catalog-unseeded  no "luminavault" publisher: the first read tries to seed the catalog
#   hybrid-off      HYBRID_EXECUTION_ENABLED=false: /v1/memory/local-sync is not mounted
#   vision-stub     VISION_EMBED_PROVIDER=stub: Swift's test adapter answers
#   vision-cohere   a Cohere key, with the base URL on the feeds stub (POST /v2/embed)
#   billing-on      BILLING_ENFORCEMENT_ENABLED=true: the /v1/knowledge and /v1/vision paywalls, the capture gate on memory upsert
#
# A state for LLM success paths, once Go serves them (group 4F), is one more
# run_state with SWITCH_HERMES_GATEWAY_URL=http://llmstub:8080 and the stub up.
# Until then llm_selftest proves the stub on Swift alone.
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
  # A difference fails the script at the end, so one run reports every state.
  go run ./parity/cmd/parity compare \
    --swift http://localhost:18083 --swift-db postgres://lv:lv@localhost:15432/parity_swift_switched \
    --go http://localhost:18084 --go-db postgres://lv:lv@localhost:15432/parity_go_switched \
    --swift-files docker:lvparity-swift-switched-1:/tmp/luminavault \
    --go-files docker:lvparity-go-switched-1:/tmp/luminavault \
    --scenarios "parity/scenarios-switched/$state" || failed=1
}

# Swift against Swift with HERMES_GATEWAY_URL on the stub: zero differences shows
# the stub is deterministic, and its counter shows both servers really reached it
# rather than failing closed the same way.
llm_selftest() {
  local db port calls
  "${COMPOSE[@]}" stop swift-switched go-switched swift2-switched >/dev/null 2>&1 || true
  for db in parity_swift_switched parity_swift2_switched; do
    "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d postgres -c "DROP DATABASE IF EXISTS $db WITH (FORCE)"
    "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d postgres -c "CREATE DATABASE $db TEMPLATE parity_template"
  done
  "${COMPOSE[@]}" up -d --build --force-recreate llmstub
  SWITCH_HERMES_GATEWAY_URL=http://llmstub:8080 "${COMPOSE[@]}" up -d --force-recreate swift-switched swift2-switched
  for port in 18083 18086 18085; do
    for _ in $(seq 1 60); do
      curl -fsS "http://localhost:$port/health" >/dev/null 2>&1 && break
      sleep 1
    done
    curl -fsS "http://localhost:$port/health" >/dev/null || { echo "server on :$port did not become healthy"; "${COMPOSE[@]}" logs --tail 40 swift-switched swift2-switched llmstub; exit 1; }
  done
  echo "==> LLM stub self-test (Swift vs Swift)"
  go run ./parity/cmd/parity compare \
    --swift http://localhost:18083 --swift-db postgres://lv:lv@localhost:15432/parity_swift_switched \
    --go http://localhost:18086 --go-db postgres://lv:lv@localhost:15432/parity_swift2_switched \
    --save-swift-transcripts swift-transcripts/llm-stub \
    --scenarios parity/scenarios-llm || failed=1
  calls=$(curl -fsS http://localhost:18085/stub/stats | sed -n 's/.*"\/v1\/chat\/completions":\([0-9]*\).*/\1/p')
  echo "stub answered ${calls:-0} chat completions"
  if [ "${calls:-0}" -lt 2 ]; then
    echo "the servers never reached the LLM stub"
    failed=1
  fi
  "${COMPOSE[@]}" stop swift-switched swift2-switched llmstub >/dev/null 2>&1 || true
}

failed=0
run_state community-off SWITCH_COMMUNITY_ENABLED=false
run_state posting-off SWITCH_COMMUNITY_POSTING_ENABLED=false
run_state unconfigured SWITCH_ADMIN_TOKEN= SWITCH_SIGNING_KEY=too-short SWITCH_RUNNER_TOKEN=
run_state runner-off SWITCH_RUNNER_TOKEN=
run_state catalog-unseeded SWITCH_NONE=
run_state hybrid-off SWITCH_HYBRID_EXECUTION_ENABLED=false
run_state vision-stub SWITCH_VISION_EMBED_PROVIDER=stub
run_state vision-cohere SWITCH_VISION_COHERE_API_KEY=parity-cohere-key SWITCH_VISION_COHERE_BASE_URL=http://feeds:8080
run_state vision-cohere-down SWITCH_VISION_COHERE_API_KEY=parity-cohere-key SWITCH_VISION_COHERE_BASE_URL=http://feeds:9
run_state billing-on SWITCH_BILLING_ENFORCEMENT_ENABLED=true
llm_selftest
"${COMPOSE[@]}" stop swift-switched go-switched >/dev/null 2>&1 || true
exit "$failed"
