# Fallback Mechanism for YouTube Link Ingestor

When Hermes tools are unavailable (e.g., during system maintenance, network issues, or misconfiguration), the YouTube Link Ingestor automatically switches to a direct file scanning fallback to ensure continuous operation.

## Trigger Conditions
The fallback is activated when:
- `hermes sessions list` tool call fails or returns an error
- Hermes CLI is not available in the environment
- Network connectivity to Hermes services is interrupted

## Fallback Implementation

### File Scanning Strategy
1. **Target Directory**: Scans the Obsidian vault wiki directory (`obsidian-vault/Vault/wiki/`)
2. **File Types**: Processes all `.md` files containing conversation history
3. **Regex Pattern**: Uses the same robust regex as the primary method:
   ```
   (?:https?://)?(?:www\.)?(?:youtube\.com/watch\?v=|youtu\.be/)([a-zA-Z0-9_-]{11})
   ```
4. **Link Extraction**: Converts video IDs to full youtu.be short links for consistency

### Advantages of Fallback Approach
- **Zero Dependency**: Doesn't require Hermes tools or network access
- **Simple & Reliable**: Direct file I/O is fast and dependable
- **Consistent Output**: Produces identical link format to primary method
- **Gradual Migration**: Allows incremental rollout of Hermes tool integration

### Limitations
- **No Conversation Context**: Cannot extract link text from surrounding conversation
- **Limited History**: Only scans files currently in the vault wiki
- **No Session Filtering**: Processes all markdown files, not just recent sessions

## Configuration

### Enabling Fallback
The fallback is automatically enabled when the primary method fails. No additional configuration needed.

### Customizing Fallback Behavior
To modify fallback behavior, edit the `save_youtube_links.py` script:

```python
# Adjust target directory if vault structure differs
WIKI_DIR = Path("/opt/data/home/obsidian-vault/Vault/wiki")

# Modify regex pattern if needed
YOUTUBE_PATTERN = r'(?:https?://)?(?:www\.)?(?:youtube\.com/watch\?v=|youtu\.be/)([a-zA-Z0-9_-]{11})'
```

## Testing Fallback

### Manual Test
Simulate tool unavailability by renaming or removing the Hermes CLI:

```bash
mv /opt/hermes/.venv/bin/hermes /opt/hermes/.venv/bin/hermes.bak
python3 save_youtube_links.py
```

Verify that links are still extracted and saved correctly.

### Monitoring
Check cron job logs for fallback activation:

```
Hermes tools not available, using fallback
Scanning conversation history for YouTube links...
Using fallback method - no Hermes tools available
```

This indicates the fallback is active.

## Best Practices

1. **Prefer Primary Method**: Use Hermes tools when available for richer context
2. **Monitor Fallback Usage**: Frequent fallback activation may indicate tool issues
3. **Keep Both Methods Updated**: Ensure regex patterns and logic stay consistent
4. **Gradual Enhancement**: Start with fallback, then add primary method as tools become available

## Integration with Other Systems

The fallback mechanism follows the same output format as the primary method, ensuring seamless integration with:
- Obsidian vault storage
- Deduplication logic
- Cron delivery pipelines

This makes the fallback completely transparent to downstream consumers.

## Troubleshooting

| Symptom | Likely Cause | Solution |
|---------|-------------|----------|
| No links found in fallback | Wiki directory empty or no markdown files | Verify vault structure and file existence |
| Duplicate links appearing | Wiki files contain links already in YouTube-Links.md | Ensure proper deduplication logic |
| Permission errors | Script lacks read access to wiki files | Adjust file permissions or run as appropriate user |
| Regex not matching | YouTube links in non-standard format | Update regex pattern to handle variations |

## Future Enhancements
- Add support for scanning additional file types (e.g., `.txt`, `.html`)
- Implement smarter conversation detection (e.g., based on file naming patterns)
- Cache previously seen links to improve performance on large vaults
- Add verbose logging for debugging fallback behavior