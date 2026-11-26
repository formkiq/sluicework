# Linux and mixed-platform clusters

For normal setup, use the main admin's **Settings** and **Machines → Enroll
machine** forms. [Central administration and guided setup](admin-settings.md)
covers HTTPS, certificates, the GitHub App and worker bundles. The service
installation commands below remain available for dedicated hosts.

For opt-in workstation CI, unpack the matching release archive and follow
[workstation setup](workstation.md) using its binary directly. Running `install.sh`
is unnecessary for this foreground mode; it creates persistent cluster services.

Linux and macOS workers share one cluster over verified HTTPS. Either OS can host the orchestrator. Choose OS and architecture requirements in each **Jobs** template; only matching workers can claim that revision. A missing platform stays queued. Existing unconstrained templates remain portable until edited; review their executable paths and file inputs before enrolling another OS. Older workers must upgrade before claiming constrained jobs.

The service installer also exposes `sluicework` at `/usr/local/bin/sluicework`. Existing commands are preserved. To add the shortcut to an older service installation, run `sudo /opt/sluicework/bin/sluicework install-cli --system`.

## Build and install

The initial Linux target is Ubuntu 24.04 LTS with systemd, on ARM64 or x86-64. Build with Go 1.26.11 or a newer fully patched Go release, Node.js 22+ and npm. The release script defaults to the compiler version in `go.mod`:

```sh
bash packaging/build-release.sh
# Or select one target:
bash packaging/build-release.sh release linux/amd64
```

This produces macOS `.run` installers and Linux `.tar.gz` packages for `arm64` and `amd64` (x86-64), plus `SHA256SUMS`. Verify artifacts against a trusted copy of their checksums. On the Linux host:

```sh
sudo apt-get install bubblewrap curl openssl python3 zstd
tar -xzf Sluicework-Linux-amd64.tar.gz
cd Sluicework-Linux-amd64
sudo bash install.sh create --name build-linux
```

This prompts for an admin password and creates a loopback cluster at `http://127.0.0.1:7443`. For network hosting, prepare a certificate/key with the cluster DNS hostname in its SAN:

```sh
sudo bash install.sh create --name orchestrator \
  --url https://cluster.example:7443 \
  --tls-cert /path/server.pem --tls-key /path/server.key \
  --ca /path/cluster-ca.pem
```

Use an unprivileged port such as 7443. Omit `--ca` for system-trusted certificates. An explicit CA requires fingerprint confirmation against the administrator's trusted copy. Configure DNS and firewall access separately. No firewall ports or system trust settings are changed. To join a Mac-hosted or Linux-hosted cluster:

```sh
sudo bash install.sh join --name build-linux \
  --url https://cluster.example:7443 --ca /path/cluster-ca.pem
```

Enter the one-use token from **Machines**. Mac workers use **Join a cluster** against the same HTTPS URL.

## Services and isolation

The binary lives in `/opt/sluicework/bin`, root-owned configuration in `/etc/sluicework`, and state in `/var/lib/sluicework`. Separate unprivileged accounts run the server and worker. Approved input files go under `/var/lib/sluicework/inputs`; grant the worker group read access and keep credentials elsewhere.

```sh
systemctl status sluicework-server sluicework-worker
sudo journalctl -u sluicework-worker -n 100
sudo bash install.sh upgrade
```

Services start at boot without desktop login. Upgrades take a server backup and retain configuration/credentials. Unmanaged accounts and services are refused. An interrupted installation retains state for diagnosis; inspect logs and configuration before resuming with the CLI. Never rerun `init` against an initialized server.

Script execution requires distro-provided `/usr/bin/bwrap` and permitted unprivileged user namespaces. The installer copies that helper to `/opt/sluicework/bin/sluicework-bwrap` and probes it as the worker account. Where Ubuntu AppArmor blocks namespace setup, it installs a named user-namespace exception attached only to that private helper. The worker service enters that named profile before launching jobs. Shared bubblewrap profiles and host-wide restrictions remain in place; a conflicting policy is preserved for administrator review. Source-build testing on Ubuntu can prepare the helper with the packaged `sudo bash install.sh isolation` command.

The host filesystem is read-only inside scripts; credentials, other attempts and source inputs are hidden. Selected files are staged into the writable attempt. A fresh PID namespace hides daemon processes. Approved commands retain network access. Missing or denied isolation fails the attempt, without an unconfined fallback. GitHub runners run separately from this script sandbox.

## Optional Ollama and Herd

```sh
# Preserve and reuse an existing runtime:
sudo bash install.sh inference --endpoint http://127.0.0.1:11434
# Or explicitly install the pinned, checksum-verified official Ollama archive:
sudo bash install.sh inference --install-ollama
```

Managed Ollama runs as `sluiceinference`, binds loopback and stores models under `/var/lib/sluicework/inference/models`. Existing installations/services are preserved. GPU drivers and AMD/Jetson-specific packages are managed separately. Installation does not download models.

Herd uses the locked dependency set and a managed Python 3.12 environment. It requires running Ollama. Its node opens an **unauthenticated Ollama LAN proxy on TCP 11434**, requiring explicit trusted-network opt-in:

```sh
sudo bash install.sh inference --install-herd --allow-herd-lan-proxy
# Node only, using an existing system-trusted HTTPS router:
sudo bash install.sh inference --install-herd --allow-herd-lan-proxy \
  --router-url https://herd.example:11435
```

The local router binds `127.0.0.1:11435`. Services start at boot and disable telemetry, automatic pulls, preloading and substitution. Existing environments and service files are preserved. Inspect `sluicework-ollama`, `sluicework-herd`, `sluicework-herd-node` and `sluicework-open-webui` with systemctl/journalctl. Configure remote HTTPS explicitly; private CAs must already be trusted by both the host and managed Python environment. Install models through **AI connections → Manage models**. [Model setup](local-models.md).

To install the optional Open WebUI **0.13.4** chat companion (`127.0.0.1:8080`, wired to local Ollama or Herd) or register an existing Open WebUI endpoint:

```sh
# Optional slim chat UI; local document extraction/embedding is not included
sudo bash install.sh inference --install-open-webui
# Or point at Herd or reuse an existing Open WebUI origin:
sudo bash install.sh inference --install-open-webui --ollama-url http://127.0.0.1:11435
sudo bash install.sh inference --open-webui-endpoint http://127.0.0.1:8080
```

## Validation

Run `go test -race -buildvcs=false ./...` as an unprivileged account with bubblewrap, and `python3 -B -m unittest discover -s packaging/linux/tests -p 'test_*.py'`. The CI matrix uses explicitly selected native OS/architecture runners and tests Linux installation, isolated execution, service restart persistence and upgrade. Physical reboot, GPU inference and mixed physical-host operation need additional pilot checks.
