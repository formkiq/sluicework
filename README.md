# Sluicework

[MIT license](LICENSE) · [Build and test](#build-and-test)

Sluicework runs approved scripts and AI tasks on networked Linux and Mac machines. One machine hosts the orchestrator, its embedded web console and a worker. Additional workers connect outbound over HTTPS. Job templates select Linux, macOS or either, and ARM64, x86-64 or either. Only matching workers can claim a run; its approved platform requirements persist through retries.

Build all four platform/architecture installers with `bash packaging/build-release.sh`. Use the `arm64` artifact for ARM64 or `amd64` for x86-64. [Linux installation and services](docs/linux.md) · [Explicit GitHub Actions runner placement](docs/github-runner.md).

## Install on a Mac

Build the installer using the commands below, then run it from the generated `release/` directory on the target Mac.

```bash
sudo /bin/bash ./Sluicework-macOS-arm64.run
```

Choose **Create a cluster** on the first Mac. Sign in at the console URL printed by the installer using the `admin` account and the password you chose. Open **Jobs**, run **Worker check**, then open its entry in **Runs** to see the assigned machine and output. To add a second Mac, configure the cluster's stable HTTPS hostname, issue a token in **Machines**, and select **Join a cluster** on that Mac. The installers target macOS 12 and later on ARM64 or x86-64; choose the matching architecture artifact. Its optional runtime setup can reuse Ollama or Herd, install the official Ollama app on macOS 14+, or install isolated Herd user agents. Choose **Configure local AI runtime** to revisit this step. [Runtime setup](docs/local-models.md#install-the-runtime).

[Installation, HTTPS and upgrades](docs/macos.md) · [Local test-failure summary](examples/local-summary/README.md) · [GitHub runner setup](docs/github-runner.md)

## Run a cluster

- **Machines** shows heartbeats, versions, labels, slots, and registered CI runners. Drain a machine to let its current work finish. An idle worker claims an eligible attempt in a SQLite transaction and renews a 30-second lease. Losing the lease interrupts the attempt and records its outcome as unknown.
- **Jobs** holds approved script and AI templates. Queuing a run freezes its revision and selected inputs. Cron schedules use five fields and an IANA time zone, with durable records for dispatched, missed and overlapping occurrences. Manual retries preserve previous attempts. A template marked safe to repeat may opt into bounded automatic retries.
- **Runs** provides live logs, cursor-based history, cancellation and retry. The default retained log cap is 1 MiB per attempt. Set `logLimit` in the server JSON to change it. Each template also controls result size and retention.
- **AI connections** includes **Set up local AI** to choose a model preset, download it into a running Ollama endpoint, and configure context length and memory retention. **Manage models** shows installed models, persistent download progress, cancellation and retries. [Local model setup](docs/local-models.md). It also supports Ollama, Herd, OpenAI, Anthropic and Gemini. The orchestrator holds provider keys in private files under its service account and resolves each request against the active attempt. The template selects the connection, allowed model and cloud permission. A local connection failure is recorded against that connection.
- **Provider access** offers shared OpenAI, Claude and Gemini API keys and private, OS-user-owned ChatGPT agents for explicitly approved recurring prompts, optional public web research with citations, and status-only desktop notifications. Personal subscription credentials stay in that user’s runtime. [Personal scheduled tasks](docs/personal-agent.md). [Provider access and limitations](docs/provider-access.md).
- **GitHub Actions** uses an official runner registered with GitHub and reported by the matching Mac. Overview, Runs and machine cards show verified local build activity and recent history when the GitHub App has Actions read access, with direct links to GitHub logs and artifacts. Sync failures retain clearly marked last-known status. [Workstation build activity](docs/workstation.md#build-activity-in-the-admin).

Each cluster is one shared team workspace. Operators can inspect runs, execute and schedule approved jobs, manage capacity, and request cancellation or retry. Administrators also approve template revisions, enroll and revoke workers, edit machine labels, register CI runners, and manage shared provider connections. Members can prepare personal task proposals, but only their local OS login can approve or control their subscription agent. Use the local `user` command to provision accounts or reset passwords.

The server publishes its OpenAPI description at `/api/v1/openapi.yaml`. Backups capture committed SQLite state, retained logs/results and the server configuration. Restore checks the snapshot, fences old credentials and preserves schedules and history. Credentials for cloud providers are re-entered on the replacement server. [API contract](docs/openapi.yaml) · [Backup and restore](docs/recovery.md)

## Build and test

For opt-in personal-machine CI, see [Workstation builds and explicit cloud fallback](docs/workstation.md).
The macOS `--workstation` package includes a `manage` helper for setup, background
startup, status, enable/pause, graceful shutdown and local password reset under
your existing login. Run `"$HOME/Sluicework-pilot/manage" setup` after extraction.
With updated server and controllers, manage each workstation's build threads
(1–8), CPU/memory admission gates, idle time, AC power and availability window in
**Machines → GitHub runner → Configure**. The main admin manages local and
networked workers; their cards distinguish requested and reported settings.
Updates apply safely between builds without restarting the server or controller.
The main admin's **Settings** page also configures network HTTPS/certificates,
the GitHub App connection and retained-output limits. **Enroll machine** prepares
platform-specific worker setup without a separate worker administrator or manual
JSON editing. [Central administration and guided setup](docs/admin-settings.md).

**Actions runners** manages repository and optional organization GitHub runners,
including external hosts, labels, guided registration/removal and the published
placement policy of compatible workflows. [GitHub Actions administration](docs/github-actions-admin.md).

Use Go 1.25 or newer and Node.js 22 or newer with npm. Build the console first so Go can embed its production assets. The installer packaging script also builds the console when run on its own.

```bash
(cd web && npm ci && npm run build)
go test -race -buildvcs=false ./...
go vet -buildvcs=false ./...
node --test integrations/github-actions/*.test.mjs integrations/github-actions/priority/*.test.mjs
go build -buildvcs=false -o sluicework ./cmd/sluicework
bash packaging/build-release.sh
bash packaging/macos/tests/inference_test.sh
python3 -B -m unittest discover -s packaging/macos/tests -p 'test_*.py'
python3 -B -m unittest discover -s packaging/linux/tests -p 'test_*.py'
```

Go entry points live in `cmd/sluicework/`. The server and worker packages live in `internal/`, with Go tests beside the code they exercise. The React console lives in `web/src/`, its static assets in `web/public/`, and its TypeScript checks run during `npm run build`. Platform installers live in `packaging/macos/` and `packaging/linux/`; `packaging/build-release.sh` builds any or all supported targets.

Run an individual Go test with `go test ./cmd/sluicework -run '^TestLiveBackupAndReplacementRestore$'`. Check Go formatting with `gofmt -l cmd docs internal web/embed.go`.

The same binary accepts `init`, `server`, `user`, `enroll-token`, `join`, `worker`, `backup`, `restore`, `local-ai`, `runtime-check`, `open-webui`, `open-webui-check`, `codex`, `coding-client`, `install-cli`, and `version`. The `workstation`, `workstation-install`, `workstation-enable`, `workstation-pause` and `workstation-status` commands manage optional CI capacity. Use `sluicework <command> -h` for flags. The service reads the JSON file selected by `--config`. `runtime-check --endpoint URL` checks a runtime without loading a model; `local-ai --config FILE --endpoint URL` provisions a local connection while preserving any existing route for that endpoint; `open-webui-check --endpoint URL` checks Open WebUI's `/health` endpoint and `open-webui --config FILE --endpoint URL` registers it in cluster settings without overwriting existing configuration. Installer tests require Bash on macOS and Python 3; the optional Herd and Open WebUI installations provision their own Python. Admin passwords and enrollment tokens are read from standard input.

The automated suite exercises competing claims, lease expiry, cancellation, retries, real subprocess execution, log replay, role permissions, DST and restart scheduling, protected key rotation, four provider protocols, and backup and restore. The physical-Mac pilot also checks reboot, a labeled GitHub Actions workflow and calls with customer-owned provider credentials. [Pilot procedure](docs/pilot.md)

### Try the source build on loopback

The following Bash session creates a disposable cluster with an unprivileged local worker. Run it from the directory containing the newly built `sluicework` binary.

```bash
umask 077
cluster_dir=$(mktemp -d /tmp/sluicework.XXXXXX)
cat > "$cluster_dir/server.json" <<JSON
{"dataDir":"$cluster_dir/data","listen":"127.0.0.1:7443"}
JSON
cat > "$cluster_dir/worker.json" <<JSON
{"url":"http://127.0.0.1:7443","name":"local","roles":["script","agent"],
 "credentialFile":"$cluster_dir/worker/credential.json","workDir":"$cluster_dir/worker/jobs"}
JSON
read -rs -p 'Choose an admin password (12–72 bytes): ' cluster_password && echo
printf '%s\n' "$cluster_password" | ./sluicework init --config "$cluster_dir/server.json"
unset cluster_password
./sluicework server --config "$cluster_dir/server.json" &
server_pid=$!
for ((i=0; i<30; i++)); do
  curl -fsS http://127.0.0.1:7443/api/v1/openapi.yaml -o /dev/null && break
  sleep 1
done
./sluicework enroll-token --config "$cluster_dir/server.json" |
  ./sluicework join --config "$cluster_dir/worker.json"
./sluicework worker --config "$cluster_dir/worker.json" &
worker_pid=$!
```

Open the console at `http://127.0.0.1:7443` and run **Worker check**. End the local session with `kill "$worker_pid" "$server_pid"`. The test data stays in `$cluster_dir`.

### Repository priority routing

Use the [shared priority dispatcher](docs/priority-routing.md) for fifteen-minute
high-priority and sixty-minute low-priority queues without a hosted runner
waiting. Workstation mode uses AWS Lambda/DynamoDB; an always-on server can run
the same engine with `sluicework github-queue --config FILE` and SQLite.
Repository priority and runner order are managed in Actions runners.

### Controlled coding clients

Use Codex on a separate Mac or Linux workstation through Sluicework-managed HTTPS, scoped credentials, model policy and inference admission. Configure **AI connections → Coding clients** and follow [coding client setup](docs/coding-clients.md). No direct Ollama access is needed on the coding workstation.

## Source distribution

This repository is a standalone source distribution with generic configuration
examples. Build installers locally using the commands above; no account-specific
App installation, cloud resources or private runner access is included. Replace
example IDs and names before connecting your own infrastructure. Public CI runs
on hosted Linux; macOS execution testing requires a trusted Mac.
