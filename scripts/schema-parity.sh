#!/usr/bin/env bash
# Phase 1 Task 1.5: goose must build exactly the schema Swift's Fluent builds.
# Runs the Go `migrate` on a fresh database and diffs its normalized
# schema-only dump against internal/migrations/sql/schema-swift.sql (from
# scripts/schema-snapshot.sh). Exits non-zero on any difference.
set -euo pipefail
cd "$(dirname "$0")/.."
COMPOSE=(docker compose -f docker-compose.parity.yml -p lvparity)
DB=goose_check

"${COMPOSE[@]}" up -d --wait pg
pg() { "${COMPOSE[@]}" exec -T pg "$@"; }
pg psql -v ON_ERROR_STOP=1 -q -U lv -d postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" -c "CREATE DATABASE $DB"

POSTGRES_HOST=localhost POSTGRES_PORT=15432 POSTGRES_USER=lv POSTGRES_PASSWORD=lv POSTGRES_DATABASE=$DB \
  go run ./cmd/App migrate

pg pg_dump --schema-only --no-owner --no-privileges --no-comments -U lv -d "$DB" \
    --exclude-table=goose_db_version --exclude-table=goose_db_version_id_seq \
  | grep -vE '^--|^$|^SELECT pg_catalog.set_config|^SET |^\\(un)?restrict ' > /tmp/lv-schema-goose.$$
if diff -u internal/migrations/sql/schema-swift.sql /tmp/lv-schema-goose.$$; then
  echo "schema parity: goose == Fluent ($(wc -l < internal/migrations/sql/schema-swift.sql) lines)"
  rm -f /tmp/lv-schema-goose.$$
else
  echo "schema parity FAILED (goose dump kept at /tmp/lv-schema-goose.$$)"
  exit 1
fi
