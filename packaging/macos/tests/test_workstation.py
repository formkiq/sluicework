import contextlib
import datetime
import importlib.machinery
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace
from unittest.mock import patch

helper_path = os.environ.get("SLUICEWORK_HELPER_TEST_SCRIPT", Path(__file__).parents[1] / "workstation.py")
spec = importlib.util.spec_from_loader("workstation_helper", importlib.machinery.SourceFileLoader("workstation_helper", str(helper_path)))
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class HelperTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="sluicework helper ")
        self.root = Path(self.temp.name)
        self.umask = os.umask(0o077)
        (self.root / "sluicework").write_text("fixture")
        self.args = SimpleNamespace(app_id=123, installation_id=456, repository="example/pilot", key_file=str(self.root / "download.pem"), port=61239)
        (self.root / "download.pem").write_text("synthetic key")
        helper.create_configs(self.root, self.args)
        self.pilot = helper.Pilot(self.root, dict(os.environ))
        self.pilot.load()

    def tearDown(self):
        os.umask(self.umask)
        self.temp.cleanup()

    def test_fresh_configuration_allows_active_use_and_remains_private(self):
        worker = helper.read_json(self.root / "workstation.json")
        self.assertEqual(worker["roles"], ["ci"])
        self.assertEqual(worker["workstation"]["minIdleSeconds"], 0)
        self.assertEqual(worker["workstation"]["buildThreads"], 2)
        for name in ["server.json", "workstation.json", "github-app.pem"]:
            self.assertEqual((self.root / name).stat().st_mode & 0o777, 0o600)

    def test_existing_configuration_is_preserved(self):
        path = self.root / "workstation.json"
        worker = helper.read_json(path)
        worker["workstation"]["minAvailableMemoryMb"] = 8192
        path.write_text(json.dumps(worker))
        before = path.read_bytes()
        helper.create_configs(self.root, self.args)
        self.assertEqual(path.read_bytes(), before)

    def test_partial_configuration_is_not_overwritten(self):
        (self.root / "workstation.json").unlink()
        with self.assertRaisesRegex(helper.SetupError, "Only one config"):
            helper.create_configs(self.root, self.args)

    def test_invalid_setup_values_do_not_create_files(self):
        for name in ["server.json", "workstation.json", "github-app.pem"]:
            (self.root / name).unlink()
        for changes in [{"app_id": 0}, {"installation_id": -1}, {"port": 65536}, {"repository": "missing-owner"}]:
            args = SimpleNamespace(**(vars(self.args) | changes))
            with self.assertRaises(helper.SetupError):
                helper.create_configs(self.root, args)
            self.assertFalse((self.root / "github-app.pem").exists())
            self.assertFalse((self.root / "server.json").exists())

    def test_remote_or_userinfo_urls_are_rejected(self):
        for url in ["http://example.com:7443", "http://:password@localhost:7443", "http://127.0.0.1:7443/path"]:
            worker = helper.read_json(self.root / "workstation.json")
            worker["url"] = url
            (self.root / "workstation.json").write_text(json.dumps(worker))
            with self.assertRaises(helper.SetupError):
                self.pilot.load()

    def test_stale_pid_never_signals_another_process(self):
        helper.write_json(self.pilot.record_path("server"), {"pid": os.getpid(), "identity": "stale"})
        with patch.object(self.pilot, "pause"), patch.object(helper.os, "kill") as kill:
            self.pilot.stop(timeout=0)
        kill.assert_not_called()

    def test_status_expires_old_reports(self):
        self.pilot.work.mkdir(parents=True)
        helper.write_json(self.pilot.work / "status.json", {
            "state": "accepting", "updated": (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(minutes=1)).isoformat(),
        })
        self.assertEqual(self.pilot.report()["state"], "offline")

    def test_repeated_setup_skips_existing_enrollment_and_download(self):
        tools = self.root / "tools"
        tools.mkdir()
        for name in ["rustup", "python3.12"]:
            path = tools / name
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o700)
        self.pilot.env["PATH"] = str(tools) + os.pathsep + os.defpath
        database = Path(self.pilot.server["dataDir"]) / "state.db"
        database.parent.mkdir()
        database.touch()
        self.pilot.credential.write_text("existing credential")
        self.pilot.runner.mkdir()
        (self.pilot.runner / ".sluicework-runner").touch()
        with patch.object(self.pilot, "start_role"), patch.object(self.pilot, "approve", return_value=False), patch.object(self.pilot, "status"), patch.object(self.pilot, "run") as run:
            with contextlib.redirect_stdout(io.StringIO()):
                self.pilot.setup()
        run.assert_not_called()
        self.assertEqual(self.pilot.credential.read_text(), "existing credential")

    def test_operator_cannot_approve_and_password_is_not_stored(self):
        self.pilot.credential.write_text(json.dumps({"id": "worker", "credential": "synthetic-worker-token"}))
        requests = []

        def api(method, path, body=None, credential=None):
            requests.append((method, path))
            if path == "/worker/runner":
                raise helper.SetupError("approval needed")
            if path == "/login":
                return {"role": "operator"}
            if path == "/logout":
                return None
            self.fail("Unexpected privileged request")

        with patch.object(self.pilot.api, "request", side_effect=api), patch.object(helper.getpass, "getpass", return_value="synthetic-password"):
            with self.assertRaisesRegex(helper.SetupError, "Administrator"):
                self.pilot.approve()
        self.assertNotIn(("PATCH", "/machines/worker"), requests)
        self.assertIn(("POST", "/logout"), requests)
        self.assertFalse(any("synthetic-password" in p.read_text() for p in self.root.iterdir() if p.is_file()))

    def test_matching_approval_keeps_custom_labels_after_status_failure(self):
        self.pilot.credential.write_text(json.dumps({"id": "worker", "credential": "synthetic-worker-token"}))

        def api(method, path, body=None, credential=None):
            if path == "/worker/runner":
                raise helper.SetupError("temporary status failure")
            if path == "/login":
                return {"role": "admin"}
            if path == "/machines":
                return [{"id": "worker", "roles": ["ci"], "runner": {
                    "workstation": True, "scope": "Example/Pilot", "labels": ["custom-label"]}}]
            if path == "/logout":
                return None
            self.fail("Existing registration must not be patched")

        with patch.object(self.pilot.api, "request", side_effect=api), patch.object(helper.getpass, "getpass", return_value="synthetic-password"):
            self.assertFalse(self.pilot.approve())

    def test_redirects_do_not_forward_credentials_and_errors_are_redacted(self):
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                if self.path.endswith("/login"):
                    self.send_response(302)
                    self.send_header("Location", "https://example.invalid/secret")
                    self.end_headers()
                else:
                    self.send_response(400)
                    self.end_headers()
                    self.wfile.write(b'{"error":"sensitive-response-body"}')

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            api = helper.LocalAPI(f"http://127.0.0.1:{server.server_port}")
            with self.assertRaisesRegex(helper.SetupError, "redirected"):
                api.request("POST", "/login", {"password": "synthetic-password"})
            with self.assertRaises(helper.SetupError) as error:
                api.request("POST", "/failure", {})
            self.assertNotIn("sensitive-response-body", str(error.exception))
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_unknown_locked_controller_prevents_stopping_server(self):
        self.pilot.work.mkdir(parents=True)
        lock = self.pilot.work / "controller.lock"
        child = subprocess.Popen([sys.executable, "-c", "import fcntl,sys,time;f=open(sys.argv[1],'w');fcntl.flock(f,fcntl.LOCK_EX);print('ready',flush=True);time.sleep(10)", str(lock)], stdout=subprocess.PIPE, text=True)
        try:
            self.assertEqual(child.stdout.readline().strip(), "ready")
            with patch.object(self.pilot, "pause"), patch.object(self.pilot, "running", return_value=None), patch.object(helper.os, "kill") as kill:
                with self.assertRaisesRegex(helper.SetupError, "server was left running"):
                    self.pilot.stop(timeout=0)
                kill.assert_not_called()
        finally:
            child.terminate()
            child.wait()
            child.stdout.close()


