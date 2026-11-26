import contextlib
import importlib.machinery
import importlib.util
import io
import json
import os
from pathlib import Path
import ssl
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from urllib.request import HTTPSHandler

helper_path = os.environ.get("SLUICEWORK_HELPER_TEST_SCRIPT", Path(__file__).parents[1] / "workstation.py")
spec = importlib.util.spec_from_loader("remote_workstation_helper", importlib.machinery.SourceFileLoader("remote_workstation_helper", str(helper_path)))
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class RemoteWorkerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="sluicework remote ")
        self.root = Path(self.temp.name)
        self.umask = os.umask(0o077)
        (self.root / "sluicework").write_text("fixture")
        self.bootstrap = {"format": 1, "url": "https://cluster.example:8443", "name": "Second Mac", "os": "darwin", "arch": "arm64", "kind": "ci"}

    def tearDown(self):
        os.umask(self.umask)
        self.temp.cleanup()

    def join(self):
        helper.write_json(self.root / "bootstrap.json", self.bootstrap)
        calls = []

        def run(pilot, command, config=None, input_text=None):
            calls.append((command, input_text))
            if command == "join":
                pilot.credential.parent.mkdir(parents=True, exist_ok=True)
                helper.write_json(pilot.credential, {"id": "remote-worker", "credential": "synthetic-worker-credential"})
            elif command == "workstation-install":
                pilot.runner.mkdir()
                (pilot.runner / ".sluicework-runner").touch()
            return ""

        with patch.object(helper.platform, "system", return_value="Darwin"), patch.object(helper.platform, "machine", return_value="arm64"), \
             patch.object(helper.getpass, "getpass", return_value="synthetic-enrollment-token"), \
             patch.object(helper.Pilot, "run", autospec=True, side_effect=run), \
             patch.object(helper.Pilot, "start_role", autospec=True) as start, patch.object(helper.Pilot, "status"), \
             contextlib.redirect_stdout(io.StringIO()):
            helper.join_existing(self.root)
        self.assertEqual([call.args[1] for call in start.call_args_list], ["controller"])
        return calls

    def test_remote_ci_join_has_no_server_app_key_or_worker_admin(self):
        self.assertEqual(self.join(), [("join", "synthetic-enrollment-token\n"), ("workstation-install", None)])
        config = helper.read_json(self.root / "workstation.json")
        self.assertEqual(config["roles"], ["ci"])
        self.assertEqual(config["url"], self.bootstrap["url"])
        self.assertFalse((self.root / "server.json").exists())
        self.assertFalse((self.root / "github-app.pem").exists())
        for path in self.root.rglob("*"):
            if path.is_file():
                self.assertNotIn("synthetic-enrollment-token", path.read_text())
        # Retrying preserves the enrolled identity and installed official runner.
        self.assertEqual(self.join(), [])
        pilot = helper.Pilot(self.root, dict(os.environ)); pilot.load()
        self.assertTrue(pilot.remote)
        context = next(handler._context for handler in pilot.api.opener.handlers if isinstance(handler, HTTPSHandler))
        self.assertTrue(context.check_hostname)
        self.assertEqual(context.verify_mode, ssl.CERT_REQUIRED)
        with patch.object(pilot, "pause"), patch.object(pilot, "running", return_value=None) as running:
            pilot.stop()
        self.assertEqual([call.args[0] for call in running.call_args_list], ["controller"])

    def test_script_ai_worker_skips_github_and_uses_separate_private_directories(self):
        self.bootstrap["kind"] = "worker"
        self.assertEqual(self.join(), [("join", "synthetic-enrollment-token\n")])
        config = helper.read_json(self.root / "workstation.json")
        self.assertEqual(config["roles"], ["script", "agent"])
        self.assertNotIn("workstation", config)
        self.assertEqual(Path(config["credentialFile"]).parent, self.root / "credentials")
        self.assertEqual(Path(config["workDir"]), self.root / "jobs")
        self.assertTrue((self.root / "inputs").is_dir())
        pilot = helper.Pilot(self.root, dict(os.environ)); pilot.load()
        self.assertEqual(pilot.role_command("controller"), "worker")
        with patch.object(pilot.api, "request") as request:
            with contextlib.redirect_stdout(io.StringIO()):
                pilot.pause()
        self.assertEqual(request.call_args.args[:2], ("POST", "/worker/drain"))
        with self.assertRaisesRegex(helper.SetupError, "server Mac"):
            pilot.reset_password()

    def test_wrong_platform_and_unsafe_url_do_not_enroll(self):
        self.bootstrap["arch"] = "amd64"
        with self.assertRaisesRegex(helper.SetupError, "another OS/CPU"):
            self.join()
        self.assertFalse((self.root / "workstation.json").exists())
        self.bootstrap["arch"] = "arm64"
        self.bootstrap["url"] = "http://192.168.1.5:8443"
        with self.assertRaisesRegex(helper.SetupError, "HTTPS"):
            self.join()
        self.assertFalse((self.root / "workstation.json").exists())

    def test_fresh_server_setup_defers_github_configuration_to_main_admin(self):
        args = SimpleNamespace(app_id=None, installation_id=None, repository=None, key_file=None, port=7443)
        with patch.object(helper, "input", side_effect=AssertionError("GitHub setup should be in admin"), create=True):
            helper.create_configs(self.root, args)
        self.assertNotIn("github", helper.read_json(self.root / "server.json"))
        self.assertFalse((self.root / "github-app.pem").exists())
