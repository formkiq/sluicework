#!/bin/bash
# This file is rendered with a private, one-job configuration. Never enable xtrace.
set -Eeuo pipefail
trap 'systemctl poweroff --no-block' ERR
umask 077
install -d -m 0700 /run/sluicework-spot
cat > /run/sluicework-spot/config.json <<'SLUICEWORK_PRIVATE_CONFIG'
__PRIVATE_CONFIG__
SLUICEWORK_PRIVATE_CONFIG
# Independent lifetime protection works even if the controller becomes unavailable.
remaining=$((__EXPIRES_EPOCH__ - $(date +%s)))
if ((remaining <= 0)); then systemctl poweroff --no-block; exit 0; fi
shutdown -h +$(((remaining + 59) / 60)) >/dev/null 2>&1
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends ca-certificates curl git python3 tar gzip unzip zip file xz-utils iptables util-linux libicu74 libssl3t64 zlib1g libkrb5-3 build-essential pkg-config
id runner >/dev/null 2>&1 || useradd --create-home --shell /bin/bash runner
# Workflow steps run as an unprivileged account: no sudo, and no instance
# metadata (which holds this machine's private user data and runner grant).
rm -f /etc/sudoers.d/sluicework-runner
getent group runner >/dev/null || groupadd runner
[[ $(id -u runner) -ne 0 && $(getent group runner | cut -d: -f3) -ne 0 ]]
# Custom images must not retain docker/admin or other privileged memberships.
usermod --gid runner --groups '' runner
install -d -o runner -g runner -m 0700 /home/runner
iptables -I OUTPUT -m owner --uid-owner runner -d 169.254.169.254 -j REJECT
ip6tables -I OUTPUT -m owner --uid-owner runner -d fd00:ec2::254 -j REJECT
install -d -m 0755 /opt/sluicework-runner /usr/local/libexec
python3 - <<'PY'
import hashlib,json,pathlib,subprocess
p=pathlib.Path('/run/sluicework-spot/config.json');c=json.loads(p.read_text())
archive=pathlib.Path('/run/sluicework-spot/runner.tar.gz')
subprocess.run(['curl','--fail','--silent','--show-error','--location','--proto','=https','--tlsv1.2','--retry','3','--connect-timeout','10','--max-time','180',c['package']['url'],'--output',str(archive)],check=True)
if hashlib.sha256(archive.read_bytes()).hexdigest()!=c['package']['sha256']:raise SystemExit('Runner package checksum failed')
subprocess.run(['tar','-xzf',str(archive),'-C','/opt/sluicework-runner'],check=True)
public={k:c[k] for k in ['job','label']}
e=pathlib.Path('/opt/sluicework-runner/expected-job.json');e.write_text(json.dumps(public));e.chmod(0o644)
PY
chown -R runner:runner /opt/sluicework-runner
chown root:root /opt/sluicework-runner/expected-job.json
cat > /usr/local/libexec/sluicework-spot-start.py <<'PY'
__JOB_START_HOOK__
PY
chmod 0555 /usr/local/libexec/sluicework-spot-start.py
cat > /usr/local/libexec/sluicework-spot-start.sh <<'SH'
#!/bin/bash
exec /usr/bin/python3 /usr/local/libexec/sluicework-spot-start.py
SH
chmod 0555 /usr/local/libexec/sluicework-spot-start.sh
cat > /usr/local/libexec/sluicework-spot-run <<'PY'
#!/usr/bin/python3
import json,os,pathlib,subprocess,time
c=json.loads(pathlib.Path('/run/sluicework-spot/config.json').read_text())
threads=str(c['cpus']);env={**os.environ,'HOME':'/home/runner','ACTIONS_RUNNER_HOOK_JOB_STARTED':'/usr/local/libexec/sluicework-spot-start.sh','CARGO_BUILD_JOBS':threads,'CMAKE_BUILD_PARALLEL_LEVEL':threads,'MAKEFLAGS':'-j'+threads,'NX_PARALLEL':threads,'OMP_NUM_THREADS':threads,'OPENBLAS_NUM_THREADS':threads}
try:
 subprocess.run(['runuser','-u','runner','--','setpriv','--no-new-privs','--','env',*[k+'='+v for k,v in env.items() if k in ['HOME','ACTIONS_RUNNER_HOOK_JOB_STARTED','CARGO_BUILD_JOBS','CMAKE_BUILD_PARALLEL_LEVEL','MAKEFLAGS','NX_PARALLEL','OMP_NUM_THREADS','OPENBLAS_NUM_THREADS']],'/opt/sluicework-runner/run.sh','--jitconfig',c['jit']],cwd='/opt/sluicework-runner',timeout=max(1,c['expires']-int(time.time())),check=False)
except Exception:
 print('Spot runner stopped before completing its assigned build',flush=True)
finally:
 subprocess.run(['systemctl','poweroff','--no-block'],check=False)
PY
chmod 0555 /usr/local/libexec/sluicework-spot-run
cat > /etc/systemd/system/sluicework-spot.service <<'UNIT'
[Unit]
Description=Sluicework single-job Spot runner
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
WorkingDirectory=/opt/sluicework-runner
ExecStart=/usr/local/libexec/sluicework-spot-run
Restart=no
NoNewPrivileges=yes
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now sluicework-spot.service
