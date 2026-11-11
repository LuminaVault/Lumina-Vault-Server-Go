#!/usr/bin/env bash
# Compares Swift and Go in the states a config switch hides from the main run.
# Needs scripts/parity-up.sh first (Postgres, parity_template, the Go image).
# Scenarios live outside parity/scenarios so the main compare skips them:
#   parity/scenarios-switched/<state>/<module>/*.yaml
#
#   community-off   COMMUNITY_ENABLED=false
#   posting-off     COMMUNITY_POSTING_ENABLED=false, and today's daily post published
#   unconfigured    ADMIN_TOKEN and REVENUECAT_WEBHOOK_SECRET empty, plugin signing key too short, no runner token
#                   (a short key with a runner token refuses to boot, in both servers)
#   runner-off      no runner token
#   catalog-unseeded  no "luminavault" publisher: the first read tries to seed the catalog
#   import-links      no switch: links staged by import have random paths, so they get a database of their own
#   hybrid-off      HYBRID_EXECUTION_ENABLED=false: /v1/memory/local-sync is not mounted
#   disk-full       a free-space floor no disk meets: every user-file write answers 507
#   vision-stub     VISION_EMBED_PROVIDER=stub: Swift's test adapter answers
#   vision-cohere   a Cohere key, with the base URL on the feeds stub (POST /v2/embed)
#   studio-off      CERBERUS_STUDIO_ENABLED=false: no workflow route is mounted
#   studio-on       CERBERUS_STUDIO_ENABLED=true and an OpenRouter key: managedInferenceAvailable
#   billing-on      BILLING_ENFORCEMENT_ENABLED=true: the /v1/knowledge and /v1/vision paywalls, the capture gate on memory upsert
#   whisper-stub    TRANSCRIBE_PROVIDER_OPENAI_BASE_URL on the LLM stub: transcripts come back
#   transcribe-fallback  TRANSCRIBE_PROVIDER=groq, which is no kind: falls back to the same stub
#   transcribe-stub TRANSCRIBE_PROVIDER=stub: Swift's test adapter answers
#   whisper-none    a blank base URL: no adapter, so 503
#   tts-on          /v1/tts mounted: Swift on a fake OpenAI key, Go on TTS_BASE_URL, both on the LLM stub
#   tts-billing     tts-on with BILLING_ENFORCEMENT_ENABLED=true
#   no-master-key   LV_SECRET_MASTER_KEY empty: /v1/grok is not mounted
#   improvement-off SELF_IMPROVEMENT_ENABLED=false and an empty SELF_IMPROVEMENT_ECONOMY_MODEL: /v1/me/improvement read-only, status unavailable
#
#   llm             HERMES_GATEWAY_URL on the LLM stub, and capture.autoFile.enabled on:
#                   the model-calling routes' success paths (group 4F)
#   agents          HERMES_GATEWAY_URL on the LLM stub, and BYO Hermes (private ranges over
#                   HTTP allowed) on its gateway routes: agent rooms' turns (group 5D)
#   chat            HERMES_GATEWAY_URL on the LLM stub, a fake OpenRouter key, and Go's runs
#                   bridge on swift2-switched: chat streams and escalations (group 5C)
#
# A state for other LLM success paths, once Go serves them (wave 5), is one more
# run_state with the stub up and SWITCH_HERMES_GATEWAY_URL=http://llmstub:8080,
# SWITCH_LLM_BASE_URL=http://llmstub:8080 or SWITCH_TRANSCRIBE_BASE_URL=http://llmstub:8080/v1.
# Chat also needs SWITCH_OPENROUTER_API_KEY: see docker-compose.parity.yml.
# Until then llm_selftest proves the stub, and the coalesce and ws steps, on Swift alone.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${PARITY_SWIFT_IMAGE:=lv-swift:fdbed9b0}"
export PARITY_SWIFT_IMAGE
COMPOSE=(docker compose -f docker-compose.parity.yml -p lvparity --profile servers --profile switched)

