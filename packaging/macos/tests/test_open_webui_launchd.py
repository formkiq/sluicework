import importlib.util
from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("open_webui_launchd", Path(__file__).parents[1] / "open-webui/launchd.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class OpenWebUILaunchdTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.state = Path(self.directory.name)
        self.agents = self.state / "LaunchAgents"
        self.calls = []

    def plan(self, ollama="http://127.0.0.1:11434"):
        return module.agent_specs(self.state, ollama, "", self.state / "Project with spaces")

    def fake_run(self, argv, **kwargs):
        self.calls.append(argv)
        return subprocess.CompletedProcess(argv, 1 if argv[1] == "print" else 0)

    def test_defaults_privacy_and_secret_persistence(self):
        plan = self.plan()
        agent = plan["com.sluicework.open-webui"]
        self.assertEqual(agent["ProgramArguments"][1:], ["serve", "--host", "127.0.0.1", "--port", "8080"])
        self.assertNotIn("UserName", agent)
        env = agent["EnvironmentVariables"]
        self.assertEqual(env["DATA_DIR"], str(self.state / "data"))
        self.assertEqual(env["OLLAMA_BASE_URL"], "http://127.0.0.1:11434")
        self.assertEqual(env["WEBUI_AUTH"], "true")
        self.assertEqual(env["USE_SLIM_DOCKER"], "true")
        self.assertEqual(env["OFFLINE_MODE"], "true")
        self.assertEqual(env["ENABLE_OLLAMA_API"], "true")
        self.assertEqual(env["ENABLE_OPENAI_API"], "false")
        self.assertEqual(env["ANONYMIZED_TELEMETRY"], "false")
        self.assertEqual(env["DO_NOT_TRACK"], "true")
        self.assertEqual(env["SCARF_NO_ANALYTICS"], "true")
        secret_file = self.state / "secret.key"
        self.assertEqual(secret_file.stat().st_mode & 0o777, 0o600)
        self.assertEqual(env["WEBUI_SECRET_KEY"], secret_file.read_text().strip())
        # Re-running preserves the same generated secret key.
        self.assertEqual(self.plan()["com.sluicework.open-webui"]["EnvironmentVariables"]["WEBUI_SECRET_KEY"], env["WEBUI_SECRET_KEY"])
        for origin in ["http://192.168.1.4:11434", "https://user:password@example.com", "https://example.com/path", "https://example.com:99999"]:
            with self.assertRaises(ValueError):
                module.validate_origin(origin)

    def test_install_and_rerun_preserve_agent(self):
        module.install_agents(self.state, self.plan(), self.agents, 501, self.fake_run)
        target = self.agents / "com.sluicework.open-webui.plist"
        original = target.read_bytes()
        self.assertEqual(plistlib.loads(original)["Label"], "com.sluicework.open-webui")
        self.assertEqual(target.stat().st_mode & 0o777, 0o600)
        self.assertEqual(len([c for c in self.calls if c[1] == "bootstrap"]), 1)
        self.calls.clear()
        module.install_agents(self.state, self.plan("http://127.0.0.1:11435"), self.agents, 501, self.fake_run)
        self.assertEqual(target.read_bytes(), original)
        self.assertEqual(self.calls, [])

    def test_invalid_or_symlinked_secret_is_preserved_and_refused(self):
        secret = self.state / 'secret.key'
        secret.write_text('invalid')
        with self.assertRaises(ValueError):
            self.plan()
        self.assertEqual(secret.read_text(), 'invalid')
        secret.unlink()
        target = self.state / 'other'
        target.write_text('a' * 64)
        secret.symlink_to(target)
        with self.assertRaises(ValueError):
            self.plan()
        self.assertEqual(target.read_text(), 'a' * 64)

    def test_unmanaged_conflict_and_rollback_on_failure(self):
        self.agents.mkdir()
        target = self.agents / "com.sluicework.open-webui.plist"
        target.write_text("existing user configuration")
        with self.assertRaises(RuntimeError):
            module.install_agents(self.state, self.plan(), self.agents, 501, self.fake_run)
        self.assertEqual(target.read_text(), "existing user configuration")
        target.unlink()

        def fail_bootstrap(argv, **kwargs):
            if argv[1] == "bootstrap":
                raise subprocess.CalledProcessError(1, argv)
            return self.fake_run(argv, **kwargs)

        with self.assertRaises(subprocess.CalledProcessError):
            module.install_agents(self.state, self.plan(), self.agents, 501, fail_bootstrap)
        self.assertEqual(list(self.agents.iterdir()), [])
        self.assertFalse((self.state / "launchd-manifest.json").exists())
        self.assertTrue(any(c[1:3] == ["bootout", "gui/501/com.sluicework.open-webui"] for c in self.calls))


if __name__ == "__main__":
    unittest.main()
