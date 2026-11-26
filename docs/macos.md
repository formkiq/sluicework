# Install on macOS

For central network/certificate setup, GitHub App configuration and worker setup
bundles, use the main admin’s **Settings** and **Machines → Enroll machine**.
[Guided setup](admin-settings.md). The service installer below remains available
for boot-time services and dedicated OS accounts.

For opt-in workstation builds, run `bash Sluicework-macOS-arm64.run --workstation "$HOME/Sluicework-pilot"` **without sudo** (choose `amd64` on Intel). This extracts the binary into a new private directory without creating accounts or launch services. Follow [workstation setup](workstation.md) to enroll a CI worker under your current login or an optional dedicated account, and enable capacity only when wanted. The administrator installation below is for persistent cluster services.

The installers target Apple Silicon (arm64) and Intel (amd64/x86-64) Macs running macOS 12 or later. Choose the architecture matching the host. Build both with `bash packaging/build-release.sh`, or set `SLUICEWORK_ARCH=amd64` when invoking the macOS packaging script. You need a local administrator account and a terminal. Build the installer on a development machine or obtain the single release artifact from your team. The installed Mac needs neither Go nor Node.

```sh
./packaging/macos/build-installer.sh
shasum -a 256 release/Sluicework-macOS-arm64.run
```

Distribute the checksum through a trusted channel. On each target Mac, verify the file against that checksum and launch setup.

```sh
shasum -a 256 Sluicework-macOS-arm64.run
sudo /bin/bash ./Sluicework-macOS-arm64.run
```

The service installer also exposes `sluicework` at `/usr/local/bin/sluicework`. Existing commands are preserved. To add the shortcut to an older service installation, run `sudo /Library/Sluicework/bin/sluicework install-cli --system`.

## Create a cluster

1. Choose **Create a cluster** on the first Mac. Supply its worker name and a password for the initial `admin` user. Setup starts the server on `http://127.0.0.1:7443`, enrols the local worker with a one-use token, and installs both system LaunchDaemons.
2. Open the console on that Mac. In **Machines**, confirm that the worker is online with one slot and the `script` and `agent` roles. In **Jobs**, approve a script template for `/usr/bin/uname` with the single argument `-m`. Run it and check the output in **Runs**. An Apple Silicon worker returns `arm64`.
3. To add another Mac, choose HTTPS during setup or rerun the installer and choose **Enable or update cluster HTTPS**. Arrange a stable DNS name that resolves to the orchestrator from both machines. Allow inbound TCP 7443 on the orchestrator from the team's network.
4. Choose the generated cluster CA or supply a PEM certificate chain and corresponding unencrypted private key. A customer leaf certificate must include the stable hostname in its Subject Alternative Name. The installer tests the HTTPS origin before switching the local worker. For a private CA, distribute the **public CA certificate** shown by setup. Compare its SHA-256 fingerprint over a separate trusted channel.
5. For browsers on macOS, explicitly trust the verified private CA using Keychain Access or the command below. The worker can use its own configured PEM CA file. With a publicly trusted certificate, select system trust.

```sh
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain ./cluster-ca.pem
```

Each operator needs a browser account. Choose an unused name and run the local command on the orchestrator. The command reads the new password on standard input. Use the following in macOS Bash to keep it out of shell history and process arguments.

```bash
read -rs -p 'New operator password: ' password && echo
printf '%s\n' "$password" | sudo -u _sluiceserver \
  /Library/Sluicework/bin/sluicework user \
  --config /Library/Sluicework/etc/server.json --name pilot --role operator
unset password
```

## Optional Ollama and Herd setup

At the end of cluster creation or joining, choose an inference runtime, or skip this step. On an existing installation, choose **Configure local AI runtime (Ollama / Herd)** in the current installer. Reuse an existing endpoint, install and start the official Ollama app on macOS 14+, or install optional Herd routing in a managed user environment. The installed app and Herd agents run as the active desktop user; their availability depends on that user's login. They are separate from Sluicework's boot-time server and worker services.

Herd's node can expose Ollama through its unauthenticated LAN proxy, so its installation requires an additional explicit startup opt-in. [Runtime choices, versions, model setup and maintenance](local-models.md#install-the-runtime).

## Join another Mac

