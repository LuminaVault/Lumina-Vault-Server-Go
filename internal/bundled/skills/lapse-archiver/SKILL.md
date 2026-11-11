---
name: lapse-archiver
description: Nightly billing housekeeping for expired trials, lapsed accounts, cold archival, and GDPR hard delete.
allowed-tools: vault_read vault_write
metadata:
  capability: low
  schedule: "0 3 * * *"
---
System-owned billing maintenance job.

The Swift `LapseArchiverJob` is the source of truth for mutations:
expired active tiers become lapsed, lapsed accounts older than 90 days move
their vault to cold storage and become archived, and archived accounts older
than 365 days are hard-deleted.
