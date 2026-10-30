#!/usr/bin/env bash
# Bring up the parity environment (docker-compose.parity.yml):
#   1. Postgres
#   2. parity_template = Swift `migrate` + parity/seed/base.sql, then every
#      other parity/seed/*.sql in name order (one file per module)
#   3. parity_swift / parity_go / parity_swift2 cloned from the template
#   4. the three servers
# Re-running drops and recreates every parity_* database.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${PARITY_SWIFT_IMAGE:=lv-swift:fdbed9b0}"
export PARITY_SWIFT_IMAGE
COMPOSE=(docker compose -f docker-compose.parity.yml -p lvparity)

psql_pg() { "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -U lv -d postgres "$@"; }

"${COMPOSE[@]}" up -d --wait pg
"${COMPOSE[@]}" --profile servers stop swift swift2 go >/dev/null 2>&1 || true

for db in parity_swift parity_go parity_swift2 parity_template; do
  psql_pg -q -c "DROP DATABASE IF EXISTS $db WITH (FORCE)"
done
psql_pg -q -c "CREATE DATABASE parity_template"

echo "==> Swift migrate into parity_template ($PARITY_SWIFT_IMAGE)"
"${COMPOSE[@]}" run --rm --no-deps -e POSTGRES_DATABASE=parity_template swift migrate

for seed in parity/seed/base.sql $(ls parity/seed/*.sql | grep -v '/base\.sql$' | sort); do
  [ -s "$seed" ] || continue
  echo "    seed $seed"
  "${COMPOSE[@]}" exec -T pg psql -v ON_ERROR_STOP=1 -q -U lv -d parity_template < "$seed"
done

for db in parity_swift parity_go parity_swift2; do
  psql_pg -q -c "CREATE DATABASE $db TEMPLATE parity_template"
done

echo "==> servers"
"${COMPOSE[@]}" --profile servers up -d --build swift swift2 go
for port in 18080 18081 18082; do
  for _ in $(seq 1 60); do
    curl -fsS "http://localhost:$port/health" >/dev/null 2>&1 && break
    sleep 1
  done
  curl -fsS "http://localhost:$port/health" >/dev/null || { echo "server on :$port did not become healthy"; "${COMPOSE[@]}" --profile servers logs --tail 40; exit 1; }
done
echo "parity environment ready: swift :18080  go :18081  swift2 :18082  pg :15432"
