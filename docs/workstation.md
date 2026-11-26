# Optional workstation builds

Use this mode on a Mac or Linux workstation that should offer spare capacity
only when you enable it. It uses GitHub's official one-job (JIT/ephemeral) runner,
not the continuously available runner service described in [github-runner.md](github-runner.md).
No system service, login item, Ollama, or Herd is started by these commands.

## Personal Mac trial with your current login

You can run the controller under your existing Mac login while using other apps.
The required separate `ci` worker is a Sluicework registration/configuration, not
a separate macOS user. Run without `sudo`. A dedicated account remains an optional
security boundary: same-login workflows can access files and credentials available
to that login, including a local server's App key. Use trusted, manually dispatched
builds for this trial; one-job runners are not virtual machines.

### Install prerequisites and extract the package

The personal-Mac helper requires Python 3.9+, Xcode Command Line Tools, rustup
and Python 3.12 for the pilot builds. Check the tools first:

```sh
xcrun --find clang
command -v rustup
python3.12 --version
```

If needed, install the missing Homebrew formulas:

```sh
brew install rustup python@3.12
```

The helper finds Homebrew's rustup proxies without changing your
shell profile. The workflow installs its pinned Rust toolchain and provisions
Node/Hugo; Python build packages use a job-local virtual environment.

Download the matching macOS installer and verify its supplied checksum. Extract
it into a new directory, without sudo:

```sh
shasum -a 256 -c SHA256SUMS
bash Sluicework-macOS-arm64.run --workstation "$HOME/Sluicework-pilot"
```

Use the `amd64` package on Intel Macs. Extraction refuses an existing destination
and installs `sluicework` plus the `manage` helper. For an existing manual pilot,
keep its directory and configurations. From a current source checkout, install
only the helper:

```sh
install -m 0700 packaging/macos/workstation.py "$HOME/Sluicework-pilot/manage"
```

### Command-line access

Workstation extraction exposes `sluicework` through `~/.local/bin/sluicework`.
When that directory is missing from PATH, setup adds a guarded block to the
current shell's startup files. Open a new terminal afterward. Existing commands
and symlinked startup files are preserved; setup reports any conflict.
For an existing pilot, run `"$HOME/Sluicework-pilot/manage" install-cli`, or
`"$HOME/Sluicework-pilot/sluicework" install-cli`. Use `--no-shell` with the binary
command to manage PATH yourself. This does not start or restart any services.

### Complete setup once

```sh
"$HOME/Sluicework-pilot/manage" setup
```

For a new pilot, setup creates private bootstrap configuration using absolute
paths. Configure the GitHub App through **Settings → GitHub App connection** in
the main admin; no App key is needed on another worker. Setup prompts for a new
local admin password only when initializing a new
database. An existing pilot retains its configurations, password, enrollment and
runner installation. A partial config pair is reported for manual completion
instead of being overwritten.

Setup starts or adopts the matching local server, enrolls the CI worker,
downloads the checksum-verified official runner if needed, and starts the
controller. Approve it from **Machines → GitHub runner → Configure**. Legacy
explicit App provisioning can instead prompt for your **Sluicework admin
password** to approve the configured repository; the password and session cookie
are not saved. Existing registrations
and custom labels are preserved. No GitHub password is requested.

Fresh pilots start paused. The console is at `http://127.0.0.1:7443`, reachable
only on this Mac; sign in as `admin`. The helper manages this same-Mac HTTP
loopback setup. Use **Settings → Network
access** for an additional HTTPS endpoint and **Machines → Enroll machine** for
remote setup bundles. [Guided setup](admin-settings.md).

### Start, enable, pause and stop

Commands work from any terminal without `SW` or `SW_PILOT` variables:

```sh
"$HOME/Sluicework-pilot/manage" start
"$HOME/Sluicework-pilot/manage" status
"$HOME/Sluicework-pilot/manage" enable
"$HOME/Sluicework-pilot/manage" pause
"$HOME/Sluicework-pilot/manage" stop
```

`start` launches missing processes in the background and writes `server.log` and
`controller.log` beside the helper. Closing Terminal does not stop these managed
processes. No login item or automatic restart after a reboot is installed.
`enable` explicitly offers capacity for the configured window (two hours by
default). `pause` withdraws capacity while assigned work finishes. `stop` pauses,
drains the controller, then stops the matching server. Drain can take over a
minute; if it is still busy after two minutes, the server stays running and you
can retry after the build finishes. A newly started controller always begins
paused; use **Offer capacity** in the main admin or local `enable` to accept
builds. Reusing an already running controller
preserves its current availability window.

For a forgotten local admin password, use the terminal's hidden prompts:

```sh
"$HOME/Sluicework-pilot/manage" reset-password
```

