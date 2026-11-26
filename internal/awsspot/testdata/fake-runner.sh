#!/bin/bash
set -euo pipefail
exec /usr/bin/python3 - <<'PY'
import json, os, pathlib, subprocess

status = pathlib.Path('/proc/self/status').read_text().splitlines()
values = dict(line.split(':', 1) for line in status if ':' in line)
assert os.getuid() != 0
assert set(os.getgroups()) <= {os.getgid()}, os.getgroups()
assert values['NoNewPrivs'].strip() == '1'
assert int(values['CapEff'].strip(), 16) == 0
assert subprocess.run(['/usr/bin/sudo', '-n', '/usr/bin/id', '-u'], capture_output=True).returncode != 0
try:
    pathlib.Path('/run/sluicework-spot/config.json').read_text()
except PermissionError:
    pass
else:
    raise AssertionError('Runner could read the private bootstrap grant')
for host in ('169.254.169.254', '[fd00:ec2::254]'):
    result = subprocess.run(['/usr/bin/curl', '--noproxy', '*', '--silent', '--fail', '--max-time', '2',
                             'http://' + host + '/'], capture_output=True)
    assert result.returncode != 0, 'Runner reached fake IMDS: ' + host
pathlib.Path('/home/runner/proof.json').write_text(json.dumps({'uid': os.getuid(), 'noNewPrivileges': True,
                                                            'metadataBlocked': True, 'privateGrantBlocked': True}))
PY