# RUN_STATE_ALSO names more services to recreate with the pair, after the databases
# (state "chat": swift2-switched, the Go server's runs bridge, on parity_go_switched).
run_state() {
  local state=$1
  shift
  local db
  local also=${RUN_STATE_ALSO:-}
  # shellcheck disable=SC2086
  "${COMPOSE[@]}" stop swift-switched go-switched $also >/dev/null 2>&1 || true
  for db in parity_swift_switched parity_go_switched; do
    "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d postgres -c "DROP DATABASE IF EXISTS $db WITH (FORCE)"
    "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d postgres -c "CREATE DATABASE $db TEMPLATE parity_template"
    if [ -s "parity/seed-switched/$state.sql" ]; then
      "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d "$db" < "parity/seed-switched/$state.sql"
    fi
  done
  # shellcheck disable=SC2086
  env "$@" "${COMPOSE[@]}" up -d --force-recreate swift-switched go-switched $also
  local port ports="18083 18084"
  case " $also " in *" swift2-switched "*) ports="$ports 18086" ;; esac
  for port in $ports; do
    for _ in $(seq 1 60); do
      curl -fsS "http://localhost:$port/health" >/dev/null 2>&1 && break
      sleep 1
    done
    # shellcheck disable=SC2086
    curl -fsS "http://localhost:$port/health" >/dev/null || { echo "server on :$port did not become healthy"; "${COMPOSE[@]}" logs --tail 40 swift-switched go-switched $also; exit 1; }
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
  SWITCH_HERMES_GATEWAY_URL=http://llmstub:8080 SWITCH_LLM_BASE_URL=http://llmstub:8080 \
    SWITCH_OPENROUTER_API_KEY=parity-openrouter-not-a-real-key SWITCH_TRANSCRIBE_BASE_URL=http://llmstub:8080/v1 \
    "${COMPOSE[@]}" up -d --force-recreate swift-switched swift2-switched
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
  # Zero differences alone would also pass if both servers failed the same way, so
  # the counters and Swift's transcripts have to show the real thing happened.
  local stats route
  stats=$(curl -fsS http://localhost:18085/stub/stats)
  for route in /v1/chat/completions /v1/audio/transcriptions /v1/runs; do
    calls=$(printf '%s' "$stats" | sed -n "s|.*\"$route\":\([0-9]*\).*|\1|p")
    echo "stub answered ${calls:-0} on $route"
    if [ "${calls:-0}" -lt 2 ]; then
      echo "the servers never reached the stub on $route"
      failed=1
    fi
  done
  local want
  while IFS='|' read -r want route; do
    if ! grep -rqE "$want" "swift-transcripts/llm-stub/$route"; then
      echo "no transcript under $route matches: $want"
      failed=1
    fi
  done <<'EXPECT'
"payload": "stub reply [0-9a-f]{16}"|chat/stream.yaml.json
"payload": "stub reply [0-9a-f]{16}"|chat/free-lane.yaml.json
"type": "hermes|chat/stream.yaml.json
"text": "stub transcript"|transcribe
"status": 101|ws
"hello"|ws
EXPECT
  "${COMPOSE[@]}" stop swift-switched swift2-switched llmstub >/dev/null 2>&1 || true
}

