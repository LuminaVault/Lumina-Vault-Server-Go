#!/usr/bin/env bash
# HER-131 — entrypoint for the backup sidecar.
#
# Validates config, snapshots the container env into a file the cron job can
# source (cron strips the environment), installs the crontab, optionally runs
# one backup immediately, then hands PID 1 to busybox crond.
set -euo pipefail

# shellcheck source=scripts/backup-lib.sh
source /usr/local/bin/backup-lib.sh

require_env
mkdir -p /var/log/backup

# Cron jobs run with an empty environment. Persist the (exported) container env
# so run-backup.sh can source it. 0600 because it contains POSTGRES_PASSWORD.
export -p >/etc/backup.env
chmod 600 /etc/backup.env

CRON="${BACKUP_CRON:-0 3 * * *}"
mkdir -p /etc/crontabs
echo "${CRON} /usr/local/bin/run-backup.sh" >/etc/crontabs/root

if [ "${BACKUP_RUN_ON_START:-false}" = "true" ]; then
  log "entrypoint: BACKUP_RUN_ON_START=true — running an initial backup"
  /usr/local/bin/run-backup.sh || log "WARN: initial backup failed (crond will retry on schedule)"
fi

log "entrypoint: starting crond (schedule: '${CRON}', remote: $(rclone_remote))"
exec crond -f -l 8 -L /var/log/backup/cron.log
