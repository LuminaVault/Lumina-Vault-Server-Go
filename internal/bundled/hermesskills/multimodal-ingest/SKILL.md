---
name: multimodal-ingest
description: Analyze a LuminaVault PDF, image, audio, video, or web source and return structured vault-ready JSON.
---

# Multimodal Ingest

Use this skill when LuminaVault asks Hermes to analyze a source that has already
been saved to the tenant vault. Never move, replace, or delete the original.

## Workflow

1. Inspect the supplied source path or HTTP(S) URL with the available document,
   vision, transcription, video, and web-reading tools.
2. Preserve page numbers and timestamps where the source provides them.
3. Extract useful text, a concise summary, and 1–8 lowercase topical tags.
4. Assess source credibility as provenance quality, not as a truth verdict.
   Personal media uses a null score. Published sources use a 0–100 score and
   list the observable signals that affected it.
5. Return JSON only. Do not wrap it in a Markdown fence.

```json
{
  "title": "Human-readable title",
  "markdown": "Source-grounded extraction with page/time references",
  "summary": "Concise summary",
  "tags": ["topic"],
  "credibility": {
    "score": 80,
    "confidence": 0.75,
    "signals": ["named publisher", "dated primary source"],
    "rationale": "Short, neutral explanation",
    "version": "hermes-source-credibility-v1"
  }
}
```

If a modality tool is unavailable, fail clearly instead of fabricating an
extraction. LuminaVault will retain the original and mark the job as capability
blocked so it can be retried after Hermes is upgraded.
