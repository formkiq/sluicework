"""Explicit, optional Linux inference installation. Existing runtimes are preserved."""

import argparse
import hashlib
import importlib.util
import os
from pathlib import Path
import platform
import pwd
import secrets
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time

SOURCE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('sluicework_install', SOURCE / 'install.py')
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)
run, as_user = installer.run, installer.as_user
ROOT = Path('/opt/sluicework')
HOME = Path('/var/lib/sluicework/inference')
OLLAMA_VERSION = '0.40.16'
UV_VERSION = '0.13.9'
DOWNLOADS = {
    'amd64': ('x86_64', '87bfafb2acbc89b2e6a1be0a9043de6aa75fb450696f86bc426e65fc448f788d', 'a7fbaff2b9a271d85c1bdb540941090fd1ce011797b4abeb48e4dcd39c2471d5'),
    'arm64': ('aarch64', 'e45b8e6dcb86cb9f3057396351655079c9a4879b4ced2678c9350d375dcbc224', '0797542018e541a9288855df555c8077eed06f7a47d206362030480e8ecc9f3d'),
}


def download(url, dest, expected):
    run(['curl', '--disable', '--fail', '--show-error', '--location', '--proto', '=https', '--proto-redir', '=https', '--output', dest, url])
    digest = hashlib.sha256()
    with dest.open('rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            digest.update(chunk)
    if digest.hexdigest() != expected:
        raise RuntimeError('Download checksum mismatch; nothing from this archive was installed')


def write_unit(name, executable, environment, environment_file=None):
    target = installer.UNITS / (name + '.service')
    if target.exists() or target.is_symlink():
        raise RuntimeError(f'Existing service preserved: {target}')
    if any('\n' in value or '\r' in value or '"' in value or '%' in value or '\\' in value for value in environment.values()):
        raise ValueError('Unsupported character in service environment')
    env = '\n'.join(f'Environment="{key}={value}"' for key, value in environment.items())
    if environment_file is not None:
        path = str(environment_file)
        if not path.startswith('/') or any(c in path for c in ('\n', '\r', '"', '%', '\\')):
            raise ValueError('Unsupported environment file path')
        env += f'\nEnvironmentFile="{path}"'
    text = f'''[Unit]
Description=Sluicework {name}
Wants=network-online.target
After=network-online.target

[Service]
User=sluiceinference
Group=sluiceinference
WorkingDirectory={HOME}
ExecStart={executable}
{env}
Restart=on-failure
RestartSec=10
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths={HOME}

[Install]
WantedBy=multi-user.target
'''
    with tempfile.TemporaryDirectory() as temp:
        source = Path(temp) / 'service'
        source.write_text(text)
        installer.atomic_copy(source, target)


def start(names):
    run(['systemctl', 'daemon-reload'])
    started = []
    try:
        for name in names:
            started.append(name)
            run(['systemctl', 'enable', '--now', name])
    except subprocess.CalledProcessError:
        for name in reversed(started):
            subprocess.run(['systemctl', 'disable', '--now', name], check=False)
        raise


def probe(endpoint):
    run([installer.BIN, 'runtime-check', '--endpoint', endpoint], stdout=subprocess.DEVNULL)


def probe_open_webui(endpoint):
    run([installer.BIN, 'open-webui-check', '--endpoint', endpoint], stdout=subprocess.DEVNULL)


def install_uv(arch):
    uv = ROOT / 'bin/inference-uv'
    if uv.is_file() and not uv.is_symlink():
        return uv
    target_name = f'uv-{DOWNLOADS[arch][0]}-unknown-linux-gnu'
    with tempfile.TemporaryDirectory(dir=ROOT) as temp:
        temp = Path(temp)
        archive = temp / 'uv.tar.gz'
        download(f'https://github.com/astral-sh/uv/releases/download/{UV_VERSION}/{target_name}.tar.gz', archive, DOWNLOADS[arch][2])
        with tarfile.open(archive) as tar:
            tar.extractall(temp, filter='data')
        installer.atomic_copy(temp / target_name / 'uv', uv, 0o755, replace=True)
    return uv


def install_ollama(arch):
    if shutil.which('ollama') or Path('/usr/local/bin/ollama').exists() or Path('/usr/bin/ollama').exists():
        raise RuntimeError('Existing Ollama installation preserved. Start it with its original service and use --endpoint.')
    unit = installer.UNITS / 'sluicework-ollama.service'
    if unit.exists():
        raise RuntimeError('Existing managed Ollama service preserved. Use systemctl to inspect/start it, then --endpoint.')
    try:
        probe('http://127.0.0.1:11434')
    except subprocess.CalledProcessError:
        pass
    else:
        raise RuntimeError('An existing runtime owns port 11434. Reuse it with --endpoint.')
    target = ROOT / ('ollama-' + OLLAMA_VERSION)
    if target.exists():
        raise RuntimeError(f'Existing files preserved: {target}. Inspect a previous interrupted setup before retrying.')
    with tempfile.TemporaryDirectory(dir=ROOT) as temp:
        temp = Path(temp)
        archive, stage = temp / 'ollama.tar.zst', temp / 'runtime'
        download(f'https://github.com/ollama/ollama/releases/download/v{OLLAMA_VERSION}/ollama-linux-{arch}.tar.zst', archive, DOWNLOADS[arch][1])
        stage.mkdir()
        run(['tar', '--no-same-owner', '--no-same-permissions', '--zstd', '-xf', archive, '-C', stage])
        if not (stage / 'bin/ollama').is_file():
            raise RuntimeError('Official archive does not contain bin/ollama')
        stage.rename(target)
    for group in ('render', 'video'):
        import grp
        try:
            grp.getgrnam(group)
        except KeyError:
            continue
        run(['usermod', '-a', '-G', group, 'sluiceinference'])
    write_unit('sluicework-ollama', f'{target}/bin/ollama serve', {
        'OLLAMA_HOST': '127.0.0.1:11434', 'OLLAMA_MODELS': str(HOME / 'models'),
    })
    start(['sluicework-ollama'])


def install_herd(arch, router, accept_lan):
    if not accept_lan:
        raise ValueError('Herd opens an unauthenticated Ollama LAN proxy on TCP 11434. Review the trusted-network requirement and explicitly pass --allow-herd-lan-proxy to install/start it.')
    remote = bool(router)
    router = installer.origin(router, remote=True) if remote else 'http://127.0.0.1:11435'
    names = (['sluicework-herd'] if not remote else []) + ['sluicework-herd-node']
    for name in ('sluicework-herd', 'sluicework-herd-node'):
        if (installer.UNITS / (name + '.service')).exists():
            raise RuntimeError('Existing Herd service preserved. Inspect/start it with systemctl.')
    if shutil.which('herd') or Path('/usr/local/bin/herd').exists():
        raise RuntimeError('Existing Herd installation preserved. Use its original setup and --endpoint.')
    probe('http://127.0.0.1:11434')
    if remote:
        probe(router)
    else:
        # Do not replace a service already listening on the local router port.
        import socket
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 11435))
    project = HOME / 'herd'
    if project.exists():
        raise RuntimeError(f'Existing environment preserved: {project}')
    uv = install_uv(arch)
    project.mkdir(mode=0o700)
    identity = pwd.getpwnam('sluiceinference')
    os.chown(project, identity.pw_uid, identity.pw_gid)
    for name in ('pyproject.toml', 'uv.lock'):
        installer.atomic_copy(SOURCE / 'herd' / name, project / name)
    environment = {**os.environ, 'HOME': str(HOME), 'UV_PYTHON_INSTALL_DIR': str(HOME / 'python'), 'UV_CACHE_DIR': str(HOME / 'uv-cache')}
    as_user('sluiceinference', [uv, 'sync', '--locked', '--no-dev', '--managed-python', '--python', '3.12', '--project', project], env=environment)
    env = {
        'FLEET_ENV_FILE': str(HOME / 'herd.env'), 'FLEET_DATA_DIR': str(HOME / 'router-data'),
        'FLEET_NODE_DATA_DIR': str(HOME / 'node-data'), 'FLEET_NODE_TELEMETRY': 'false',
        'FLEET_TELEMETRY': 'false', 'FLEET_AUTO_PULL': 'false', 'FLEET_VRAM_FALLBACK': 'false',
        'FLEET_CONTEXT_PROTECTION': 'passthrough', 'FLEET_DISABLE_MODEL_PRELOADER': 'true',
        'FLEET_FLEET_INTELLIGENCE': 'false', 'FLEET_THINKING_OVERHEAD': '1.0',
        'FLEET_THINKING_MIN_PREDICT': '0', 'FLEET_DEBUG_REQUEST_BODIES': 'false',
    }
    # An explicit empty file prevents importing an unrelated existing Herd config.
    (HOME / 'herd.env').touch(mode=0o644)
    if not remote:
        write_unit('sluicework-herd', f'{project}/.venv/bin/herd --host 127.0.0.1 --port 11435', env)
    write_unit('sluicework-herd-node', f'{project}/.venv/bin/herd-node --router-url {router} --ollama-host http://127.0.0.1:11434', env)
    start(names)
    return router


