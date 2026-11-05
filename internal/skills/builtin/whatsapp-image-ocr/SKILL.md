---
name: whatsapp-image-ocr
description: OCR images received via WhatsApp with Tesseract and save extracted text to the Obsidian vault. Use when an inbound WhatsApp message has an image attachment that should be transcribed and archived.
license: MIT
metadata:
  capability: medium
---

# WhatsApp Image OCR

Extracts text from images received via WhatsApp using Tesseract OCR and writes results into the Obsidian vault.

## Entrypoint

```
python3 scripts/whatsapp_image_ocr.py
```

## Environment

| Variable | Required | Default | Description |
|---|---|---|---|
| `OBSIDIAN_VAULT_PATH` | yes | `~/obsidian-vault/Vault` | Vault root |
| `OBSIDIAN_VAULT_NAME` | yes | `Vault` | Vault name |
| `OCR_LANGUAGE` | no | `eng+deu` | Tesseract language codes |
| `SAVE_IMAGES` | no | `false` | Persist original image files |

## Message contract

Bridge delivers JSON with `mediaType=image` and `mediaUrls=[<local-path>]`. See `references/README.md` for the full schema.

## Files

- `scripts/whatsapp_image_ocr.py` — main processor
- `scripts/test_whatsapp_ocr.py` — test harness
- `references/README.md` — environment + message format reference
