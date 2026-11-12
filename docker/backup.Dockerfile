# HER-131 — encrypted backup sidecar image.
#
# Base on the official postgres:18-alpine so pg_dump / pg_restore match the
# pgvector/pgvector:pg18 server major exactly (logical dumps are picky about
# client >= server major). Layer on age (encryption), rclone (off-site copy),
# and bash (the scripts use arrays / pipefail). busybox crond ships in the
# base image and drives the nightly schedule.
FROM postgres:18-alpine

RUN apk add --no-cache age rclone bash tzdata coreutils curl

# Backup + restore scripts and their shared lib. backup.sh resolves its lib by
# its own directory, so all three must live together.
COPY scripts/backup-lib.sh scripts/backup.sh scripts/restore.sh /usr/local/bin/
COPY docker/backup-entrypoint.sh docker/run-backup.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/backup-lib.sh \
             /usr/local/bin/backup.sh \
             /usr/local/bin/restore.sh \
             /usr/local/bin/backup-entrypoint.sh \
             /usr/local/bin/run-backup.sh \
  && mkdir -p /var/log/backup /backup/src

# Override the postgres image's entrypoint — we are NOT starting a database.
ENTRYPOINT ["/usr/local/bin/backup-entrypoint.sh"]
