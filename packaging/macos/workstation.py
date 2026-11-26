#!/usr/bin/env python3
"""Set up and manage a local Sluicework pilot or a networked worker."""

import argparse
import datetime
import fcntl
import getpass
import http.cookiejar
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import shlex
import signal
import ssl
import hashlib
import platform
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


class SetupError(Exception):
    pass


def display_state(state):
    return "ready" if state == "accepting" else state


def read_json(path):
    with path.open() as source:
        return json.load(source)


def write_json(path, value):
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as out:
        temporary = Path(out.name)
        json.dump(value, out, indent=2)
        out.write("\n")
    try:
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def build_environment():
    env = dict(os.environ)
    additions = [str(Path.home() / ".cargo/bin")]
    brew = shutil.which("brew")
    if brew:
        for args in [["--prefix", "rustup"], ["--prefix"]]:
            result = subprocess.run([brew] + args, capture_output=True, text=True)
            if result.returncode == 0:
                additions.append(str(Path(result.stdout.strip()) / "bin"))
    env["PATH"] = os.pathsep.join(additions + [env.get("PATH", os.defpath)])
    return env


def process_identity(pid, binary, command, config):
    """Do not signal a reused PID or a different installation's server."""
    if not isinstance(pid, int) or pid <= 1:
        return None
    try:
        if sys.platform.startswith("linux"):
            executable = os.readlink(f"/proc/{pid}/exe").removesuffix(" (deleted)")
            arguments = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
            arguments = [os.fsdecode(a) for a in arguments if a]
            matches = arguments[1:] == [command, "--config", str(config)]
        else:
            executable = subprocess.check_output(
                ["/bin/ps", "-ww", "-p", str(pid), "-o", "comm="], text=True
            ).strip()
            arguments = subprocess.check_output(
                ["/bin/ps", "-ww", "-p", str(pid), "-o", "command="], text=True
            ).strip()
            # argv[0] may use /var while the canonical executable is under
            # /private/var on macOS. Compare the executable separately.
            matches = arguments.endswith(f" {command} --config {config}")
        if Path(executable).resolve() != binary.resolve() or not matches:
            return None
        env = dict(os.environ, LC_ALL="C")
        born = subprocess.check_output(
            ["/bin/ps", "-p", str(pid), "-o", "lstart="], text=True, env=env
        ).strip()
        return born or None
    except (OSError, subprocess.CalledProcessError):
        return None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def http_error_302(self, request, response, code, message, headers):
        response.close()
        raise SetupError("Local server redirected a request; refusing to forward credentials")

    http_error_301 = http_error_303 = http_error_307 = http_error_308 = http_error_302


class LocalAPI:
    def __init__(self, origin, ca_file=None):
        self.origin = origin
        self.cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect(),
            urllib.request.HTTPCookieProcessor(self.cookies),
            urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=ca_file)),
        )

    def request(self, method, path, body=None, credential=None):
        headers = {"Content-Type": "application/json", "Origin": self.origin}
        if credential:
            headers["Authorization"] = "Bearer " + credential
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(
            self.origin + "/api/v1" + path, method=method, data=data, headers=headers
        )
        try:
            with self.opener.open(request, timeout=5) as response:
                data = response.read(2 << 20)
                return json.loads(data) if data else None
        except urllib.error.HTTPError as error:
            # Never include response bodies, cookies, passwords, or worker tokens.
            code = error.code
            error.close()
            raise SetupError(f"Local API {method} {path}: HTTP {code}") from None
        except (OSError, ValueError):
            raise SetupError("Cannot reach the configured local server") from None

    def healthy(self):
        try:
            with self.opener.open(self.origin + "/api/v1/openapi.yaml", timeout=1) as response:
                return response.status == 200 and b"Sluicework" in response.read(4096)
        except urllib.error.HTTPError as error:
            error.close()
            return False
        except (OSError, SetupError):
            return False