Fresh configs use `minIdleSeconds: 0`, allowing typing and other apps while
retaining AC power, thermal, CPU and memory admission checks, one build at a
time, background priority and two build threads. These are admission gates,
not hard CPU/RAM caps. Once both the server and controller are updated, use
**Machines → GitHub runner → Configure** to manage resource settings centrally.
Existing installation defaults remain in effect until an admin saves a profile.

### Adjust build parallelism

In the main admin, open **Machines → GitHub runner → Configure**, choose
**Build threads** from **1–8**, and save. Resource settings are directly editable.
The same form controls the CPU load admission threshold, minimum free
memory, input idle time, AC power requirement and availability-window length.
Each enrolled machine has its own profile; networked workers need no separate
admin console. These changes require an administrator account.

The worker receives the requested profile over its existing authenticated
outbound heartbeat. Its card shows requested and reported settings. An offline
worker retains its last reported values until it reconnects. A busy runner
finishes with its original profile. Sluicework withdraws idle capacity and allows
the assignment-settling window (about 75 seconds) before replacing its listener.
The next listener uses the new profile without restarting the server or controller.

Saving a resource profile does not enable a paused machine or extend its current
opt-in expiry. Use **Offer capacity** or **Pause capacity** on the machine card for
explicit availability changes, or keep using the local helper. The availability
window supplies the default offer duration. A newer local pause wins over an
older admin request, and a restarted controller always needs a fresh offer.
Choose **Restore installation defaults**, then save, to return to the
installation's defaults. This also takes effect safely between builds. Profiles
are persisted and audited in the cluster database and included in its backup.

New installations default to two threads. Four is a conservative starting point
while using other apps; compare responsiveness and build duration before trying
six or eight. This controls compiler parallelism preferences, not a hard CPU/core
limit. One controller still accepts only one build at a time. Hosted fallback
parallelism is controlled separately by the workflow.

Both server and controller must support central settings. Older controllers show
an upgrade message instead of an editable form. The initial server URL, trust,
credential and host-directory paths remain installation/bootstrap settings;
worker identity files are not editable configuration.

### Test a trusted workflow

First add a manually dispatched workflow in your own private test repository.
Copy the exact machine label from `manage status` and require it alongside
`self-hosted`, `macOS` and `ARM64` in `runs-on`. Start with a harmless `hostname`
step and confirm the runner identity in GitHub before enabling builds or secrets.
The generic adapters in `integrations/github-actions/` support explicit placement
and fallback once their repository-specific workflows have been reviewed.

## Connect the GitHub App

Install a repository-scoped GitHub App with **Administration: read/write**,
**Metadata: read** and **Actions: read**. Install it only on your approved
test repositories. This server-to-server connection needs neither an OAuth
callback nor a webhook.

In the main admin, open **Settings → GitHub App connection**, enter the App ID,
installation ID and repository, and upload the downloaded PEM private key. Save,
then **Test saved connection**. No server restart is required. Drain registrations
before changing the App, installation or repository; replacing the key for the
same connection preserves registrations.

The server keeps the key private and never returns its contents. Workers receive
one-run credentials scoped to the approved repository. A shared OS login still
allows trusted CI jobs to access files readable by that login, including a local
server's key. Escrow keys separately for restore. See [guided setup](admin-settings.md).

## Choose the runner login and configure it

You may use your current non-root login or a dedicated unprivileged account with
its own home. A dedicated account helps separate personal files and credentials.
GitHub Actions jobs run with that account's filesystem/network access; this is
not Sluicework's script sandbox. Enable only trusted workflows. Do not route untrusted PRs or fork code
to a personal workstation, and never use generic `runs-on: self-hosted` jobs in
the pilot repository. Every local job must require its explicit machine label.

For a networked machine, use **Machines → Enroll machine** in the main admin.
Choose **Optional CI workstation**, its actual OS/CPU and the configured cluster
HTTPS URL. Download and extract the setup ZIP into a new private directory on
that machine. For another platform, first obtain the matching binary as described
in the setup instructions. Verify checksums, then run:

```sh
chmod 700 ./sluicework ./manage
python3 ./manage join
```

Compare the public CA fingerprint with the main admin, then enter its one-use
enrollment token at the hidden prompt. The helper generates private paths and
configuration, enrolls the worker and starts a paused controller. It installs no
server or separate worker admin. The same-Mac pilot above uses `manage setup`
instead of `join`.

Installation downloads the pinned official runner 2.338.2 for the current
Mac/Linux CPU and checks SHA256 before extracting it into a new private directory.
It requires network access and refuses to overwrite an existing installation.
Linux runner prerequisites must already be installed using GitHub's instructions.
Do not install the official runner's separate `svc.sh` service for this directory.

