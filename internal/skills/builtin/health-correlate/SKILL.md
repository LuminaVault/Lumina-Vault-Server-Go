---
name: health-correlate
description: Correlate HealthKit signals (sleep, HRV, workouts) with notes and memories to surface mood and energy patterns.
allowed-tools: session_search vault_read memory_upsert
metadata:
  capability: high
  # Manual only until HER-173 writes the prompt below; it ran nightly for every user.
  schedule: ""
  outputs:
    - kind: memory_emit
    - kind: apns_nudge
---
TODO HER-173 — body prompt to be written. The runner expects ~300 words of skill-specific instructions here. Skips users with <30 days of HealthKit data.