class Pilot:
    def __init__(self, root, env=None):
        self.root = root.expanduser().resolve()
        self.binary = self.root / "sluicework"
        self.env = build_environment() if env is None else env
        self.server_config = self.root / "server.json"
        self.worker_config = self.root / "workstation.json"
        self.children = {}

    def load(self):
        self.remote = not self.server_config.is_file()
        self.server = {} if self.remote else read_json(self.server_config)
        self.worker = read_json(self.worker_config)
        url = urllib.parse.urlsplit(self.worker["url"])
        loopback = url.hostname == "localhost"
        try:
            loopback = loopback or ipaddress.ip_address(url.hostname).is_loopback
        except ValueError:
            pass
        if (url.scheme != "https" and not (url.scheme == "http" and loopback)) or url.username is not None or url.password is not None or url.query or url.fragment or url.path not in ("", "/"):
            raise SetupError("Use verified HTTPS for remote clusters, or HTTP on loopback")
        if not self.remote and (not loopback or url.scheme != "http"):
            raise SetupError("Local pilot management requires its HTTP loopback URL; use the managed HTTPS listener for other machines")
        if url.port is None or not 1 <= url.port <= 65535:
            raise SetupError("Configure a fixed local server port")
        if not self.remote:
            host, port = self.server.get("listen", "").rsplit(":", 1)
            if host.strip("[]") not in ("127.0.0.1", "::1", "localhost") or int(port) != url.port:
                raise SetupError("server.json listen and workstation.json URL must match on loopback")
        self.is_workstation = self.worker.get("workstation") is not None
        if self.is_workstation and self.worker.get("roles") != ["ci"]:
            raise SetupError('The workstation config must have roles: ["ci"]')
        if not self.is_workstation and self.worker.get("roles") != ["script", "agent"]:
            raise SetupError('Use the generated script/agent worker configuration')
        self.origin = urllib.parse.urlunsplit((url.scheme, url.netloc, "", "", ""))
        self.api = LocalAPI(self.origin, self.worker.get("caFile") or None)
        self.port = url.port
        self.work = Path(self.worker["workDir"]) / "workstation"
        self.runner = Path(self.worker["workstation"]["runnerDir"]) if self.is_workstation else None
        self.credential = Path(self.worker["credentialFile"])

    def run(self, command, config=None, input_text=None):
        arguments = [str(self.binary), command]
        if config is not None:
            arguments += ["--config", str(config)]
        result = subprocess.run(arguments, input=input_text, text=True,
                                capture_output=True, env=self.env, cwd=self.root)
        if result.returncode:
            raise SetupError(f"{command} failed: {result.stderr.strip()[:800]}")
        return result.stdout.strip()

    def role_config(self, role):
        return self.server_config if role == "server" else self.worker_config

    def role_command(self, role):
        return "server" if role == "server" else ("workstation" if self.is_workstation else "worker")

    def record_path(self, role):
        return self.root / (role + ".process.json")

    def identify(self, role, pid):
        return process_identity(pid, self.binary, self.role_command(role), self.role_config(role))

    def running(self, role):
        if role == "server" and self.remote:
            return None
        record = self.record_path(role)
        if record.is_file():
            saved = read_json(record)
            identity = self.identify(role, saved.get("pid"))
            if identity and identity == saved.get("identity"):
                return saved["pid"]
        # Adopt an existing manual instance only after verifying executable,
        # exact configuration arguments and process start time.
        candidates = []
        legacy = self.root / (role + ".pid")
        if legacy.is_file():
            try:
                candidates.append(int(legacy.read_text().strip()))
            except ValueError:
                pass
        if role == "server":
            args = ["lsof", "-nP", f"-iTCP:{self.port}", "-sTCP:LISTEN", "-t"]
        else:
            args = ["lsof", "-t", str(self.work / "controller.lock")]
        if shutil.which("lsof"):
            result = subprocess.run(args, capture_output=True, text=True)
            candidates += [int(p) for p in result.stdout.split() if p.isdigit()]
        for pid in candidates:
            identity = self.identify(role, pid)
            if identity:
                write_json(record, {"pid": pid, "identity": identity, "adopted": True})
                return pid
        return None

    def start_role(self, role):
        existing = self.running(role)
        if existing:
            print(f"{role}: already running (PID {existing})")
            return
        if role == "server" and self.api.healthy():
            raise SetupError("Another server is using this port; its process identity did not match")
        if role == "controller" and self.is_workstation and not (self.runner / ".sluicework-runner").is_file():
            raise SetupError("Runner is not installed; run manage setup first")
        started = time.time()
        log = self.root / (role + ".log")
        if log.is_symlink():
            raise SetupError("Refusing a symlink for " + str(log))
        with log.open("ab") as out:
            child = subprocess.Popen(
                [str(self.binary), self.role_command(role), "--config", str(self.role_config(role))],
                cwd=self.root, env=self.env, stdin=subprocess.DEVNULL,
                stdout=out, stderr=subprocess.STDOUT, start_new_session=True,
            )
        self.children[role] = child
        identity = self.identify(role, child.pid)
        if not identity:
            raise SetupError(f"{role} did not start; inspect {log}")
        write_json(self.record_path(role), {"pid": child.pid, "identity": identity})
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            if child.poll() is not None:
                raise SetupError(f"{role} exited with code {child.returncode}; inspect {log}")
            if role == "server" and self.api.healthy():
                print(f"server: ready at {self.origin}")
                return
            if role == "controller" and not self.is_workstation:
                time.sleep(0.2)
                if child.poll() is not None:
                    raise SetupError("Worker exited; inspect " + str(log))
                print(f"worker: running (PID {child.pid})")
                return
            if role == "controller":
                report = self.report()
                if report and report["updated_epoch"] >= started:
                    print(f"controller: {display_state(report['state'])} (PID {child.pid})")
                    return
            time.sleep(0.2)
        raise SetupError(f"{role} is still starting; inspect {log} and run manage status")

    def report(self):
        path = self.work / "status.json"
        if not path.is_file():
            return None
        data = read_json(path)
        updated = datetime.datetime.fromisoformat(data["updated"].replace("Z", "+00:00")).timestamp()
        data["updated_epoch"] = updated
        if time.time() - updated > 30:
            data["state"] = "offline"
            data["reason"] = "controller report expired"
        return data

    def start(self):
        if not self.remote:
            self.start_role("server")
        if not self.credential.is_file():
            raise SetupError("Worker is not enrolled; run manage setup first")
        self.start_role("controller")

    def setup(self):
        missing = [tool for tool in ["rustup", "python3.12"] if not shutil.which(tool, path=self.env["PATH"])]
        if missing:
            raise SetupError("Missing build tools: " + ", ".join(missing) + ". Install the missing Homebrew formulas (rustup, python@3.12), then rerun setup.")
        if sys.platform == "darwin":
            result = subprocess.run(["/usr/bin/xcrun", "--find", "clang"], capture_output=True)
            if result.returncode:
                raise SetupError("Install Xcode Command Line Tools, then rerun setup")
        database = Path(self.server["dataDir"]) / "state.db"
        if not database.is_file():
            password = new_password()
            self.run("init", self.server_config, password + "\n")
        self.start_role("server")
        if not self.credential.is_file():
            token = self.run("enroll-token", self.server_config)
            self.run("join", self.worker_config, token + "\n")
            print("CI worker enrolled")
        if not (self.runner / ".sluicework-runner").is_file():
            print("Downloading the checksum-verified official GitHub runner…", flush=True)
            self.run("workstation-install", self.worker_config)
        self.start_role("controller")
        changed = self.approve() if self.server.get("github") else False
        if not self.server.get("github"):
            print("Open the main admin: configure Settings → GitHub App connection, then approve this worker in Machines")
        if changed:
            approved = time.time()
            deadline = time.monotonic() + 8
            while time.monotonic() < deadline:
                report = self.report()
                if report and report["updated_epoch"] >= approved:
                    break
                time.sleep(0.2)
        self.status()
        print("Setup complete. Accept builds explicitly with:", shlex.quote(str(self.root / "manage")), "enable")

    def approve(self):
        saved = read_json(self.credential)
        token = saved["credential"]
        try:
            self.api.request("POST", "/worker/runner", {"operation": "status"}, token)
            print("Repository approval already configured")
            return False
        except SetupError:
            pass
        repository = self.server.get("github", {}).get("repository")
        if not repository:
            raise SetupError("Configure the GitHub App in server.json before approving a runner")
        print(f"Approve this CI worker for {repository}. Password is used locally and not saved.")
        password = getpass.getpass("Sluicework admin password: ")
        logged_in = False
        try:
            user = self.api.request("POST", "/login", {"username": "admin", "password": password})
            password = None
            logged_in = True
            if user.get("role") != "admin":
                raise SetupError("Administrator permission is required")
            machines = self.api.request("GET", "/machines")
            machine = next((m for m in machines if m["id"] == saved["id"]), None)
            if not machine or machine.get("roles") != ["ci"]:
                raise SetupError("Enrolled CI worker was not found in this cluster")
            registration = machine.get("runner")
            if registration and (not registration.get("workstation") or registration.get("scope", "").lower() != repository.lower()):
                raise SetupError("Existing runner registration differs; it was not overwritten")
            if registration:
                print("Repository approval already configured; existing labels preserved")
                return False
            operating_system = {"darwin": "macOS", "linux": "Linux"}.get(machine.get("os"))
            architecture = {"arm64": "ARM64", "amd64": "X64"}.get(machine.get("arch"))
            if not operating_system or not architecture:
                raise SetupError("Upgrade the worker to report its platform before approval")
            self.api.request("PATCH", "/machines/" + urllib.parse.quote(saved["id"], safe=""), {
                "runner": {"workstation": True, "name": machine["name"], "scope": repository,
                           "labels": ["self-hosted", operating_system, architecture, "sluicework-" + saved["id"][:12]], "service": ""}
            })
            print("Repository approval saved")
            return True
        finally:
            password = None
            if logged_in:
                try:
                    self.api.request("POST", "/logout", {})
                except SetupError:
                    pass

    def status(self):
        server = self.running("server")
        controller = self.running("controller")
        print(f"Console: {self.origin}")
        if self.remote:
            print("Cluster:", "reachable" if self.api.healthy() else "not responding")
        else:
            print("Server:", f"running (PID {server})" if server and self.api.healthy() else "not responding")
        report = self.report()
        print("Controller:", ("running" if not self.is_workstation else display_state(report["state"]) if report else "starting") if controller else "stopped")
        if controller and report and report.get("reason"):
            print("Reason:", report["reason"])
        if self.credential.is_file() and self.is_workstation:
            print("Machine label: sluicework-" + read_json(self.credential)["id"][:12])
        print("Logs:", self.root / "server.log", "and", self.root / "controller.log")

    def enable(self):
        if not self.is_workstation:
            raise SetupError("Resume script/AI worker claims from the main admin Machines page")
        self.start()
        saved = read_json(self.credential)
        self.api.request("POST", "/worker/runner", {"operation": "status"}, saved["credential"])
        self.run("workstation-enable", self.worker_config)
        print("Capacity enabled within the configured resource limits and expiry window")
        self.status()

    def pause(self):
        if not self.is_workstation:
            self.api.request("POST", "/worker/drain", {}, read_json(self.credential)["credential"])
            print("Worker drained; assigned work is allowed to finish")
            return
        self.run("workstation-pause", self.worker_config)
        print("Capacity paused; assigned work is allowed to finish")

    def stop(self, timeout=120):
        self.pause()
        if not self.is_workstation:
            deadline = time.monotonic() + timeout
            while self.api.request("GET", "/worker/status", credential=read_json(self.credential)["credential"])["running"]:
                if time.monotonic() >= deadline:
                    raise SetupError("Worker is still draining; it was left running. Retry stop after active work finishes")
                time.sleep(0.2)
        for role in (["controller"] if self.remote else ["controller", "server"]):
            pid = self.running(role)
            if not pid:
                if role == "controller" and (self.work / "controller.lock").exists():
                    with (self.work / "controller.lock").open("r") as lock:
                        try:
                            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                        except BlockingIOError:
                            raise SetupError("An unrecognized controller holds the lock; the server was left running") from None
                continue
            os.kill(pid, signal.SIGTERM)
            deadline = time.monotonic() + timeout
            while self.identify(role, pid) and time.monotonic() < deadline:
                if role in self.children:
                    self.children[role].poll()
                time.sleep(0.2)
            if self.identify(role, pid):
                raise SetupError(f"{role} is still draining. The server was left running; use manage status, then retry manage stop after the build finishes")
            if role in self.children:
                self.children[role].wait(timeout=5)
            print(role + ": stopped")

    def reset_password(self):
        if self.remote:
            raise SetupError("Reset cluster passwords on the server Mac; this worker has no separate admin account")
        password = new_password()
        result = subprocess.run(
            [str(self.binary), "user", "--config", str(self.server_config), "--name", "admin", "--role", "admin"],
            input=password + "\n", text=True, capture_output=True, env=self.env,
        )
        if result.returncode:
            raise SetupError("Password reset failed: " + result.stderr.strip()[:800])
        print("Admin password reset. Sign in again; the server can remain running")


