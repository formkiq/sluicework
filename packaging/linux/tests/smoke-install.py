"""Exercise systemd installation only on disposable GitHub Actions Linux VMs."""
import http.cookiejar
import json
import os
from pathlib import Path
import platform
import secrets
import subprocess
import sys
import time
import urllib.request

if os.geteuid() != 0 or os.environ.get('GITHUB_ACTIONS') != 'true':
    sys.exit('This installation smoke test requires root on a disposable GitHub Actions VM')

package = Path(sys.argv[1]).resolve()
# Hosted images may make this prefix writable by everyone. The installer
# deliberately refuses such directories. Establish production-like permissions
# only in this disposable VM; installation itself never changes an existing dir.
cli_dir = Path('/usr/local/bin')
assert cli_dir.is_dir() and not cli_dir.is_symlink()
cli_dir.chmod((cli_dir.stat().st_mode & 0o777) & ~0o022)
password = secrets.token_urlsafe(24)
subprocess.run(['bash', package / 'install.sh', 'create', '--name', 'linux-ci'], input=f'{password}\n{password}\n', text=True, check=True)
assert Path('/usr/local/bin/sluicework').resolve() == Path('/opt/sluicework/bin/sluicework')
subprocess.run(['/usr/local/bin/sluicework', 'version'], check=True)
cookies = http.cookiejar.CookieJar()
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies))


def request(path, body=None):
    req = urllib.request.Request('http://127.0.0.1:7443/api/v1' + path,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={'Content-Type': 'application/json', 'Origin': 'http://127.0.0.1:7443'})
    with client.open(req, timeout=10) as response:
        return json.load(response)


request('/login', {'username': 'admin', 'password': password})
for _ in range(20):
    machines = request('/machines')
    if machines and machines[0]['online']:
        break
    time.sleep(1)
else:
    raise RuntimeError('Installed worker did not become online')
worker = machines[0]
arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}[platform.machine()]
assert worker['os'] == 'linux' and worker['arch'] == arch, worker
job = request('/jobs', {'name': 'Linux installed-worker smoke', 'type': 'script', 'os': 'linux', 'arch': arch,
                        'command': '/usr/bin/uname', 'args': ['-m']})
run = request('/jobs/' + job['id'] + '/runs', {'inputs': {}})
for _ in range(25):
    result = request('/runs/' + run['id'])
    if result['state'] not in ('queued', 'running'):
        break
    time.sleep(1)
assert result['state'] == 'succeeded', result
assert result['attempts'][0]['output'].strip() == platform.machine(), result
subprocess.run(['systemctl', 'restart', 'sluicework-server', 'sluicework-worker'], check=True)
for _ in range(20):
    try:
        request('/session')
        break
    except Exception:
        time.sleep(1)
else:
    raise RuntimeError('Services did not restart')
assert request('/runs/' + run['id'])['state'] == 'succeeded'
subprocess.run(['bash', package / 'install.sh', 'upgrade'], check=True)
print('Linux install, native isolated execution, restart persistence and upgrade passed')
subprocess.run(['systemctl', 'stop', 'sluicework-worker', 'sluicework-server'], check=True)
