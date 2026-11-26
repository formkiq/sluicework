"""Create user-owned Open WebUI login agent after an explicit installer opt-in."""

import hashlib
import json
import os
from pathlib import Path
import plistlib
import secrets
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit


def validate_origin(value):
    parsed = urlsplit(value)
    if (
        parsed.scheme not in ("http", "https")
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("", "/")
        or parsed.query
        or parsed.fragment
    ):
        raise ValueError("Use an HTTP(S) server origin without credentials or paths")
    if parsed.scheme == "http" and parsed.hostname not in ("localhost", "127.0.0.1", "::1"):
        raise ValueError("Remote Ollama endpoints require HTTPS")
    _ = parsed.port  # Validate the port before writing any files.


def load_or_create_secret(state):
    secret_path = state / "secret.key"
    if os.path.lexists(secret_path):
        if secret_path.is_symlink():
            raise ValueError("Refusing to read symlinked secret key")
        value = secret_path.read_text().strip()
        if len(value) < 32:
            raise ValueError("Existing secret is invalid; restore a valid backup instead of replacing it")
        secret_path.chmod(0o600)
        return value
    value = secrets.token_hex(32)
    fd = os.open(secret_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(fd, (value + "\n").encode("ascii"))
        os.fsync(fd)
    finally:
        os.close(fd)
    return value


def agent_specs(state, ollama, ca, project, secret_key=None):
    validate_origin(ollama)
    if secret_key is None:
        secret_key = load_or_create_secret(state)
    env = {
        "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
        "DATA_DIR": str(state / "data"),
        "OLLAMA_BASE_URL": ollama.rstrip("/"),
        "WEBUI_AUTH": "true",
        "USE_SLIM_DOCKER": "true",
        "OFFLINE_MODE": "true",
        "ENABLE_OLLAMA_API": "true",
        "ENABLE_OPENAI_API": "false",
        "ANONYMIZED_TELEMETRY": "false",
        "DO_NOT_TRACK": "true",
        "SCARF_NO_ANALYTICS": "true",
        "WEBUI_SECRET_KEY": secret_key,
    }
    if ca:
        if not Path(ca).is_file():
            raise ValueError("CA certificate is missing")
        env["SSL_CERT_FILE"] = ca
    label = "com.sluicework.open-webui"
    return {
        label: {
            "Label": label,
            "ProgramArguments": [
                str(project / ".venv/bin/open-webui"),
                "serve",
                "--host",
                "127.0.0.1",
                "--port",
                "8080",
            ],
            "EnvironmentVariables": env,
            "WorkingDirectory": str(state),
            "RunAtLoad": True,
            "KeepAlive": True,
            "ThrottleInterval": 15,
            "StandardOutPath": str(state / "logs" / (label + ".out.log")),
            "StandardErrorPath": str(state / "logs" / (label + ".err.log")),
        }
    }


def install_agents(state, specs, agent_dir, uid, run=subprocess.run):
    manifest = state / "launchd-manifest.json"
    if manifest.exists():
        print("Open WebUI startup is already configured. Existing agent was preserved.")
        return
    domain = f"gui/{uid}"
    label = "com.sluicework.open-webui"
    target = agent_dir / (label + ".plist")
    if os.path.lexists(target) or run(
        ["/bin/launchctl", "print", domain + "/" + label],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    ).returncode == 0:
        raise RuntimeError(f"Existing agent {label} is not managed by this installation")
    agent_dir.mkdir(parents=True, exist_ok=True)
    created = []
    started = []
    try:
        for name, spec in specs.items():
            plist_path = agent_dir / (name + ".plist")
            payload = plistlib.dumps(spec)
            with plist_path.open("xb") as handle:
                created.append((plist_path, payload))
                handle.write(payload)
            plist_path.chmod(0o600)
            run(["/bin/launchctl", "enable", domain + "/" + name], check=True)
            started.append(name)
            run(["/bin/launchctl", "bootstrap", domain, str(plist_path)], check=True)
        with tempfile.NamedTemporaryFile(mode="w", dir=state, prefix=".launchd-manifest-", delete=False) as handle:
            candidate = Path(handle.name)
            json.dump({"agents": {str(p): hashlib.sha256(b).hexdigest() for p, b in created}}, handle)
            handle.flush()
            os.fsync(handle.fileno())
        try:
            os.link(candidate, manifest)
        finally:
            candidate.unlink()
    except Exception:
        for name in reversed(started):
            run(["/bin/launchctl", "bootout", domain + "/" + name], check=False)
        for plist_path, payload in created:
            if plist_path.exists() and plist_path.read_bytes() == payload:
                plist_path.unlink()
        raise
    print("Open WebUI user agent started. It will start again when this user logs in.")


def main(args):
    if os.geteuid() == 0:
        raise RuntimeError("Run Open WebUI setup as the desktop user")
    if len(args) != 4:
        raise ValueError("Expected state, Ollama URL, CA and project")
    state, ollama, ca, project = args
    state, project = Path(state), Path(project)
    if not state.is_absolute() or not project.is_absolute():
        raise ValueError("Setup directories must be absolute")
    for name in ("logs", "data"):
        (state / name).mkdir(parents=True, exist_ok=True)
    specs = agent_specs(state, ollama, ca, project)
    install_agents(state, specs, Path.home() / "Library/LaunchAgents", os.getuid())


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except Exception as exc:
        print(f"Open WebUI setup failed: {exc}", file=sys.stderr)
        sys.exit(1)
