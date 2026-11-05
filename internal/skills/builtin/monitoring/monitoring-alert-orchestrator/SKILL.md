---
name: monitoring-alert-orchestrator
description: Design, implement, and debug orchestrator patterns that dispatch alerts to multiple platforms (Slack, Telegram, Discord, Email, etc). Covers architecture, wrapper scripts, exit-code vs output-based detection, and common failure modes.
trigger:
  - Any task involving "orchestrator", "dispatcher", or "multi-platform alert" or "cross-platform notification" system
  - Building or fixing scripts that coordinate delivery to Slack/Telegram/Discord
  - Debugging why cron jobs "produce no output" despite successful delivery
  - Designing alert pipelines with platform-specific wrappers
examples:
  - "Fix stock alert orchestrator that exits 0 even when alerts sent"
  - "Build a multi-channel notification system"
  - "Debug why cron job shows no output but platforms received messages"
  - "Design wrapper scripts for platform delivery"
classification: monitoring / systems
version: 0.1.0
---

# Monitoring Alert Orchestrator

Comprehensive guidance for building, debugging, and maintaining orchestrator systems that dispatch alerts to multiple platforms (Slack, Telegram, Discord, email, etc.).

## Architecture Pattern

An **orchestrator** (sometimes called a "triple" or "dispatcher") coordinates platform-specific deliverers:

```
orchestrator.py
├─> deliver_slack.py   (wrapper)
├─> deliver_telegram.py (wrapper)
└─> deliver_discord.sh (wrapper)
```

Each **wrapper** does:
1. Run the actual alert/content generator script (the "producer")
2. Capture its stdout (the alert message)
3. Send the message to its platform via HTTP API / CLI
4. Exit with a code that signals whether an alert was delivered

## Critical Pitfall: Detection Bug

**Symptom:** Cron job "runs successfully but produces no output" even when platforms receive alerts.

**Root cause:** Wrappers consume the producer's output and exit with the producer's exit code instead of signaling delivery success.

```python
# WRONG — default behavior most scripts start with
result = subprocess.run([sys.executable, "producer.py"], capture_output=True, text=True)
output = result.stdout.strip()
exit_code = result.returncode

# Send alert if there's output
if output:
    send_to_platform(output)
    # BUG: exit_code is still the producer's code (often 0)
sys.exit(exit_code)  # ← loses delivery signal
```

**Consequence:** Orchestrator checks `if wrapper_exit_code == 1 or wrapper_stdout.strip()` — both false → thinks no alert sent.

**Two correct patterns:**

### Pattern A — Exit-code signaling (preferred for cron)
Wrappers exit with code **1** after successful delivery to indicate "alert sent":

```python
if output:
    send_to_platform(output)
    sys.exit(1)   # signal delivered
else:
    sys.exit(0)   # nothing delivered
```

Orchestrator detects via exit code; cron still sees exit 1 but that's acceptable — deliverer succeeded.

### Pattern B — Pass-through output
Wrappers **also print** the alert message to their own stdout after sending:

```python
if output:
    send_to_platform(output)
    print(output)   # make orchestrator aware
sys.exit(exit_code)
```

Orchestrator detects via non-empty stdout; exit code remains 0 for "success".

### Pattern C — Dual-channel (robust)
Combine both for explicit signaling:

```python
if output:
    send_to_platform(output)
    print("ALERT_SENT")  # sentinel
    sys.exit(1)
else:
    print("NO_ALERT")
    sys.exit(0)
```

## Orchestrator Contract

The orchestrator relies on a **contract** with wrappers. Choose ONE convention and document it:

| Convention | Orchestrator check | Cron exit | Notes |
|------------|-------------------|-----------|-------|
| Exit-1-on-deliver | `if rc == 1` | 1 (visible) | Simple, unambiguous |
| Pass-through output | `if stdout.strip()` | 0 (clean) | Keeps cron happy |
| Sentinel stdout | `if "ALERT_SENT" in stdout` | 0 | Explicit but verbose |

**Recommendation:** Use **Pattern B** (pass-through output). It preserves cron success semantics while providing a reliable detection signal. Document: "Wrappers must print the alert message to stdout after successful delivery."

## Platform Credential Loading

Multi-platform scripts often share `.env` loading code. Factor this into a helper:

```python
def load_dotenv(path="/opt/data/.env"):
    import os
    if os.path.isfile(path):
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith('#'): continue
                if '=' in line:
                    k, _, v = line.partition('=')
                    k = k.strip()
                    if k.startswith('export '): k = k[7:].strip()
                    if k and k not in os.environ: os.environ[k] = v.strip()
```

All wrappers should call this BEFORE running the producer OR inherit from orchestrator. Ensure orchestrator loads `.env` first and passes environment to subprocesses:

```python
subprocess.run([interpreter, wrapper], env=os.environ, ...)
```

## Cron Job Status vs Delivery

- **Hermes cron** tracks job status via exit code and can capture output (when `deliver=local`).
- **Traditional crontab** typically redirects to `/dev/null`; exit code is only signal.
- If your orchestrator exits 1 to indicate "alert delivered", cron may mark the job as "error" in its logs — this is **expected** and acceptable. Consider:
  - Using `|| true` in crontab to force exit 0 if desired
  - Or rely on output-based detection (Pattern B) to keep exit 0

## Debugging Checklist

When orchestrator "produces no output":

- [ ] Run the producer directly: `python3 producer.py` — does it output an alert?
- [ ] Run each wrapper individually from the **same cwd** cron uses — does it send?
- [ ] Check wrapper's **exit code** and **stderr** for errors
- [ ] Verify wrapper captures producer output (`result.stdout.strip()` truthy?)
- [ ] Confirm wrapper actually **prints** the alert to its own stdout if using Pattern B
- [ ] Validate platform credentials (token, channel, permissions)
- [ ] Inspect `.env` loading — are env vars present in the subprocess?
- [ ] Check file paths — wrappers often use bare filenames; ensure producer is PATH-accessible or pass absolute path

## Fixing a Detection Bug (Quick Patch)

If wrappers currently exit with producer's code:

```diff
- sys.exit(exit_code)
+ sys.exit(1 if output else 0)
```

Or, to preserve exit code but signal:

```diff
- sys.exit(exit_code)
+ if output:
+     print(output)   # forward to orchestrator stdout
+ sys.exit(exit_code)
```

For Bash wrappers (Discord/Telegram shell scripts):

```bash
if [ -n "$OUTPUT" ]; then
    echo "$OUTPUT"      # forward to stdout
    curl ... && exit 1  # signal delivered
else
    exit 0
fi
```

## References

See `references/` directory for session-specific details:
- `detection-bug-pattern.md` — full analysis of the exit-code vs output ambiguity
- `wrapper-fix-examples.md` — before/after patches for Python and Bash wrappers
- `hermes-cron-integration.md` — how Hermes cron interprets exit codes and output
