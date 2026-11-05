---
name: capture-enrich
description: Enrich newly captured URLs with oEmbed metadata, summary, and topic tags written back to the source vault file.
allowed-tools: vault_read
metadata:
  capability: low
  on_event:
    - vault_file_created
  outputs:
    - kind: vault_rewrite
---
TODO HER-173 — body prompt to be written. The runner expects ~300 words of skill-specific instructions here.
