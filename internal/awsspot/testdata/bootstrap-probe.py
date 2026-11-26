"""Run the rendered bootstrap only in a disposable, network-isolated container."""

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


mode = sys.argv[1]
stubs = Path('/test-stubs')
stubs.mkdir()
for name in ('apt-get', 'shutdown', 'systemctl'):
    path = stubs / name
    path.write_text('#!/bin/sh\nprintf "%s %s\\n" "${0##*/}" "$*" >> /root/stub-calls\n')
    path.chmod(0o755)
curl = stubs / 'curl'
curl.write_text('''#!/usr/bin/python3
import shutil, sys
shutil.copyfile('/fixture/runner.tar.gz', sys.argv[sys.argv.index('--output') + 1])
''')
curl.chmod(0o755)
env = {**os.environ, 'PATH': str(stubs) + ':' + os.environ['PATH']}

# Simulate account-owned images with both a privileged group and a direct sudo
# grant, rather than relying on the default Ubuntu account configuration.
run('groupadd', '-f', 'docker')
if mode == 'root-user':
    run('useradd', '--non-unique', '--uid', '0', '--create-home', 'runner')
else:
    run('useradd', '--create-home', '--groups', 'sudo,docker', 'runner')
legacy = Path('/etc/sudoers.d/legacy-runner')
legacy.write_text('runner ALL=(ALL) NOPASSWD: ALL\n')
legacy.chmod(0o440)
if mode == 'firewall-failure':
    path = stubs / 'ip6tables'
    path.write_text('#!/bin/sh\nexit 1\n')
    path.chmod(0o755)

result = subprocess.run(['/bin/bash', '/fixture/bootstrap.sh'], env=env)
calls = Path('/root/stub-calls').read_text()
if mode != 'normal':
    assert result.returncode != 0, 'Unsafe bootstrap was accepted'
    assert 'systemctl poweroff --no-block' in calls, 'Failure did not trigger shutdown'
    assert not Path('/usr/local/libexec/sluicework-spot-run').exists()
    print('PASS fail-closed bootstrap:', mode)
    raise SystemExit(0)
assert result.returncode == 0
assert 'poweroff' not in calls, 'Bootstrap unexpectedly requested shutdown'
assert 'NoNewPrivileges=yes' in Path('/etc/systemd/system/sluicework-spot.service').read_text()
assert Path('/run/sluicework-spot/config.json').stat().st_mode & 0o777 == 0o600

# Both fake metadata addresses exist only inside --network=none. Prove root can
# reach them, then run the actual privilege-dropping launcher and test the runner.
run('ip', 'link', 'set', 'lo', 'up')
run('ip', 'addr', 'add', '169.254.169.254/32', 'dev', 'lo')
run('ip', '-6', 'addr', 'add', 'fd00:ec2::254/128', 'dev', 'lo', 'nodad')


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'isolated fake metadata')


class IPv6Server(ThreadingHTTPServer):
    address_family = socket.AF_INET6


servers = [ThreadingHTTPServer(('169.254.169.254', 80), Handler), IPv6Server(('fd00:ec2::254', 80), Handler)]
for server in servers:
    threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    for host in ('169.254.169.254', '[fd00:ec2::254]'):
        output = subprocess.check_output(['/usr/bin/curl', '--noproxy', '*', '--silent', '--fail', '--max-time', '2',
                                          'http://' + host + '/'])
        assert output == b'isolated fake metadata'
    run('/usr/local/libexec/sluicework-spot-run', env=env)
    proof = json.loads(Path('/home/runner/proof.json').read_text())
    assert proof['uid'] != 0 and proof['metadataBlocked'] and proof['privateGrantBlocked']
    assert 'systemctl poweroff --no-block' in Path('/root/stub-calls').read_text()
    print('PASS bootstrap privilege drop, sudo denial, IPv4/IPv6 metadata isolation and shutdown')
finally:
    for server in servers:
        server.shutdown()
        server.server_close()
