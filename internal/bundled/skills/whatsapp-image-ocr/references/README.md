# WhatsApp Image OCR Skill - Reference Guide

## Overview

This skill provides OCR processing for images received via WhatsApp, extracting text and saving it to your Obsidian vault.

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `OBSIDIAN_VAULT_PATH` | Yes | - | Path to your Obsidian vault |
| `OBSIDIAN_VAULT_NAME` | Yes | - | Name of your primary vault |
| `OCR_LANGUAGE` | No | `eng+deu` | Tesseract language codes (e.g., `eng+deu+spa`) |
| `SAVE_IMAGES` | No | `false` | Set to `true` to save original image files |

## Message Format

The WhatsApp bridge provides message events with this structure:

```json
{
  "messageId": "abc123",
  "chatId": "1234567890@s.whatsapp.net",
  "senderId": "1234567890@s.whatsapp.net",
  "senderName": "John Doe",
  "chatName": "John Doe",
  "isGroup": false,
  "body": "[image received]",
  "hasMedia": true,
  "mediaType": "image",
  "mediaUrls": ["/root/.hermes/image_cache/img_abcdef.jpg"],
  "timestamp": 1715678901.234
}
```

## OCR Processing

### Tesseract Configuration

The skill uses Tesseract with these default parameters:

- `--oem 1`: OCR Engine Mode 1 (LSTM neural net)
- `--psm 3`: Page Segmentation Mode 3 (fully automatic)
- Language: `eng+deu` (English + German)

### Supported Image Formats

Tesseract supports: JPEG, PNG, TIFF, BMP, GIF, WebP, and more.

## Vault Structure

Processed images are saved to:

```
vault/Raw/WhatsApp Images/YYYY-MM-DD - WhatsApp Image {message_id}.md
```

## Error Handling

### Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `Tesseract not found` | Tesseract not installed | Install Tesseract: `apt-get install tesseract-ocr` |
| `Language data not found` | Missing Tesseract language pack | Install language pack: `apt-get install tesseract-ocr-<lang>` |
| `Failed to download image` | Bridge couldn't download media | Check WhatsApp connection and permissions |
| `OCR returned empty` | Image contains no text | Verify image quality and orientation |

### Debugging Tips

1. Check bridge logs: `tail -f ~/.hermes/logs/gateway.log | grep whatsapp`
2. Verify Tesseract installation: `tesseract --version`
3. Test OCR manually: `tesseract test.jpg stdout -l eng`
4. Check image cache: `ls ~/.hermes/image_cache/`

## Performance

### Processing Time

- Small images (100KB-1MB): 2-5 seconds
- Medium images (1-5MB): 5-15 seconds
- Large images (5MB+): 15-30 seconds

### Resource Usage

- CPU: Moderate (Tesseract is CPU-intensive)
- Memory: Low to moderate (depends on image size)
- Disk: Temporary files in `~/.hermes/image_cache`

## Security

### Privacy Considerations

- Images may contain personal/sensitive information
- Stored images are in your local vault (not uploaded to cloud)
- Consider encrypting your vault if processing sensitive content

### Access Control

- Ensure `WHATSAPP_ALLOWED_USERS` is configured to prevent unauthorized access
- Use strong authentication for your Hermes instance

## Maintenance

### Log Files

- Main logs: `~/.hermes/logs/agent.log`
- WhatsApp bridge logs: `~/.hermes/logs/gateway.log`
- Skill-specific logs: Check the agent log for "whatsapp_image_ocr" entries

### Cleanup

The skill automatically cleans up temporary files. To manually clean the image cache:

```bash
rm -rf ~/.hermes/image_cache/*
```

## Troubleshooting

### Problem: OCR returns gibberish or no text

**Solution:**
1. Check image quality (blur, rotation, lighting)
2. Try preprocessing the image (deskew, denoise)
3. Add appropriate language pack (e.g., `tesseract-ocr-spa` for Spanish)
4. Adjust Tesseract parameters (PSM mode)

### Problem: Images not appearing in vault

**Solution:**
1. Verify vault path: `echo $OBSIDIAN_VAULT_PATH`
2. Check permissions: `ls -la ~/obsidian-vault/`
3. Look for errors in agent log: `hermes logs --level ERROR`

### Problem: Skill not triggering

**Solution:**
1. Verify WhatsApp bridge is running: `systemctl status hermes-gateway`
2. Check if images are being received: `tail -f ~/.hermes/logs/gateway.log`
3. Test with a simple message containing an image

## Integration

### With URL Ingestor

This skill complements the existing URL ingestor, providing comprehensive content capture from WhatsApp.

### With Knowledge Base

Automatically triggers `kb-compile` when new content is added.

## Future Enhancements

### Planned Features

- [ ] Multi-language OCR support
- [ ] Image preprocessing (deskew, denoise, enhance)
- [ ] OCR confidence scoring and highlighting
- [ ] Automatic image tagging and classification
- [ ] Save original image files (configurable)
- [ ] Support for other media types (PDF, document OCR)

### Backlog

- [ ] Batch processing for multiple images
- [ ] Image metadata extraction (EXIF, geolocation)
- [ ] Integration with cloud OCR services (Google Vision, Azure)
- [ ] Audio transcription for voice messages
- [ ] Video transcription and analysis

## Version History

| Version | Date | Changes |
|---------|------|---------|
| 1.0 | 2026-05-09 | Initial release - basic OCR processing |
| 1.1 | 2026-05-10 | Added error handling and logging improvements |
| 1.2 | 2026-05-11 | Added support for multiple languages |

## See Also

- [WhatsApp Bridge Documentation](../gateway/platforms/whatsapp.py)
- [Tesseract OCR User Guide](https://github.com/tesseract-ocr/tesseract/wiki)
- [Obsidian Vault Structure](https:// obsidian.md/Help)