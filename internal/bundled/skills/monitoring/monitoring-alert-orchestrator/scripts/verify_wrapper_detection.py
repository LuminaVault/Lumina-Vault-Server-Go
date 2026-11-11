#!/usr/bin/env python3
"""
Verify wrapper detection behavior for alert orchestrator systems.

Tests that each wrapper:
  1. Runs the stock producer successfully (output = alert message)
  2. Sends the alert via platform API (mocked or real)
  3. Exits 0 but prints output to stdout (Pass-through Pattern) OR exits 1 (Exit-code pattern)
  4. Orchestrator would correctly detect the alert

Usage:
  python3 verify_wrapper_detection.py --pattern pass-through
  python3 verify_wrapper_detection.py --pattern exit-code
  python3 verify_wrapper_detection.py --all

Assumes .env credentials are present and platforms are reachable.
"""

import subprocess, sys, os, argparse, json

# Paths to test
STOCK_SCRIPT = "/opt/data/home/.hermes/scripts/stock_threshold_alert.py"
WRAPPERS = [
    ("slack", "/opt/data/home/.hermes/scripts/stock_alert_slack.py", "python3"),
    ("telegram", "/opt/data/home/.hermes/scripts/stock_alert_telegram.py", "python3"),
    ("discord", "/opt/data/home/.hermes/scripts/stock_alert_discord.sh", "bash"),
]

def load_env():
    env_path = "/opt/data/.env"
    if os.path.isfile(env_path):
        with open(env_path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith('#'): continue
                if '=' in line:
                    k, _, v = line.partition('=')
                    k = k.strip()
                    if k.startswith('export '): k = k[7:].strip()
                    if k and k not in os.environ: os.environ[k] = v.strip()

def test_wrapper(name, wrapper_path, interpreter, pattern):
    """Run wrapper and report detection-relevant metrics."""
    print(f"\n{'='*60}")
    print(f"Testing: {name} — {wrapper_path}")
    print('='*60)

    cmd = [interpreter, wrapper_path]
    # For discord bash script, it calls python3 internally — ensure same python
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=30, env=os.environ)

    print(f"Exit code: {r.returncode}")
    print(f"stdout length: {len(r.stdout)}")
    print(f"stderr (last 300 chars): {r.stderr[-300:]}")

    has_output = bool(r.stdout.strip())
    alert_detected = False

    if pattern == "pass-through":
        alert_detected = has_output
        if has_output:
            print("✅ PASS: Wrapper forwarded alert to stdout (orchestrator will see it)")
        else:
            print("❌ FAIL: No stdout output — orchestrator cannot detect delivery")
    elif pattern == "exit-code":
        alert_detected = (r.returncode == 1)
        if r.returncode == 1:
            print("✅ PASS: Wrapper exited 1 (orchestrator sees delivery)")
        else:
            print("❌ FAIL: Wrapper did not exit 1")
    else:
        # sentinel mode
        alert_detected = "ALERT_SENT" in r.stdout
        print(f"Sentinel check: {alert_detected}")

    # Additional checks
    if "not set" in r.stderr.lower():
        print("⚠️  Credential missing in wrapper")
    if "error" in r.stderr.lower() or "failed" in r.stderr.lower():
        print("⚠️  Platform error in stderr")

    return alert_detected

def main():
    load_env()
    parser = argparse.ArgumentParser(description="Verify wrapper detection")
    parser.add_argument("--pattern", choices=["pass-through", "exit-code", "sentinel"],
                        default="pass-through", help="Detection pattern to check")
    parser.add_argument("--all", action="store_true", help="Test all wrappers")
    args = parser.parse_args()

    print("=== Alert Wrapper Detection Verification ===")
    print(f"Pattern: {args.pattern}")
    print(f"Stock producer: {STOCK_SCRIPT}")

    results = {}
    for name, path, interp in WRAPPERS:
        try:
            detected = test_wrapper(name, path, interp, args.pattern)
            results[name] = detected
        except Exception as e:
            print(f"ERROR testing {name}: {e}")
            results[name] = False

    print("\n" + "="*60)
    print("SUMMARY")
    print("="*60)
    for name, ok in results.items():
        status = "✅ DETECTED" if ok else "❌ NOT DETECTED"
        print(f"  {name}: {status}")

    all_ok = all(results.values())
    if all_ok:
        print("\nAll wrappers correctly signal delivery to orchestrator.")
        sys.exit(0)
    else:
        print("\nSome wrappers fail to signal — review and apply fixes from skill.")
        sys.exit(1)

if __name__ == "__main__":
    main()