In **Machines → GitHub Actions runner**, select **Manage as an optional workstation
runner** and approve the repository. Add its actual platform labels and the pilot's
chosen machine label, for example
`self-hosted, macOS, ARM64, sluicework-example-mac-arm64`.
The broker also adds the stable `sluicework-<machine-ID-prefix>` label shown in the
console. Use that label to choose one particular machine. The recorded name is
descriptive; actual ephemeral GitHub names are derived from the machine ID.

## Offer and pause capacity

Each controller session starts paused. On its machine card in the main admin,
choose **Offer capacity** or **Pause capacity**. Local controls remain available
in another terminal under the same login:

```sh
sluicework workstation-enable --config workstation.json
sluicework workstation-status --config workstation.json
sluicework workstation-pause --config workstation.json
```

Enabling expires after two hours by default. Only one build can run per controller.
Defaults require 4 GiB available memory and one-minute CPU load below half of the
logical CPU count. macOS additionally requires AC power, no reported thermal
throttling, and 120 seconds without keyboard/mouse input when `minIdleSeconds`
is omitted. The examples here explicitly set it to `0` for use while you work;
set it to `120` for idle-only admission. Linux input-idle
detection is unavailable; its default is `minIdleSeconds: 0`, with load/memory gates.
Unavailable resource readings prevent accepting a new build.

Foreground activity (when an idle gate is enabled), resource pressure, expiry, local pause, or console **Drain**
withdraw the runner's custom labels. An already assigned build finishes; a start
hook rechecks approval and resources before workflow steps. A failed start gate is
a failed local build, not a cloud retry. macOS processes use background scheduling;
all builds inherit nice priority and conservative compiler thread settings. These
are resource preferences, not hard CPU/RAM caps or a guarantee of zero impact.

Ctrl-C drains before exit. An idle listener can take at least 75 seconds to stop,
allowing GitHub's assignment window to settle. Do not force-kill a running build.
After a crash, the next session withdraws the previous registration and waits for
an assigned job before recycling it. The server also withdraws offline/revoked
machines and cleans up idle leases. GitHub connectivity failures can delay drain;
the controller stays conservative and reports its state in the console and local
`workDir/workstation/status.json`. Private runner output is in `runner.log` and the
official runner's `_diag` directory.

## Explicit local/cloud routing

### Build activity in the admin

**Overview**, **Runs**, and each **Machines** runner card show GitHub builds
executed by enrolled Sluicework workstations, including status, current step,
machine, branch, start/finish times, attempt and a link to GitHub logs/artifacts.
The separate **Sluicework jobs / slots** counter counts Sluicework job attempts.
**Draining** withdraws new runner capacity while any assigned build continues;
the machine card explains this and shows the build separately.

To enable verified build metadata, grant the repository-scoped GitHub App
**Actions: read** and approve that permission on its installation. Runner
registration still uses **Administration: read/write**. Activity sync uses a
separate token limited to Actions read; unavailable history never blocks runner
control. No webhook or public callback is required.

The server polls about every 30 seconds. It discovers the latest 50 repository
workflow runs plus active runs (up to 500), and continues tracking known active
builds even when they fall out of discovery. Matching requires the exact enrolled
runner name and machine label; hosted builds are not counted as local. Verified
records survive restarts and runner recycling. The cache retains up to 1,000
completed jobs and the admin shows the latest 100 active/recent records. This is
recent local build history rather than a full import of the repository's history.
Build logs and artifact contents remain on GitHub.

Missing permissions or sync errors appear in the admin with the last successful
sync time. After 90 seconds without a successful sync, statuses are labeled
**last known** rather than presented as current. Permission failures back off for
five minutes; restarting the server also retries the connection.

### Select placement in GitHub

The pilot repository's **Sluicework ARM64 pilot** workflow offers `local-only`
(default), `prefer-local` (explicitly permits billed cloud fallback), and
`cloud-only`. Choose Mac ARM64 or Linux ARM64 and an explicit machine label.
The hosted coordinator consumes GitHub Actions minutes with every policy;
cloud fallback additionally consumes hosted build minutes.
Its hosted coordinator works while your workstation and Sluicework server are off.
It dispatches one child build, waits for a bounded local queue, confirms cancellation
without assignment, and then may dispatch GitHub-hosted Linux ARM64 work. An
assigned, failed, or ambiguously canceled build is never duplicated on another host.
Artifacts stay on the child run linked in the coordinator summary; the pilot does
not deploy or request AWS credentials. Both Lambda packages remain Linux ARM64.

The generic coordinator and race tests live in
[`integrations/github-actions`](../integrations/github-actions). Canceling a
coordinator normally cancels its child in cleanup; after a hard hosted-runner
failure, inspect the linked child run and cancel it manually if necessary.

[GitHub ephemeral runners](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)
· [Runner REST API](https://docs.github.com/en/rest/actions/self-hosted-runners)
