#!/usr/bin/env python3
"""Portable local development entry point. No host Go/Node/media tools required."""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import time
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parent.parent


def read_env(path):
    values = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.strip() and not line.lstrip().startswith("#"):
            key, separator, value = line.partition("=")
            if not separator or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key.strip()):
                raise ValueError("Invalid private env file; use KEY=value without shell commands.")
            values[key.strip()] = value.strip().strip("\"'")
    return values


def initialize(root, web_port, s3_port):
    if not (1024 <= web_port <= 65535 and 1024 <= s3_port <= 65535) or web_port == s3_port:
        raise ValueError("Choose two different unprivileged ports in [1024, 65535].")
    directory = root / ".runtime" / "container"
    targets = [directory / name for name in ("env", "config.yaml", "s3.json")]
    existing = [path.exists() for path in targets]
    if all(existing):
        print("Existing container configuration preserved. Edit .runtime/container/config.yaml and env as needed.")
        return
    if any(existing):
        raise ValueError("Container configuration is incomplete; existing files were preserved. Restore the missing files before continuing.")
    directory.mkdir(parents=True, mode=0o700, exist_ok=True)
    password, access, key, redis_password = [secrets.token_hex(24) for _ in range(4)]
    origin = f"http://127.0.0.1:{web_port}"
    env = (
        f"VA_COMPOSE_PROJECT=vedioagent-{secrets.token_hex(4)}\n"
        f"VA_WEB_PORT={web_port}\nVA_S3_PORT={s3_port}\nVA_PUBLIC_ORIGIN={origin}\n"
        f"MYSQL_ROOT_PASSWORD={password}\n"
        f"VA_MYSQL_DSN=root:{password}@tcp(mysql:3306)/video_agent?parseTime=true&loc=UTC\n"
        f"VA_REDIS_PASSWORD={redis_password}\nVA_S3_ACCESS_KEY={access}\nVA_S3_SECRET_KEY={key}\n"
        "# Cloud API credentials: fill your own values here, never commit this file.\n"
        "VA_MODEL_API_KEY=\nVA_ASR_SECRET_ID=\nVA_ASR_SECRET_KEY=\nVA_COS_SECRET_ID=\nVA_COS_SECRET_KEY=\n"
    )
    config = (root / "configs" / "container.example.yaml").read_text(encoding="utf-8")
    config = config.replace("http://127.0.0.1:5173", origin).replace("http://127.0.0.1:18333", f"http://127.0.0.1:{s3_port}")
    identity = {"identities": [{"name": "vedioagent-local", "credentials": [{"accessKey": access, "secretKey": key}], "actions": ["Admin", "Read", "Write", "List", "Tagging"]}]}
    for path, content in zip(targets, (env, config, json.dumps(identity, indent=2) + "\n")):
        # Never replace a developer's existing keys or settings.
        mode = 0o644 if path.name == "config.yaml" else 0o600
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
        if hasattr(os, "fchmod"):
            os.fchmod(descriptor, mode)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(content)
    print("Created private .runtime/container/{env,config.yaml,s3.json}. No keys printed.")
    print("Cloud providers are disabled until you fill your credentials and enable them in config.yaml.")


def compose_command(root, engine, values):
    executable = shutil.which(engine) if engine else (shutil.which("docker") or shutil.which("podman"))
    if not executable:
        raise ValueError("Install Docker Engine/Desktop with Compose v2, or Podman with a Compose provider.")
    project = values.get("VA_COMPOSE_PROJECT", "")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]+", project):
        raise ValueError("Invalid VA_COMPOSE_PROJECT in private env file.")
    return [executable, "compose", "--project-name", project, "--env-file", str(root / ".runtime/container/env"), "-f", str(root / "deploy/compose.local.yaml")]


def redact(message, values):
    sensitive = [value for key, value in values.items() if value and re.search(r"PASSWORD|SECRET|KEY|TOKEN|DSN", key)]
    for value in sorted(sensitive, key=len, reverse=True):
        message = message.replace(value, "[REDACTED]")
    return message


def run(command, values):
    # Some Podman Compose providers print docker/podman arguments including env values.
    environment = os.environ.copy()
    environment.update(values)
    with subprocess.Popen(command, cwd=ROOT, env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True) as child:
        try:
            for line in child.stdout:
                print(redact(line.rstrip(), values), flush=True)
            code = child.wait()
        except KeyboardInterrupt:
            child.terminate()
            child.wait()
            raise
    if code:
        raise ValueError(f"Compose command failed (exit {code}); inspect the redacted output above.")


def wait_ready(origin, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with urlopen(origin + "/readyz", timeout=2) as response:
                if json.load(response).get("status") == "ready":
                    print(f"vedioagent is ready: {origin}")
                    return
        except (OSError, ValueError):
            pass
        time.sleep(1)
    raise ValueError("Application did not become ready; run local.py logs and inspect bootstrap/API logs.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["init", "up", "down", "logs", "status", "check", "provider-check"])
    parser.add_argument("--engine", choices=["docker", "podman"])
    parser.add_argument("--web-port", type=int, default=5173, help="init only")
    parser.add_argument("--s3-port", type=int, default=18333, help="init only")
    parser.add_argument("--no-build", action="store_true", help="up: reuse previously built application images")
    args = parser.parse_args()
    if args.action == "init":
        initialize(ROOT, args.web_port, args.s3_port)
        return 0
    env_path = ROOT / ".runtime/container/env"
    if not env_path.is_file():
        raise ValueError("Run python3 deploy/local.py init first.")
    values = read_env(env_path)
    config_path = env_path.with_name("config.yaml")
    if os.name != "nt" and config_path.stat().st_mode & 0o004 == 0:
        raise ValueError("The container UID must read config.yaml: run chmod 644 .runtime/container/config.yaml. Keep env and s3.json private (0600).")
    command = compose_command(ROOT, args.engine, values)
    if args.action == "up":
        # Reload bind-mounted configuration and refresh Nginx's upstream DNS.
        # Only application labels change; middleware and its volumes stay intact.
        values["VA_LOCAL_RUN"] = secrets.token_hex(8)
        if not args.no_build:
            run(command + ["build", "api", "web"], values)
        run(command + ["up", "-d", "--no-build"], values)
        wait_ready(values["VA_PUBLIC_ORIGIN"], 180)
        print("No sign-up or password needed. Cloud AI becomes available after configuring your own APIs.")
    elif args.action == "down":
        run(command + ["down"], values)
        print("Stopped this local project. Data volumes and private settings are preserved.")
    elif args.action == "logs":
        run(command + ["logs", "--tail", "100"], values)
    elif args.action == "status":
        run(command + ["ps"], values)
    else:
        run(command + ["run", "--rm", "--no-deps", "--entrypoint", "devtool", "api", "-config", "/app/configs/local.yaml", "-action", args.action], values)
        if args.action == "check":
            wait_ready(values["VA_PUBLIC_ORIGIN"], 5)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, OSError) as failure:
        print(str(failure), file=sys.stderr)
        raise SystemExit(1)
    except KeyboardInterrupt:
        raise SystemExit(130)
