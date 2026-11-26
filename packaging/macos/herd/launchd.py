"""Create user-owned Herd login agents after an explicit installer opt-in."""

import hashlib
import json
import os
from pathlib import Path
import plistlib
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
        raise ValueError("Remote Herd endpoints require HTTPS")
    _ = parsed.port  # Validate the port before writing any files.


def agent_specs(state, mode, router, ollama, ca, project):
    if mode not in ("router", "node"):
        raise ValueError("Choose router or node")
    validate_origin(router)
    validate_origin(ollama)
    common = {
        "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
        "FLEET_ENV_FILE": str(state / "env"),
        "FLEET_DATA_DIR": str(state / "router-data"),
        "FLEET_NODE_DATA_DIR": str(state / "node-data"),
        "FLEET_NODE_TELEMETRY": "false",
        "FLEET_TELEMETRY": "false",
        "FLEET_AUTO_PULL": "false",
        "FLEET_VRAM_FALLBACK": "false",
        "FLEET_CONTEXT_PROTECTION": "passthrough",
        "FLEET_DISABLE_MODEL_PRELOADER": "true",
        "FLEET_FLEET_INTELLIGENCE": "false",
        "FLEET_THINKING_OVERHEAD": "1.0",
        "FLEET_THINKING_MIN_PREDICT": "0",
        "FLEET_DEBUG_REQUEST_BODIES": "false",
    }
    if ca:
        if not Path(ca).is_file():
            raise ValueError("CA certificate is missing")
        common["SSL_CERT_FILE"] = ca
    programs = {}
    if mode == "router":
        programs["com.sluicework.herd"] = [
            str(project / ".venv/bin/herd"), "--host", "127.0.0.1", "--port", "11435"
        ]
    programs["com.sluicework.herd-node"] = [
        str(project / ".venv/bin/herd-node"),
        "--router-url", router, "--ollama-host", ollama,
    ]
    return {
        label: {
            "Label": label,
            "ProgramArguments": args,
            "EnvironmentVariables": common,
            "WorkingDirectory": str(state),
            "RunAtLoad": True,
            "KeepAlive": True,
            "ThrottleInterval": 15,
            "StandardOutPath": str(state / "logs" / (label + ".out.log")),
            "StandardErrorPath": str(state / "logs" / (label + ".err.log")),
        }
        for label, args in programs.items()
    }


def install_agents(state, specs, agent_dir, uid, run=subprocess.run):
    manifest = state / "launchd-manifest.json"
    if manifest.exists():
        print("Herd startup is already configured. Existing agents were preserved.")
        return
    domain = f"gui/{uid}"
    # Refuse conflicts before writing or starting anything.
    for label in ("com.sluicework.herd", "com.sluicework.herd-node"):
        target = agent_dir / (label + ".plist")
        if os.path.lexists(target) or run(
            ["/bin/launchctl", "print", domain + "/" + label],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
        ).returncode == 0:
            raise RuntimeError(f"Existing agent {label} is not managed by this installation")
    agent_dir.mkdir(parents=True, exist_ok=True)
    created = []
    started = []
    try:
        for label, spec in specs.items():
            target = agent_dir / (label + ".plist")
            payload = plistlib.dumps(spec)
            with target.open("xb") as handle:
                created.append((target, payload))
                handle.write(payload)
            target.chmod(0o600)
            run(["/bin/launchctl", "enable", domain + "/" + label], check=True)
            started.append(label)
            run(["/bin/launchctl", "bootstrap", domain, str(target)], check=True)
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
        for label in reversed(started):
            run(["/bin/launchctl", "bootout", domain + "/" + label], check=False)
        for target, payload in created:
            if target.exists() and target.read_bytes() == payload:
                target.unlink()
        raise
    print("Herd user agents started. They will start again when this user logs in.")


def main(args):
    if os.geteuid() == 0:
        raise RuntimeError("Run Herd setup as the desktop user")
    if len(args) != 6:
        raise ValueError("Expected state, mode, router, Ollama URL, CA and project")
    state, mode, router, ollama, ca, project = args
    state, project = Path(state), Path(project)
    if not state.is_absolute() or not project.is_absolute():
        raise ValueError("Setup directories must be absolute")
    for name in ("logs", "router-data", "node-data"):
        (state / name).mkdir(parents=True, exist_ok=True)
    # Explicit FLEET_ENV_FILE prevents reading an existing ~/.fleet-manager/env.
    if not (state / "env").exists():
        with (state / "env").open("x") as handle:
            handle.write("# Sluicework-managed Herd environment\n")
    specs = agent_specs(state, mode, router, ollama, ca, project)
    install_agents(state, specs, Path.home() / "Library/LaunchAgents", os.getuid())


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except Exception as exc:
        print(f"Herd setup failed: {exc}", file=sys.stderr)
        sys.exit(1)
