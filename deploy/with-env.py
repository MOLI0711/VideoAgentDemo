#!/usr/bin/env python3
"""Run a command with a private env file. Never evaluate shell substitutions."""
import os
import subprocess
import signal
import sys
from pathlib import Path

if len(sys.argv) < 3:
    raise SystemExit("usage: python3 deploy/with-env.py .runtime/dev.env COMMAND [ARG ...]")
env = os.environ.copy()
for line in Path(sys.argv[1]).read_text().splitlines():
    if not line.strip() or line.lstrip().startswith("#"):
        continue
    key, value = line.split("=", 1)
    env[key.strip()] = value.strip().strip("'\"")
child = subprocess.Popen(sys.argv[2:], env=env)
try:
    status = child.wait()
except KeyboardInterrupt:
    # Allow a supervised command to stop its own children before exiting.
    try:
        child.send_signal(signal.SIGINT)
    except ProcessLookupError:
        pass
    try:
        child.wait(timeout=30)
    except subprocess.TimeoutExpired:
        child.terminate()
        try:
            child.wait(timeout=5)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait()
    status = 130
raise SystemExit(status)
