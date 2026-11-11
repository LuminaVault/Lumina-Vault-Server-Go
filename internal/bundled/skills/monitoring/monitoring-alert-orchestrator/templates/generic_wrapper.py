#!/usr/bin/env python3
"""
Generic Alert Wrapper Template — Cross-platform (Slack/Telegram/Discord-style)

Usage:
  python3 generic_wrapper.py <producer_script> [args...]

This template implements the correct pattern:
  1. Runs a producer script that prints the alert message
  2. If output is non-empty, sends it to a platform via HTTP
  3. After successful send, prints the output to stdout (orchestrator detection)
  4. Exits 0 on success, 1 on failure

To adapt:
  - Fill in `send_to_platform()` with your platform's API call
  - Optionally change detection method (exit code vs stdout)
  - Adjust authentication: token env var names, channel/chat IDs
"""

import subprocess, sys, os, json, urllib.request

def load_dotenv(path="/opt/data/.env"):
    """Load environment variables from .env file."""
    if os.path.isfile(path):
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith('#'):
                    continue
                if '=' in line:
                    k, _, v = line.partition('=')
                    k = k.strip()
                    if k.startswith('export '):
                        k = k[7:].strip()
                    if k and k not in os.environ:
                        os.environ[k] = v.strip()


def send_to_platform(message: str) -> bool:
    """
    Send `message` to your platform.

    Return True on success, False on failure.
    """
    # Example: Slack
    token = os.environ.get("PLATFORM_BOT_TOKEN")
    channel = os.environ.get("PLATFORM_CHANNEL", "C00000000")

    if not token:
        print("⚠️  PLATFORM_BOT_TOKEN not set", file=sys.stderr)
        return False

    url = "https://platform.example.com/api/chat.postMessage"
    payload = {"channel": channel, "text": message}
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
    }

    try:
        req = urllib.request.Request(
            url, data=json.dumps(payload).encode(), headers=headers, method="POST"
        )
        resp = urllib.request.urlopen(req, timeout=15)
        data = json.loads(resp.read())
        if not data.get("ok"):
            print(f"Platform error: {data.get('error')}", file=sys.stderr)
            return False
        return True
    except Exception as e:
        print(f"Send failed: {e}", file=sys.stderr)
        return False


def main():
    load_dotenv()

    if len(sys.argv) < 2:
        print("Usage: generic_wrapper.py <producer_script> [args...]", file=sys.stderr)
        sys.exit(1)

    producer = sys.argv[1]
    producer_args = sys.argv[2:]

    # Run producer
    result = subprocess.run(
        [sys.executable, producer, *producer_args],
        capture_output=True, text=True,
    )
    output = result.stdout.strip()
    exit_code = result.returncode

    # Alert condition: non-zero exit OR any output (standard in this system)
    if exit_code == 1 or output:
        if send_to_platform(output):
            # CORRECT PATTERN: forward output to orchestrator
            print(output)
            # Option A: keep exit code from producer (0), orchestrator sees stdout
            # Option B: exit 1 to signal delivery
            # sys.exit(1)  # ← uncomment for exit-code signaling
            sys.exit(0)
        else:
            # Send failed — still exit with producer code
            sys.exit(exit_code)
    else:
        # No alert
        sys.exit(exit_code)


if __name__ == "__main__":
    main()