1. As an administrator, create a one-use enrolment token in **Machines**. Send it to the operator at the new Mac through a private channel.
2. On that Mac, start the same installer and select **Join a cluster**. Enter the HTTPS cluster URL, its verified CA file if applicable, a worker name, and the token. The service makes outbound requests to the same API as the first worker.
3. In **Machines**, set placement labels such as `build-mac` or `summaries`. Set the host slot cap and optional limits for individual roles. Drain a worker before maintenance and wait for its active attempts to finish. Revoke it to retire its credential. Revocation can leave an in-flight process with an unknown outcome.
4. For a read-only input, install the file on each eligible Mac beneath `/Library/Sluicework/inputs`. Grant read access to the worker group. Include its exact absolute path in the approved input definition and select that path when running or scheduling the template. Selected files may total up to 256 KiB per attempt. AI inputs must be UTF-8 text. Use input placeholders as positional command arguments.

```sh
sudo install -o root -g _sluiceworker -m 0640 ./failures.txt \
  /Library/Sluicework/inputs/failures.txt
```

## Connect local inference over the LAN

For inference on another Mac, give its endpoint a stable private hostname and an HTTPS certificate for that name. Put an HTTPS reverse proxy in front of Ollama or the Herd router if the runtime offers HTTP. Restrict inbound access to the orchestrator. Install the proxy's verified private CA into the orchestrator's **System** keychain using the trust command above, so the headless server can validate its certificate. The worker's `caFile` authenticates the Sluicework server. In **AI connections**, enter the inference endpoint URL and allowlisted model ID, then test. An endpoint on the orchestrator's own loopback interface can use HTTP.

## Services and files

| Item | Location or name |
| --- | --- |
| Executable | `/Library/Sluicework/bin/sluicework` |
| Server settings and worker settings | `/Library/Sluicework/etc/server.json` and `worker.json` |
| SQLite, retained output, provider secret files | `/Library/Sluicework/data`, private to `_sluiceserver` |
| Enrolled credential and scratch directories | `/Library/Sluicework/worker`, private to `_sluiceworker` |
| Generated CA signing material | `/Library/Sluicework/pki`, accessible to root |
| Leaf certificate, service-readable key, optional public CA | The `tls.*` directory referenced in server settings |
| System service labels | `com.sluicework.server` and `com.sluicework.worker` |

View service state with `sudo launchctl print system/com.sluicework.worker` and, on the orchestrator, `sudo launchctl print system/com.sluicework.server`. Inspect **Machines** for the latest authenticated heartbeat. Both installed services start at boot. Placement, capacity, runner metadata and provider changes take effect through the console. To set the retained log byte cap per attempt, set `logLimit` in `server.json` and restart the server after draining active work. After manually editing a service config, drain active work and run `sudo launchctl kickstart -k system/com.sluicework.worker` or its server equivalent.

macOS runs each approved script as `_sluiceworker` through the system’s `/usr/bin/sandbox-exec`. The attempt can use its fresh working directory and selected input copies. The sandbox blocks access to the contents of the worker credential directory, other attempts’ working directories and the configured input source directory. It restricts process inspection and signalling across the sandbox boundary. Other paths, installed tools and network connections use the worker account’s normal permissions. A missing sandbox tool or a rejected profile fails the attempt. Linux foreground workers are intended for development and run scripts under the invoking account’s permissions.

Keep the credential in a dedicated directory, and source files beneath the configured `inputRoot`. Keep approved executables, their dependencies and original input files writable only by trusted maintainers. Administrators must review what each template can run and where it can send data. Pass operator-supplied values as separate positional arguments. GitHub runners use their own installation, OS identity and trust boundary. [GitHub runner setup](github-runner.md)

## Upgrade and certificate renewal

1. Take an off-machine backup. Drain the affected workers and confirm active attempts have finished. For orchestrator maintenance, account for scheduled occurrences that may be missed.
2. Verify the new installer checksum and choose **Upgrade this Mac**. On the orchestrator the installer creates a pre-upgrade SQLite snapshot under `/Library/Sluicework/backups`. It updates the executable and restarts the installed services. Config, credentials and state persist across upgrades.
3. Check console sign-in, worker versions and heartbeats, and run the `uname` template. Enable claims again by clearing drain.
4. To renew the generated one-year leaf certificate, drain the local worker, rerun **Enable or update cluster HTTPS**, and keep the existing hostname and cluster CA. For a customer certificate, supply the renewed chain and matching key. Distribute changed CA roots before switching certificates. If the DNS name changes, update the remote workers’ `url` and `caFile` settings and use **Re-enrol an existing worker after restore** with a fresh token for each Mac. [Recovery procedure](recovery.md)