def new_password():
    password = getpass.getpass("Choose an admin password (12–72 bytes): ")
    if not 12 <= len(password.encode()) <= 72:
        raise SetupError("Password must be 12–72 bytes")
    if getpass.getpass("Confirm password: ") != password:
        raise SetupError("Passwords do not match")
    return password


def create_configs(root, args):
    paths = [root / "server.json", root / "workstation.json"]
    if all(p.is_file() for p in paths):
        return
    if any(p.exists() for p in paths):
        raise SetupError("Only one config exists; preserve it and complete the missing config manually")
    github = None
    explicit_github = any(getattr(args, name, None) is not None for name in ("app_id", "installation_id", "repository", "key_file"))
    if explicit_github:
        app_id = args.app_id if args.app_id is not None else int(input("GitHub App ID: "))
        installation = args.installation_id if args.installation_id is not None else int(input("GitHub App installation ID: "))
        repository = args.repository or input("Repository (owner/name): ").strip()
        if app_id <= 0 or installation <= 0 or not 1 <= args.port <= 65535:
            raise SetupError("App IDs must be positive and the port must be between 1 and 65535")
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+", repository):
            raise SetupError("Repository must have the form owner/name")
        source = Path(args.key_file or input("Downloaded App private-key path: ").strip()).expanduser()
        key = root / "github-app.pem"
        if key.is_symlink():
            raise SetupError("Refusing a symlink for the App key")
        if not key.exists():
            descriptor = os.open(source, os.O_RDONLY | os.O_NOFOLLOW)
            with os.fdopen(descriptor, "rb") as src:
                if not stat.S_ISREG(os.fstat(src.fileno()).st_mode):
                    raise SetupError("Expected a regular App key file")
                with key.open("xb") as dst:
                    shutil.copyfileobj(src, dst)
        if stat.S_IMODE(key.stat().st_mode) != 0o600:
            raise SetupError("App key must have mode 600; existing permissions were not changed")
        github = {"appId": app_id, "installationId": installation, "repository": repository, "privateKeyFile": str(key)}
    elif not 1 <= args.port <= 65535:
        raise SetupError("Port must be between 1 and 65535")
    configurations = [
        {"dataDir": str(root / "server-data"), "listen": f"127.0.0.1:{args.port}",
         **({"github": github} if github else {})},
        {"url": f"http://127.0.0.1:{args.port}", "name": "Personal Mac CI", "roles": ["ci"],
         "credentialFile": str(root / "worker-credential.json"), "workDir": str(root / "control"),
         "workstation": {"runnerDir": str(root / "github-runner"), "minIdleSeconds": 0,
                         "maxLoadPerCpu": 0.5, "minAvailableMemoryMb": 4096, "buildThreads": 2, "enableMinutes": 120}},
    ]
    for path, config in zip(paths, configurations):
        with path.open("x") as out:
            json.dump(config, out, indent=2)
            out.write("\n")



