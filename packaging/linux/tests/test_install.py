import importlib.util
import os
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[1]


def load(name):
    spec = importlib.util.spec_from_file_location('test_' + name, SOURCE / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


install = load('install')
runtime = load('runtime')


class InstallTests(unittest.TestCase):
    def test_network_origins_require_tls_and_safe_unit_characters(self):
        for value in ('http://192.168.1.5:7443', 'https://user:pass@host', 'https://host/path', 'https://host\nExecStart=/bin/sh', 'https://ho%st', 'https://ho$st', 'https://host:0'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                install.origin(value)
        self.assertEqual(install.origin('https://build.example:7443/'), 'https://build.example:7443')
        self.assertEqual(install.origin('http://127.0.0.1:11434'), 'http://127.0.0.1:11434')
        with self.assertRaises(ValueError):
            install.origin('http://127.0.0.1:7443', remote=True)

    def test_atomic_install_preserves_existing_files_and_symlinks(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, target = root / 'new', root / 'target'
            source.write_text('new')
            target.write_text('existing')
            with self.assertRaises(RuntimeError):
                install.atomic_copy(source, target)
            self.assertEqual(target.read_text(), 'existing')
            link = root / 'link'
            link.symlink_to(target)
            with self.assertRaises(RuntimeError):
                install.atomic_copy(source, link, replace=True)
            self.assertEqual(target.read_text(), 'existing')
            install.atomic_copy(source, target, mode=0o600, replace=True)
            self.assertEqual(target.read_text(), 'new')
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_binary_install_exposes_cli_without_replacing_a_conflicting_command(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / 'sluicework').write_bytes(b'fixture executable')
            target = root / 'installed' / 'sluicework'
            with patch.object(install, 'SOURCE', root), patch.object(install, 'BIN', target), patch.object(install, 'run') as run:
                install.install_binary()
                run.assert_called_once_with([target, 'install-cli', '--system'])
                run.side_effect = install.subprocess.CalledProcessError(1, 'install-cli')
                install.install_binary()
            self.assertEqual(target.read_bytes(), b'fixture executable')

    def test_existing_unmanaged_service_account_is_refused(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(install, 'ETC', Path(temp)), patch.object(install.pwd, 'getpwnam', return_value=SimpleNamespace(pw_uid=500)), patch.object(install, 'run') as run:
            with self.assertRaises(RuntimeError):
                install.account('sluiceworker', Path(temp) / 'worker')
            run.assert_not_called()

    def test_worker_config_keeps_credentials_out_of_inputs(self):
        with patch.object(install, 'STATE', Path('/var/lib/sluicework')):
            config = install.worker_config('https://cluster.example:7443', 'build-linux', '')
        self.assertEqual(config['credentialFile'], '/var/lib/sluicework/worker/credential.json')
        self.assertEqual(config['inputRoot'], '/var/lib/sluicework/inputs')
        self.assertEqual(config['roles'], ['script', 'agent'])

    def test_unmanaged_private_sandbox_helper_is_preserved(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / 'sluicework-bwrap').write_text('existing')
            with patch.object(install, 'BIN', root / 'sluicework'), patch.object(install, 'ETC', root), self.assertRaises(RuntimeError):
                install.install_isolation()
            self.assertEqual((root / 'sluicework-bwrap').read_text(), 'existing')

    def test_apparmor_exception_attaches_only_to_private_helper(self):
        policy = (SOURCE / 'sluicework-bwrap.apparmor').read_text()
        self.assertIn('profile sluicework-bwrap /opt/sluicework/bin/sluicework-bwrap', policy)
        self.assertIn('userns,', policy)
        self.assertNotIn('profile bwrap /usr/bin/bwrap', policy)
        self.assertNotIn('sysctl', policy.split('abi <abi/4.0>,')[1])

    def test_inference_unit_is_unprivileged_and_preserves_conflicts(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(runtime.installer, 'UNITS', Path(temp)):
            runtime.write_unit('sluicework-herd', '/opt/runtime/herd --host 127.0.0.1', {'FLEET_TELEMETRY': 'false'})
            content = (Path(temp) / 'sluicework-herd.service').read_text()
            self.assertIn('User=sluiceinference', content)
            self.assertIn('NoNewPrivileges=yes', content)
            self.assertIn('ProtectSystem=strict', content)
            self.assertIn('FLEET_TELEMETRY=false', content)
            with self.assertRaises(RuntimeError):
                runtime.write_unit('sluicework-herd', '/other', {})
            with self.assertRaises(ValueError):
                runtime.write_unit('other', '/safe', {'BAD': 'x\nExecStart=bad'})

    def test_herd_decline_has_no_install_side_effects(self):
        with patch.object(runtime, 'run') as run:
            with self.assertRaises(ValueError):
                runtime.install_herd('amd64', None, False)
            run.assert_not_called()

    def test_bad_download_is_rejected_before_extraction(self):
        with tempfile.TemporaryDirectory() as temp:
            dest = Path(temp) / 'archive'
            dest.write_bytes(b'invalid')
            with patch.object(runtime, 'run'), self.assertRaises(RuntimeError):
                runtime.download('https://example.test/archive', dest, '0' * 64)

    def test_open_webui_existing_unit_is_preserved(self):
        with tempfile.TemporaryDirectory() as temp:
            units = Path(temp)
            (units / 'sluicework-open-webui.service').write_text('existing')
            with patch.object(runtime.installer, 'UNITS', units), patch.object(runtime.shutil, 'which', return_value=None):
                with self.assertRaises(RuntimeError):
                    runtime.install_open_webui('amd64', 'http://127.0.0.1:11434')
            self.assertEqual((units / 'sluicework-open-webui.service').read_text(), 'existing')

    def test_open_webui_install_keeps_private_data_and_uses_locked_slim_profile(self):
        with tempfile.TemporaryDirectory() as temp:
            home = Path(temp)
            units = home / 'units'
            units.mkdir()
            data = home / 'open-webui-data'
            data.mkdir()
            (data / 'existing-chat').write_text('preserved')
            secret = home / 'open-webui-secret.key'
            secret.write_text('a' * 64)
            identity = SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid())
            source = SOURCE.parent / 'macos'
            with patch.object(runtime, 'HOME', home), patch.object(runtime, 'SOURCE', source), \
                    patch.object(runtime.installer, 'UNITS', units), patch.object(runtime.installer, 'ETC', home / 'etc'), \
                    patch.object(runtime.shutil, 'which', return_value=None), \
                    patch.object(runtime.socket, 'socket'), patch.object(runtime, 'probe'), patch.object(runtime, 'install_uv', return_value=Path('/verified/uv')), \
                    patch.object(runtime, 'as_user') as prepare, patch.object(runtime, 'start') as start, \
                    patch.object(runtime.pwd, 'getpwnam', return_value=identity), patch.object(runtime.os, 'chown'):
                runtime.install_open_webui('arm64', 'http://127.0.0.1:11434')
            unit = (units / 'sluicework-open-webui.service').read_text()
            self.assertIn('User=sluiceinference', unit)
            self.assertIn('serve --host 127.0.0.1 --port 8080', unit)
            for flag in ('WEBUI_AUTH=true', 'USE_SLIM_DOCKER=true', 'OFFLINE_MODE=true', 'ENABLE_OPENAI_API=false'):
                self.assertIn(flag, unit)
            self.assertEqual((data / 'existing-chat').read_text(), 'preserved')
            self.assertEqual(secret.read_text(), 'a' * 64)
            self.assertEqual(secret.stat().st_mode & 0o777, 0o600)
            self.assertNotIn('WEBUI_SECRET_KEY', unit)
            self.assertNotIn('a' * 64, unit)
            private = home / 'etc/open-webui.env'
            self.assertIn('WEBUI_SECRET_KEY="' + 'a' * 64 + '"', private.read_text())
            self.assertEqual(private.stat().st_mode & 0o777, 0o600)
            self.assertIn(f'EnvironmentFile="{private}"', unit)
            argv = prepare.call_args.args[1]
            self.assertIn('--locked', argv)
            project = next(home.glob('open-webui-0.13.4-*'))
            self.assertEqual(argv[argv.index('--config-file') + 1], project / 'uv.toml')
            self.assertEqual((project / 'uv.toml').read_bytes(), (source / 'open-webui/uv.toml').read_bytes())
            start.assert_called_once_with(['sluicework-open-webui'])


if __name__ == '__main__':
    unittest.main()
