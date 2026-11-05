# Actual Trakt Watcher Implementation

The working Trakt watcher script is located at `/opt/data/home/trakt_watcher.py`. This standalone Python script should be used for all future runs.

## Key Characteristics

- **Location**: `/opt/data/home/trakt_watcher.py`
- **Configuration**: Hardcoded defaults in the script:
  - `trakt_client_id`: `dc22389fbad6bb350eb7b3714d21a71d0a16c35cba461c375728d99f55c3de59`
  - `trakt_client_secret`: `aa64d05b2812c0ad9e1a3430b199b933ea5bd0cb4671c45b65026c744fa827a3`
  - `trakt_access_token`: Loaded from `~/.hermes/tokens/trakt.json` if config file not used
  - `trakt_refresh_token`: Loaded from `~/.hermes/tokens/trakt.json` if config file not used
- **State Management**: State is saved to `~/.hermes/skills/trakt-watcher/state.json`
- **Output**: 
  - Debug information goes to stderr
  - Notifications are sent as JSON lines to stdout in the format: `{"target": "channel:id", "message": "notification text"}`
- **Behavior**: Checks the user's current watching status on Trakt. If watching something new or with progress, sends notifications. If stopped watching, sends a stopped notification. If nothing changes, outputs `[SILENT]`.

## Usage

Execute with: `python3 /opt/data/home/trakt_watcher.py`

The script will:
1. Load configuration (preferring a config file at `~/.hermes/skills/trakt-watcher/config.json` if it exists)
2. Load tokens from `~/.hermes/tokens/trakt.json`
3. Check the user's watching status via the Trakt API
4. Compare with previous state
5. Send notifications if there's a change
6. Save state for next run

## Troubleshooting

- The implementation uses only Python's standard library for HTTP, so no `requests` install is needed.
- Check that `~/.hermes/tokens/trakt.json` exists and contains valid tokens.
- For debug output, add print statements as shown in `references/debug-output.md`.
- The script will output `[SILENT]` if there's nothing new to report, which is the expected behavior for cron jobs.

## History

This script was discovered during a cron job execution on May 6, 2026. It supersedes the earlier referenced script at `/opt/data/skills/media/trakt-watcher/references/trakt_watcher.py`.