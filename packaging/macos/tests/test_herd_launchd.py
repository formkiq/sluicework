import importlib.util
from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("herd_launchd", Path(__file__).parents[1] / "herd/launchd.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class HerdLaunchdTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.state = Path(self.directory.name)
        self.agents = self.state / 'LaunchAgents'
        self.calls = []

    def plan(self, mode="router"):
        return module.agent_specs(self.state, mode, 'http://127.0.0.1:11435', 'http://127.0.0.1:11434', '', self.state / 'Project with spaces')

    def fake_run(self, argv, **kwargs):
        self.calls.append(argv)
        return subprocess.CompletedProcess(argv, 1 if argv[1] == 'print' else 0)

    def test_local_defaults_and_isolation(self):
        plan = self.plan()
        router = plan['com.sluicework.herd']
        self.assertEqual(router['ProgramArguments'][1:], ['--host','127.0.0.1','--port','11435'])
        self.assertNotIn('UserName', router)
        for agent in plan.values():
            env = agent['EnvironmentVariables']
            self.assertEqual(env['FLEET_ENV_FILE'], str(self.state / 'env'))
            for key in ['FLEET_TELEMETRY', 'FLEET_NODE_TELEMETRY', 'FLEET_AUTO_PULL', 'FLEET_VRAM_FALLBACK', 'FLEET_FLEET_INTELLIGENCE']:
                self.assertEqual(env[key], 'false')
            self.assertEqual(env['FLEET_DISABLE_MODEL_PRELOADER'], 'true')
            self.assertEqual(env['FLEET_CONTEXT_PROTECTION'], 'passthrough')
        self.assertEqual(list(self.plan('node')), ['com.sluicework.herd-node'])
        for origin in ['http://192.168.1.4:11435', 'https://user:password@example.com', 'https://example.com/path', 'https://example.com:99999']:
            with self.assertRaises(ValueError):
                module.validate_origin(origin)

    def test_install_and_rerun_preserve_agents(self):
        module.install_agents(self.state, self.plan(), self.agents, 501, self.fake_run)
        target = self.agents / 'com.sluicework.herd.plist'
        original = target.read_bytes()
        self.assertEqual(plistlib.loads(original)['Label'], 'com.sluicework.herd')
        self.assertEqual(target.stat().st_mode & 0o777, 0o600)
        self.assertEqual(len([c for c in self.calls if c[1] == 'bootstrap']), 2)
        self.calls.clear()
        module.install_agents(self.state, self.plan('node'), self.agents, 501, self.fake_run)
        self.assertEqual(target.read_bytes(), original)
        self.assertEqual(self.calls, [])

    def test_unmanaged_conflict_and_partial_failure(self):
        self.agents.mkdir()
        target = self.agents / 'com.sluicework.herd.plist'
        target.write_text('existing user configuration')
        with self.assertRaises(RuntimeError):
            module.install_agents(self.state, self.plan(), self.agents, 501, self.fake_run)
        self.assertEqual(target.read_text(), 'existing user configuration')
        target.unlink()

        def fail_node(argv, **kwargs):
            if argv[1] == 'bootstrap' and 'herd-node' in argv[-1]:
                raise subprocess.CalledProcessError(1, argv)
            return self.fake_run(argv, **kwargs)

        with self.assertRaises(subprocess.CalledProcessError):
            module.install_agents(self.state, self.plan(), self.agents, 501, fail_node)
        self.assertEqual(list(self.agents.iterdir()), [])
        self.assertFalse((self.state / 'launchd-manifest.json').exists())
        self.assertTrue(any(c[1:3] == ['bootout', 'gui/501/com.sluicework.herd'] for c in self.calls))


if __name__ == '__main__':
    unittest.main()