def join_existing(root):
    """Join a GUI-generated setup bundle; only the main admin approves runners."""
    if (root / "server.json").exists():
        raise SetupError("This directory hosts a server; use a new directory for a remote worker")
    bootstrap = read_json(root / "bootstrap.json")
    if bootstrap.get("format") != 1:
        raise SetupError("Unsupported setup bundle; download a current bundle from the main admin")
    operating_system = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
    architecture = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine())
    if bootstrap.get("os") != operating_system or bootstrap.get("arch") != architecture:
        raise SetupError("This setup bundle targets another OS/CPU; download the matching bundle")
    url = bootstrap.get("url", "")
    name = bootstrap.get("name", "")
    if not isinstance(name, str) or not name.strip() or len(name) > 128:
        raise SetupError("Invalid worker name in setup bundle")
    worker_config = root / "workstation.json"
    if not worker_config.exists():
        kind = bootstrap.get("kind", "ci")
        if kind not in ("ci", "worker"):
            raise SetupError("Unsupported worker kind")
        config = {"url": url, "name": name, "roles": ["ci"],
                  "credentialFile": str(root / "worker-credential.json"), "workDir": str(root / "control"),
                  "workstation": {"runnerDir": str(root / "github-runner"), "minIdleSeconds": 0,
                                  "maxLoadPerCpu": 0.5, "minAvailableMemoryMb": 4096,
                                  "buildThreads": 2, "enableMinutes": 120}}
        if kind == "worker":
            config.pop("workstation")
            config["roles"] = ["script", "agent"]
            config["credentialFile"] = str(root / "credentials" / "credential.json")
            config["workDir"] = str(root / "jobs")
            config["inputRoot"] = str(root / "inputs")
            (root / "inputs").mkdir(mode=0o700, exist_ok=True)
        ca = root / "cluster-ca.pem"
        if ca.exists():
            result = subprocess.run(["openssl", "x509", "-in", str(ca), "-outform", "DER"], capture_output=True)
            if result.returncode:
                raise SetupError("Invalid cluster CA certificate")
            fingerprint = hashlib.sha256(result.stdout).hexdigest().upper()
            if fingerprint != bootstrap.get("caFingerprint", ""):
                raise SetupError("Setup bundle CA fingerprint does not match")
            print("Cluster CA SHA-256:", fingerprint)
            if input("Compare with the fingerprint in the main admin. Trust this CA? [y/N]: ").strip().lower() != "y":
                raise SetupError("Cluster CA trust was declined")
            config["caFile"] = str(ca)
        # Validate the complete origin and trust configuration before saving it.
        write_json(worker_config, config)
        try:
            validation = Pilot(root); validation.load()
        except Exception:
            worker_config.unlink(missing_ok=True)
            raise
    pilot = Pilot(root); pilot.load()
    if pilot.origin != url.rstrip("/"):
        raise SetupError("Existing worker belongs to a different origin; it was not overwritten")
    if not pilot.credential.exists():
        token = getpass.getpass("One-use enrollment token from the main admin: ")
        pilot.run("join", pilot.worker_config, token + "\n")
        token = None
        print("Worker enrolled; configure its approval and settings in the main admin’s Machines page")
    if pilot.is_workstation and not (pilot.runner / ".sluicework-runner").is_file():
        pilot.run("workstation-install", pilot.worker_config)
    pilot.start_role("controller")
    pilot.status()
    if pilot.is_workstation:
        print("After approval, choose Offer capacity in the main admin, or run:", shlex.quote(str(root / "manage")), "enable")
    else:
        print("Configure labels, slots and approved jobs in the main admin")

