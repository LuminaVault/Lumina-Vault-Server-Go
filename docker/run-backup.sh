#!/usr/bin/env bash
# HER-131 — thin cron wrapper. crond runs with an empty environment, so source
# the snapshot written by backup-entrypoint.sh before invoking backup.sh.
set -euo pipefail
[ -f /etc/backup.env ] && . /etc/backup.env
exec /usr/local/bin/backup.sh
