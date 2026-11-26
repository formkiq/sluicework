"""Install a packaged Linux build on a systemd host. No inference is installed by default."""

import argparse
import getpass
import ipaddress
import json
import os
from pathlib import Path
import platform
import pwd
import re
import shutil
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit

SOURCE = Path(__file__).resolve().parent
ETC = Path('/etc/sluicework')
STATE = Path('/var/lib/sluicework')
BIN = Path('/opt/sluicework/bin/sluicework')
UNITS = Path('/etc/systemd/system')


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, **kwargs)


def as_user(user, args, **kwargs):
    return run(['/usr/sbin/runuser', '-u', user, '--', *args], **kwargs)


def origin(value, remote=False):
    if any(c.isspace() or ord(c) < 32 or c in '"\\%$' for c in value):
        raise ValueError('Unsupported character in endpoint')
    u = urlsplit(value)
    if (not u.hostname or u.username is not None or u.password is not None or
            u.path not in ('', '/') or u.query or u.fragment or u.scheme not in ('http', 'https')):
        raise ValueError('Use an HTTP(S) origin without credentials or paths')
    if u.port is not None and not 1 <= u.port <= 65535:
        raise ValueError('Invalid endpoint port')
    try:
        ipaddress.ip_address(u.hostname)
    except ValueError:
        if not re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?', u.hostname):
            raise ValueError('Invalid endpoint hostname')
    if u.scheme != 'https' and (remote or u.hostname not in ('localhost', '127.0.0.1', '::1')):
        raise ValueError('Network connections require verified HTTPS')
    return value.rstrip('/')


def atomic_copy(source, dest, mode=0o644, replace=False):
    dest = Path(dest)
    if dest.is_symlink() or (dest.exists() and not replace):
        raise RuntimeError(f'Existing file preserved: {dest}')
    dest.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=dest.parent, delete=False) as f:
        candidate = Path(f.name)
        with Path(source).open('rb') as src:
            shutil.copyfileobj(src, f)
        os.fchmod(f.fileno(), mode)
        f.flush()
        os.fsync(f.fileno())
    try:
        if replace:
            os.replace(candidate, dest)
        else:
            os.link(candidate, dest)
    finally:
        candidate.unlink(missing_ok=True)


def write_json(dest, value, replace=False):
    with tempfile.TemporaryDirectory() as temp:
        source = Path(temp) / 'config.json'
        source.write_text(json.dumps(value, indent=2) + '\n')
        atomic_copy(source, dest, replace=replace)


def account(name, home):
    marker = ETC / ('account.' + name)
    try:
        identity = pwd.getpwnam(name)
        if not marker.is_file() or identity.pw_uid == 0:
            raise RuntimeError(f'Existing service identity is not managed by Sluicework: {name}')
    except KeyError:
        run(['useradd', '--system', '--user-group', '--home-dir', home, '--shell', '/usr/sbin/nologin', name])
        marker.touch(mode=0o600)
        identity = pwd.getpwnam(name)
    home.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chown(home, identity.pw_uid, identity.pw_gid)
    os.chmod(home, 0o700)


def unit(kind):
    target = UNITS / f'sluicework-{kind}.service'
    marker = ETC / ('unit.' + kind)
    if target.exists() and not marker.exists():
        raise RuntimeError(f'Existing service is not managed by Sluicework: {target}')
    atomic_copy(SOURCE / target.name, target, replace=True)
    marker.touch(mode=0o600)
    run(['systemctl', 'daemon-reload'])


