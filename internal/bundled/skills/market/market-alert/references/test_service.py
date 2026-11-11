#!/usr/bin/env python3
"""
Market Alert Service - Test Script
Tests the complete service end-to-end.
"""

import subprocess
import sys
import os
import time

market_dir = os.path.expanduser('~/hermes/market-alerts')

print("🧪 Testing Market Alert Service...")
print(f"📁 Market directory: {market_dir}")

# Test 1: Check configuration file exists
if os.path.exists(os.path.join(market_dir, "config.json")):
    print("✅ 1. Configuration file exists")
else:
    print("❌ 1. Configuration file missing")
    sys.exit(1)

# Test 2: Check service script exists
service_script = os.path.join(market_dir, "market_alert_service.py")
if os.path.exists(service_script):
    print("✅ 2. Service script exists")
else:
    print("❌ 2. Service script missing")
    sys.exit(1)

# Test 3: Run a single processing cycle
print("\n🧪 Test 3: Running a single processing cycle...")
result = subprocess.run([sys.executable, service_script], 
                       capture_output=True, text=True, cwd=market_dir)
if result.returncode == 0:
    print("✅ 3. Single cycle ran successfully")
    if "Cycle complete" in result.stdout:
        print("   ✓ Cycle completed successfully")
    else:
        print(f"   Output: {result.stdout[:200]}...")
else:
    print(f"❌ 3. Single cycle failed")
    print(f"   Error: {result.stderr}")
    sys.exit(1)

# Test 4: Check data files created
data_dir = os.path.join(market_dir, "data")
if os.path.exists(os.path.join(data_dir, "latest_fetch.json")):
    print("✅ 4. Data file created")
else:
    print("❌ 4. Data file not created")
    sys.exit(1)

print("\n" + "="*60)
print("🎉 All tests passed! Market Alert Service is working.")
print("="*60)