def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    here = Path(__file__).resolve().parent
    default = here if (here / "sluicework").is_file() else Path.home() / "Sluicework-pilot"
    parser.add_argument("--dir", type=Path, default=default, help="pilot directory; defaults beside this helper")
    parser.add_argument("command", choices=["setup", "join", "start", "status", "enable", "pause", "stop", "reset-password", "install-cli"])
    parser.add_argument("--app-id", type=int)
    parser.add_argument("--installation-id", type=int)
    parser.add_argument("--repository")
    parser.add_argument("--key-file")
    parser.add_argument("--port", type=int, default=7443)
    args = parser.parse_args(argv)
    os.umask(0o077)
    try:
        if os.geteuid() == 0:
            raise SetupError("Run manage under your normal login, without sudo")
        root = args.dir.expanduser().resolve()
        if not root.is_dir() or root.stat().st_uid != os.geteuid() or root.stat().st_mode & 0o022:
            raise SetupError("Use an existing pilot directory owned by your login and not writable by other users")
        if not (root / "sluicework").is_file():
            raise SetupError("Extract the Sluicework binary into this directory first")
        if args.command == "install-cli":
            return subprocess.run([str(root / "sluicework"), "install-cli"]).returncode
        if args.command == "status":
            pilot = Pilot(root)
            pilot.load()
            pilot.status()
            return 0
        with (root / "manage.lock").open("a") as lock:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            if args.command in ("setup", "join"):
                result = subprocess.run([str(root / "sluicework"), "install-cli"])
                if result.returncode:
                    print("CLI shortcut setup needs attention; the existing command was preserved.", file=sys.stderr)
            if args.command == "join":
                join_existing(root)
                return 0
            if args.command == "setup":
                create_configs(root, args)
            pilot = Pilot(root)
            pilot.load()
            getattr(pilot, args.command.replace("-", "_"))()
        return 0
    except BlockingIOError:
        print("Another management command is running; retry after it finishes", file=sys.stderr)
    except (SetupError, OSError, ValueError, KeyError, EOFError) as error:
        print(str(error), file=sys.stderr)
    except KeyboardInterrupt:
        print("Setup interrupted; existing files and background services were preserved", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
