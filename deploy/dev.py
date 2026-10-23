#!/usr/bin/env python3
"""Run the local application processes together; infrastructure stays independent."""
import argparse
import fcntl
import json
import os
import signal
import subprocess
import time
from pathlib import Path
from urllib.parse import urlsplit


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default="configs/local.yaml")
    parser.add_argument("--no-web", action="store_true", help="keep an already running Vite server")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    os.chdir(root)
    runtime = root / ".runtime"
    runtime.mkdir(mode=0o700, exist_ok=True)
    lock = (runtime / "dev.lock").open("w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        raise SystemExit("The local application supervisor is already running.")
    binaries = runtime / "bin"
    binaries.mkdir(mode=0o700, exist_ok=True)
    subprocess.run(["go", "build", "-o", str(binaries) + "/", "./cmd/..."], check=True)
    config_args = ["-config", args.config]
    web_config = json.loads(subprocess.check_output([str(binaries / "devtool"), *config_args, "-action", "web-config"]))
    origin = urlsplit(web_config["public_origin"])
    if origin.scheme != "http" or origin.hostname not in ("localhost", "127.0.0.1", "::1"):
        raise SystemExit("make dev requires an HTTP loopback public_origin. Use deployment tooling for public hosting.")
    for action in ["migrate", "init-storage", "check"]:
        subprocess.run([str(binaries / "devtool"), *config_args, "-action", action], check=True)
    commands = [
        ("api", [str(binaries / "api"), *config_args], root),
        ("media", [str(binaries / "worker"), *config_args, "-kind", "media"], root),
        ("agent", [str(binaries / "worker"), *config_args, "-kind", "agent"], root),
        ("coordinator", [str(binaries / "coordinator"), *config_args], root),
    ]
    if not args.no_web:
        commands.append(("web", ["npm", "run", "dev", "--", "--host", origin.hostname, "--port", str(origin.port or 80), "--strictPort"], root / "web"))
    children = []
    stopped = False

    def stop(_signum, _frame):
        nonlocal stopped
        stopped = True

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    try:
        for name, command, cwd in commands:
            log_path = runtime / f"dev-{name}.log"
            descriptor = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            with os.fdopen(descriptor, "ab") as log:
                env = os.environ.copy()
                if name == "web":
                    env = {key: value for key, value in env.items() if not key.startswith(("VA_", "MYSQL_", "AWS_"))}
                    env["VEDIOAGENT_API_TARGET"] = web_config["api_target"]
                child = subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=log, start_new_session=True)
            children.append((name, child))
            print(f"Started {name}; log: {log_path}", flush=True)
        print(f"vedioagent: {web_config['public_origin']} — Ctrl+C stops these application processes.", flush=True)
        while not stopped:
            for name, child in children:
                if child.poll() is not None:
                    print(f"{name} exited with code {child.returncode}; inspect its local log.", flush=True)
                    return 1
            time.sleep(0.5)
        return 0
    finally:
        # Only process groups launched by this invocation are touched.
        for _, child in children:
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
        for _, child in children:
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                child.wait()


if __name__ == "__main__":
    raise SystemExit(main())