@unittest.skipUnless(os.environ.get("SLUICEWORK_HELPER_TEST_BINARY") and os.geteuid() != 0, "provide the native test binary as an unprivileged user")
class LiveHelperTests(unittest.TestCase):
    def test_setup_adopts_server_is_idempotent_and_stops_gracefully(self):
        with tempfile.TemporaryDirectory(prefix="sluicework live helper ") as directory:
            root = Path(directory)
            previous_mask = os.umask(0o077)
            self.addCleanup(os.umask, previous_mask)
            binary = root / "sluicework"
            shutil.copyfile(os.environ["SLUICEWORK_HELPER_TEST_BINARY"], binary)
            binary.chmod(0o700)
            key = root / "download.pem"
            subprocess.run(["/usr/bin/openssl", "genrsa", "-out", str(key), "2048"], check=True, capture_output=True)
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            args = SimpleNamespace(app_id=123, installation_id=456, repository="example/pilot", key_file=str(key), port=port)
            helper.create_configs(root, args)
            tools = root / "tools"
            tools.mkdir()
            for name in ["rustup", "python3.12"]:
                path = tools / name
                path.write_text("#!/bin/sh\nexit 0\n")
                path.chmod(0o700)
            env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ["PATH"])
            pilot = helper.Pilot(root, env)
            pilot.load()
            pilot.runner.mkdir()
            (pilot.runner / ".sluicework-runner").touch()
            password = "disposable-helper-password"
            pilot.run("init", pilot.server_config, password + "\n")
            with (root / "server.log").open("ab") as out:
                server = subprocess.Popen([str(binary), "server", "--config", str(pilot.server_config)], stdout=out, stderr=subprocess.STDOUT)
            try:
                deadline = time.monotonic() + 10
                while not pilot.api.healthy() and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertTrue(pilot.api.healthy())
                (root / "server.pid").write_text(str(server.pid))
                with patch.object(helper.getpass, "getpass", return_value=password):
                    pilot.setup()
                self.assertEqual(pilot.running("server"), server.pid)
                controller = pilot.running("controller")
                self.assertIsNotNone(controller)
                credential = pilot.credential.read_bytes()
                control = helper.read_json(pilot.work / "control.json")
                self.assertTrue(control["enabledUntil"].startswith("0001-"))
                with patch.object(helper.getpass, "getpass") as prompt:
                    pilot.setup()
                    prompt.assert_not_called()
                self.assertEqual(pilot.running("controller"), controller)
                self.assertEqual(pilot.credential.read_bytes(), credential)
                replacement_password = "replacement-helper-password"
                with patch.object(helper.getpass, "getpass", return_value=replacement_password):
                    pilot.reset_password()
                self.assertEqual(pilot.api.request("POST", "/login", {"username": "admin", "password": replacement_password})["role"], "admin")
                pilot.api.request("POST", "/logout", {})
                self.assertEqual(pilot.running("server"), server.pid)
                pilot.stop(timeout=10)
                self.assertIsNone(pilot.running("controller"))
                self.assertFalse(pilot.api.healthy())
                server.wait(timeout=10)
                pilot.start()
                self.assertTrue(pilot.api.healthy())
                self.assertEqual(pilot.report()["state"], "paused")
                self.assertEqual(pilot.credential.read_bytes(), credential)
                pilot.stop(timeout=10)
            finally:
                for role in ["controller", "server"]:
                    pid = pilot.running(role)
                    if pid:
                        os.kill(pid, signal.SIGTERM)
                if server.poll() is None:
                    server.terminate()
                try:
                    server.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait()


if __name__ == "__main__":
    unittest.main()