# Voice's states, against the LLM stub. Zero differences alone would also pass if both
# servers failed the same way, so the stub's counters have to show the calls happened.
voice_states() {
  local stats route calls
  "${COMPOSE[@]}" up -d --build --force-recreate llmstub
  for _ in $(seq 1 60); do
    curl -fsS http://localhost:18085/health >/dev/null 2>&1 && break
    sleep 1
  done
  run_state whisper-stub SWITCH_TRANSCRIBE_BASE_URL=http://llmstub:8080/v1
  run_state transcribe-fallback SWITCH_TRANSCRIBE_PROVIDER=groq SWITCH_TRANSCRIBE_BASE_URL=http://llmstub:8080/v1
  run_state transcribe-stub SWITCH_TRANSCRIBE_PROVIDER=stub
  run_state whisper-none 'SWITCH_TRANSCRIBE_BASE_URL= '
  run_state tts-on SWITCH_OPENAI_API_KEY=parity-openai-not-a-real-key SWITCH_LLM_BASE_URL=http://llmstub:8080 SWITCH_TTS_BASE_URL=http://llmstub:8080/v1
  run_state tts-billing SWITCH_OPENAI_API_KEY=parity-openai-not-a-real-key SWITCH_LLM_BASE_URL=http://llmstub:8080 SWITCH_TTS_BASE_URL=http://llmstub:8080/v1 SWITCH_BILLING_ENFORCEMENT_ENABLED=true
  stats=$(curl -fsS http://localhost:18085/stub/stats)
  for route in /v1/audio/transcriptions /v1/audio/speech; do
    calls=$(printf '%s' "$stats" | sed -n "s|.*\"$route\":\([0-9]*\).*|\1|p")
    echo "stub answered ${calls:-0} on $route"
    if [ "${calls:-0}" -lt 2 ]; then
      echo "the servers never reached the stub on $route"
      failed=1
    fi
  done
}

# Agents (wave 5D): the central agent on the stub as the gateway, and BYO Hermes on the
# stub's gateway routes, which the SSRF guard admits only with private ranges over HTTP.
# The stub's counters have to show both servers asked a BYO Hermes for real.
agents_state() {
  local before after route calls
  before=$(curl -fsS http://localhost:18085/stub/stats)
  run_state agents SWITCH_HERMES_GATEWAY_URL=http://llmstub:8080 SWITCH_BYO_HERMES_ALLOW_PRIVATE=true SWITCH_BYO_HERMES_REQUIRE_HTTPS=false
  after=$(curl -fsS http://localhost:18085/stub/stats)
  # /v1/chat/completions is the central agent (llm.RoutedService through the gateway).
  for route in /api/instance /api/sessions/{id}/chat /v1/chat/completions; do
    calls=$(( $(stub_count "$after" "$route") - $(stub_count "$before" "$route") ))
    echo "stub answered $calls on $route in state agents"
    if [ "$calls" -lt 2 ]; then
      echo "the servers never reached the stub on $route"
      failed=1
    fi
  done
}

# Chat (wave 5C): the classic stream and /v1/llm/chat through the stub as the gateway,
# and escalated turns. Swift starts their run itself; Go asks swift2-switched over the
# runs bridge (owner decision 1, option A), so swift2 shares Go's database. The stub's
# counters have to show both servers really streamed and started runs.
chat_state() {
  local before after route calls
  before=$(curl -fsS http://localhost:18085/stub/stats)
  RUN_STATE_ALSO=swift2-switched run_state chat SWITCH_HERMES_GATEWAY_URL=http://llmstub:8080 \
    SWITCH_OPENROUTER_API_KEY=parity-openrouter-not-a-real-key \
    SWITCH_CHAT_BRIDGE_URL=http://swift2-switched:8080 SWITCH_SWIFT2_DB=parity_go_switched
  after=$(curl -fsS http://localhost:18085/stub/stats)
  for route in /v1/chat/completions /v1/runs; do
    calls=$(( $(stub_count "$after" "$route") - $(stub_count "$before" "$route") ))
    echo "stub answered $calls on $route in state chat"
    if [ "$calls" -lt 2 ]; then
      echo "the servers never reached the stub on $route"
      failed=1
    fi
  done
  "${COMPOSE[@]}" stop swift2-switched >/dev/null 2>&1 || true
}

# stub_count STATS ROUTE prints ROUTE's counter in a /stub/stats body, 0 when absent.
stub_count() {
  local n
  n=$(printf '%s' "$1" | sed -n "s|.*\"$2\":\([0-9]*\).*|\1|p" | head -1)
  echo "${n:-0}"
}

failed=0
run_state community-off SWITCH_COMMUNITY_ENABLED=false
run_state posting-off SWITCH_COMMUNITY_POSTING_ENABLED=false
run_state unconfigured SWITCH_ADMIN_TOKEN= SWITCH_REVENUECAT_WEBHOOK_SECRET= SWITCH_SIGNING_KEY=too-short SWITCH_RUNNER_TOKEN=
run_state runner-off SWITCH_RUNNER_TOKEN=
run_state catalog-unseeded SWITCH_NONE=
run_state import-links SWITCH_NONE=
run_state hybrid-off SWITCH_HYBRID_EXECUTION_ENABLED=false
run_state disk-full SWITCH_STORAGE_MIN_FREE_BYTES=4611686018427387903
run_state vision-stub SWITCH_VISION_EMBED_PROVIDER=stub
run_state vision-cohere SWITCH_VISION_COHERE_API_KEY=parity-cohere-key SWITCH_VISION_COHERE_BASE_URL=http://feeds:8080
run_state vision-cohere-down SWITCH_VISION_COHERE_API_KEY=parity-cohere-key SWITCH_VISION_COHERE_BASE_URL=http://feeds:9
run_state studio-off SWITCH_CERBERUS_STUDIO_ENABLED=false
run_state studio-on SWITCH_CERBERUS_STUDIO_ENABLED=true SWITCH_OPENROUTER_API_KEY=parity-openrouter-key
run_state billing-on SWITCH_BILLING_ENFORCEMENT_ENABLED=true
run_state improvement-off SWITCH_SELF_IMPROVEMENT_ENABLED=false SWITCH_SELF_IMPROVEMENT_ECONOMY_MODEL=
voice_states
run_state no-master-key SWITCH_LV_SECRET_MASTER_KEY=
"${COMPOSE[@]}" up -d --build --force-recreate llmstub
for _ in $(seq 1 60); do
  curl -fsS "http://localhost:18085/health" >/dev/null 2>&1 && break
  sleep 1
done
run_state llm SWITCH_HERMES_GATEWAY_URL=http://llmstub:8080 SWITCH_CAPTURE_AUTO_FILE_ENABLED=true
agents_state
chat_state
llm_selftest
"${COMPOSE[@]}" stop swift-switched go-switched >/dev/null 2>&1 || true
exit "$failed"