def install_open_webui(arch, ollama_url):
    if shutil.which('open-webui') or Path('/usr/local/bin/open-webui').exists() or Path('/usr/bin/open-webui').exists():
        raise RuntimeError('Existing Open WebUI installation preserved. Start it with its original service and use --open-webui-endpoint.')
    unit = installer.UNITS / 'sluicework-open-webui.service'
    if unit.exists() or unit.is_symlink():
        raise RuntimeError('Existing managed Open WebUI service preserved. Use systemctl to inspect/start it, then --open-webui-endpoint.')
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 8080))
    probe(ollama_url)
    digest = hashlib.sha256()
    for name in ('pyproject.toml', 'uv.toml', 'uv.lock'):
        digest.update((SOURCE / 'open-webui' / name).read_bytes())
    project = HOME / ('open-webui-0.13.4-' + digest.hexdigest()[:12])
    if os.path.lexists(project):
        raise RuntimeError(f'Existing environment preserved: {project}')
    uv = install_uv(arch)
    project.mkdir(mode=0o700)
    data_dir = HOME / 'open-webui-data'
    if data_dir.is_symlink():
        raise RuntimeError('Refusing symlinked Open WebUI data directory')
    data_dir.mkdir(mode=0o700, exist_ok=True)
    identity = pwd.getpwnam('sluiceinference')
    os.chown(project, identity.pw_uid, identity.pw_gid)
    os.chown(data_dir, identity.pw_uid, identity.pw_gid)
    for name in ('pyproject.toml', 'uv.lock', 'uv.toml'):
        installer.atomic_copy(SOURCE / 'open-webui' / name, project / name)
    environment = {**os.environ, 'HOME': str(HOME), 'UV_PYTHON_INSTALL_DIR': str(HOME / 'python'), 'UV_CACHE_DIR': str(HOME / 'uv-cache')}
    as_user('sluiceinference', [uv, 'sync', '--config-file', project / 'uv.toml', '--locked', '--no-dev', '--no-install-project', '--managed-python', '--python', '3.12', '--project', project], env=environment)
    secret_path = HOME / 'open-webui-secret.key'
    if secret_path.is_symlink():
        raise RuntimeError(f'Refusing to read symlinked secret key: {secret_path}')
    if secret_path.exists():
        secret_key = secret_path.read_text().strip()
        if len(secret_key) < 32:
            raise RuntimeError('Existing Open WebUI secret is invalid; preserve it and restore a valid backup')
        secret_path.chmod(0o600)
    else:
        secret_key = secrets.token_hex(32)
        fd = os.open(secret_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        try:
            os.write(fd, (secret_key + '\n').encode('ascii'))
            os.fsync(fd)
        finally:
            os.close(fd)
        os.chown(secret_path, identity.pw_uid, identity.pw_gid)
    # Slim uses the upstream external-inference profile; no embedded ML/vector stack.
    env = {
        'DATA_DIR': str(data_dir),
        'OLLAMA_BASE_URL': ollama_url,
        'WEBUI_AUTH': 'true',
        'USE_SLIM_DOCKER': 'true',
        'OFFLINE_MODE': 'true',
        'ENABLE_OLLAMA_API': 'true',
        'ENABLE_OPENAI_API': 'false',
        'ANONYMIZED_TELEMETRY': 'false',
        'DO_NOT_TRACK': 'true',
        'SCARF_NO_ANALYTICS': 'true',
    }
    # Unit definitions are public. systemd reads this separate root-only file;
    # never put the signing key in an Environment= line or command argument.
    if any(c in secret_key for c in ('\x00', '\n', '\r', '"', '\\')):
        raise RuntimeError('Existing secret cannot be represented safely in the service environment')
    environment_file = installer.ETC / 'open-webui.env'
    with tempfile.TemporaryDirectory() as temp:
        private = Path(temp) / 'environment'
        private.write_text(f'WEBUI_SECRET_KEY="{secret_key}"\n')
        installer.atomic_copy(private, environment_file, mode=0o600)
    write_unit('sluicework-open-webui', f'{project}/.venv/bin/open-webui serve --host 127.0.0.1 --port 8080', env, environment_file)
    start(['sluicework-open-webui'])
    return 'http://127.0.0.1:8080'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--endpoint', help='Reuse an existing Ollama/Herd origin')
    parser.add_argument('--install-ollama', action='store_true')
    parser.add_argument('--install-herd', action='store_true')
    parser.add_argument('--router-url', help='Existing verified HTTPS router for a node-only Herd install')
    parser.add_argument('--allow-herd-lan-proxy', action='store_true')
    parser.add_argument('--install-open-webui', action='store_true')
    parser.add_argument('--open-webui-endpoint', help='Reuse an existing Open WebUI origin')
    parser.add_argument('--ollama-url', help='Ollama or Herd origin used by Open WebUI')
    args = parser.parse_args()
    if sys.platform != 'linux' or os.geteuid() != 0 or not Path('/run/systemd/system').is_dir():
        raise RuntimeError('Run with sudo on a systemd Linux host')
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
    if arch not in DOWNLOADS or arch != (SOURCE / 'target-arch').read_text().strip():
        raise RuntimeError('Unsupported or mismatched installer architecture')
    if not installer.BIN.exists():
        raise RuntimeError('Install Sluicework first')
    if not any((args.endpoint, args.install_ollama, args.install_herd, args.install_open_webui, args.open_webui_endpoint)):
        parser.error('Choose --endpoint, --install-ollama, --install-herd, --install-open-webui or --open-webui-endpoint')
    if args.endpoint and (args.install_ollama or args.install_herd):
        parser.error('Choose either reuse or installation for Ollama/Herd')
    if args.open_webui_endpoint and args.install_open_webui:
        parser.error('Choose either reuse or installation for Open WebUI')
    if args.install_herd and not args.allow_herd_lan_proxy:
        parser.error('Herd exposes an unauthenticated LAN proxy on TCP 11434; explicitly opt in with --allow-herd-lan-proxy')
    if args.install_ollama or args.install_herd or args.install_open_webui:
        for command in ('curl', 'tar', 'zstd'):
            if not shutil.which(command):
                raise RuntimeError(f'Install required command first: {command}')
        installer.account('sluiceinference', HOME)
    ollama_endpoint = None
    if args.endpoint or args.install_ollama or args.install_herd:
        endpoint = installer.origin(args.endpoint) if args.endpoint else 'http://127.0.0.1:11434'
        if args.install_ollama:
            install_ollama(arch)
            # Allow startup to settle without loading any model.
            for attempt in range(30):
                try:
                    probe(endpoint)
                    break
                except subprocess.CalledProcessError:
                    if attempt == 29:
                        raise
                    time.sleep(1)
        if args.install_herd:
            endpoint = install_herd(arch, args.router_url, args.allow_herd_lan_proxy)
        probe(endpoint)
        ollama_endpoint = endpoint
        if (installer.ETC / 'server.json').exists():
            as_user('sluiceserver', [installer.BIN, 'local-ai', '--config', installer.ETC / 'server.json', '--endpoint', endpoint, '--name', 'Herd routing' if args.install_herd else 'Local AI'])
        else:
            print(f'Runtime endpoint: {endpoint}. For a remote orchestrator, explicitly configure verified HTTPS access first.')
        print('Use AI connections → Manage models to install model files. GPU drivers are managed separately.')
    if args.install_open_webui or args.open_webui_endpoint:
        backend = installer.origin(args.ollama_url) if args.ollama_url else (ollama_endpoint or 'http://127.0.0.1:11434')
        if args.install_open_webui:
            webui_endpoint = install_open_webui(arch, backend)
            for attempt in range(45):
                try:
                    probe_open_webui(webui_endpoint)
                    break
                except subprocess.CalledProcessError:
                    if attempt == 44:
                        raise
                    time.sleep(1)
        else:
            webui_endpoint = installer.origin(args.open_webui_endpoint)
            probe_open_webui(webui_endpoint)
        if (installer.ETC / 'server.json').exists():
            as_user('sluiceserver', [installer.BIN, 'open-webui', '--config', installer.ETC / 'server.json', '--endpoint', webui_endpoint, '--ollama-url', backend])
        else:
            print(f'Open WebUI endpoint: {webui_endpoint}. For a remote orchestrator, explicitly configure verified HTTPS access in Settings.')
        print('Open WebUI is ready. Models installed into Ollama via Manage models appear automatically.')


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