def wait(url, ca):
    command = ['curl', '--disable', '--noproxy', '*', '--fail', '--silent', '--show-error', '--max-time', '2']
    if ca:
        command += ['--cacert', ca]
    for _ in range(20):
        try:
            run([*command, url + '/api/v1/openapi.yaml'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            return
        except subprocess.CalledProcessError:
            time.sleep(1)
    raise RuntimeError('Server is not ready; inspect journalctl -u sluicework-server')


def trusted_ca(source):
    if not source:
        return ''
    run(['openssl', 'x509', '-in', source, '-noout', '-subject', '-fingerprint', '-sha256'])
    if input('Compare this fingerprint with the trusted administrator copy. Trust it? [y/N]: ').lower() != 'y':
        raise RuntimeError('CA trust declined')
    target = ETC / 'cluster-ca.pem'
    atomic_copy(source, target)
    return str(target)


def install_binary():
    atomic_copy(SOURCE / 'sluicework', BIN, mode=0o755, replace=True)
    try:
        run([BIN, 'install-cli', '--system'])
    except subprocess.CalledProcessError:
        print(f'CLI shortcut setup needs attention; Use {BIN} directly.', file=sys.stderr)


def worker_config(url, name, ca):
    return dict(url=url, name=name, caFile=ca, credentialFile=str(STATE / 'worker/credential.json'),
                workDir=str(STATE / 'worker/jobs'), inputRoot=str(STATE / 'inputs'), roles=['script', 'agent'])


def setup_worker(url, name, ca):
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,62}', name):
        raise ValueError('Use a worker name of up to 63 letters, numbers, dots, underscores or hyphens')
    account('sluiceworker', STATE / 'worker')
    install_isolation()
    inputs = STATE / 'inputs'
    inputs.mkdir(exist_ok=True, mode=0o750)
    os.chown(inputs, 0, pwd.getpwnam('sluiceworker').pw_gid)
    write_json(ETC / 'worker.json', worker_config(url, name, ca))
    unit('worker')


def install_isolation():
    helper = BIN.parent / 'sluicework-bwrap'
    marker = ETC / 'isolation-helper'
    if helper.exists() and not marker.is_file():
        raise RuntimeError('Existing sandbox helper is unmanaged; it was preserved')
    atomic_copy('/usr/bin/bwrap', helper, mode=0o755, replace=True)
    marker.touch(mode=0o600)
    command = [helper, '--unshare-user', '--unshare-pid', '--unshare-ipc', '--unshare-uts', '--ro-bind', '/', '/', '--proc', '/proc', '--dev', '/dev', '--', '/usr/bin/true']
    try:
        as_user('sluiceworker', command, capture_output=True)
        return
    except subprocess.CalledProcessError:
        pass
    # Ubuntu's userns restriction needs a named profile. Attach it only to our
    # private helper, preserving all shared /usr/bin/bwrap profiles and sysctls.
    parser = shutil.which('apparmor_parser')
    if not parser or not Path('/sys/module/apparmor/parameters/enabled').is_file():
        raise RuntimeError('Sandbox probe failed. User namespaces must be supported; inspect the host isolation policy.')
    target = Path('/etc/apparmor.d/sluicework-bwrap')
    source = SOURCE / 'sluicework-bwrap.apparmor'
    if target.exists() and target.read_bytes() != source.read_bytes():
        raise RuntimeError('Existing Sluicework AppArmor policy differs; it was preserved for administrator review')
    if not target.exists():
        atomic_copy(source, target)
    run([parser, '-r', target])
    as_user('sluiceworker', command)


def install(args):
    if (ETC / 'server.json').exists() or (ETC / 'worker.json').exists():
        raise RuntimeError('Existing configuration preserved. Use upgrade for an installed machine.')
    name = args.name or platform.node().split('.')[0]
    url = origin(args.url, remote=True) if args.action == 'join' else 'http://127.0.0.1:7443'
    ca = trusted_ca(args.ca)
    if args.action == 'join':
        wait(url, ca)
    if bool(args.tls_cert) != bool(args.tls_key):
        raise ValueError('Supply both --tls-cert and --tls-key')
    if args.action == 'create':
        password = getpass.getpass('New admin password (12–72 UTF-8 bytes): ')
        confirm = getpass.getpass('Confirm admin password: ')
        if password != confirm or not 12 <= len(password.encode()) <= 72:
            raise ValueError('Password mismatch or invalid length')
        account('sluiceserver', STATE / 'server')
        config = dict(dataDir=str(STATE / 'server/data'), listen='127.0.0.1:7443')
        if args.url or args.tls_cert:
            url = origin(args.url or '', remote=True)
            if not args.tls_cert:
                raise ValueError('Network hosting requires --url, --tls-cert and --tls-key')
            if (urlsplit(url).port or 443) < 1024:
                raise ValueError('Choose an unprivileged cluster port, such as 7443')
            atomic_copy(args.tls_cert, ETC / 'server.pem')
            atomic_copy(args.tls_key, ETC / 'server.key', mode=0o640)
            os.chown(ETC / 'server.key', 0, pwd.getpwnam('sluiceserver').pw_gid)
            config.update(listen='0.0.0.0:' + str(urlsplit(url).port or 443), tlsCert=str(ETC / 'server.pem'), tlsKey=str(ETC / 'server.key'))
        write_json(ETC / 'server.json', config)
    setup_worker(url, name, ca)
    install_binary()
    if args.action == 'create':
        as_user('sluiceserver', [BIN, 'init', '--config', ETC / 'server.json'], input=password + '\n', text=True)
        del password, confirm
        unit('server')
        run(['systemctl', 'enable', '--now', 'sluicework-server'])
        wait(url, ca)
        token = as_user('sluiceserver', [BIN, 'enroll-token', '--config', ETC / 'server.json'], capture_output=True, text=True).stdout
    else:
        token = getpass.getpass('One-use enrollment token: ') + '\n'
    as_user('sluiceworker', [BIN, 'join', '--config', ETC / 'worker.json'], input=token, text=True)
    del token
    run(['systemctl', 'enable', '--now', 'sluicework-worker'])
    print(f'Sluicework ready: {url}. Configure platform requirements in Jobs and GitHub runs-on labels in workflows.')
    print('Optional inference: sudo ./install.sh inference --help')


def upgrade():
    if not (ETC / 'worker.json').is_file() or not BIN.is_file():
        raise RuntimeError('No installed worker configuration found')
    server = (ETC / 'server.json').is_file()
    install_isolation()
    services = ['sluicework-worker'] + (['sluicework-server'] if server else [])
    if server:
        dest = STATE / 'server' / ('upgrade-' + time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
        as_user('sluiceserver', [BIN, 'backup', '--config', ETC / 'server.json', '--out', dest])
        print(f'Pre-upgrade backup: {dest}')
    run(['systemctl', 'stop', *services])
    install_binary()
    for kind in (['server', 'worker'] if server else ['worker']):
        unit(kind)
    run(['systemctl', 'start', *reversed(services)])
    print('Upgrade installed. Check service state and machine heartbeats.')


def main():
    if len(sys.argv) > 1 and sys.argv[1] == 'inference':
        return run([sys.executable, '-I', SOURCE / 'runtime.py', *sys.argv[2:]])
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    for action in ('create', 'join'):
        command = sub.add_parser(action)
        command.add_argument('--name')
        command.add_argument('--url', required=action == 'join')
        command.add_argument('--ca', default='')
        command.add_argument('--tls-cert', default='')
        command.add_argument('--tls-key', default='')
    sub.add_parser('upgrade')
    sub.add_parser('isolation', help='Prepare the private sandbox helper and scoped AppArmor user-namespace exception')
    sub.add_parser('inference', help='Optional Ollama and Herd setup')
    args = parser.parse_args()
    if sys.platform != 'linux' or os.geteuid() != 0:
        raise RuntimeError('Run the Linux installer with sudo on a systemd host')
    arch = {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
    if arch != (SOURCE / 'target-arch').read_text().strip():
        raise RuntimeError('Installer architecture does not match this machine')
    dependencies = ['systemctl', 'runuser', 'useradd', 'curl', 'openssl']
    if args.action in ('create', 'join', 'isolation'):
        dependencies.append('bwrap')
    for command in dependencies:
        if not shutil.which(command):
            raise RuntimeError(f'Install required command first: {command}')
    if not Path('/run/systemd/system').is_dir():
        raise RuntimeError('A running systemd host is required')
    ETC.mkdir(parents=True, exist_ok=True, mode=0o755)
    STATE.mkdir(parents=True, exist_ok=True, mode=0o755)
    BIN.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
    if args.action == 'isolation':
        account('sluiceworker', STATE / 'worker')
        install_isolation()
    elif args.action == 'upgrade':
        upgrade()
    else:
        install(args)


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
