# Connect a GitHub Actions runner

For GitHub-side discovery, labels, external registration/removal and compatible
workflow placement, use **Actions runners** in the main admin.
[GitHub Actions administration](github-actions-admin.md) includes organization
runners such as Linux runner and the additional App permissions they require.

For a workstation that offers spare capacity only when enabled, use the
[optional workstation controller](workstation.md) instead of installing an always-on runner service.

## Choose an OS and architecture explicitly

Mac and Linux machines may coexist in a cluster. Register the official runner for the host's actual OS/CPU. GitHub owns workflow dispatch; Sluicework records the service and status. A Sluicework job template's platform settings do not modify GitHub workflow YAML.

Specify all required labels in each workflow job. Add a unique custom label when a particular machine is required:

| Build host | GitHub `runs-on` |
| --- | --- |
| Linux x86-64 | `[self-hosted, linux, x64, build-linux-x64]` |
| Linux ARM64 | `[self-hosted, linux, ARM64, build-linux-arm64]` |
| Intel Mac | `[self-hosted, macOS, x64, build-mac-x64]` |
| Apple Silicon Mac | `[self-hosted, macOS, ARM64, build-mac-arm64]` |

Register the custom label on the intended runner. Every listed label must match; when that platform is unavailable, the job waits. Do not use only `self-hosted` or a shared team label for a platform-specific build. [GitHub runner selection](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/use-in-a-workflow).

For an explicitly selected build matrix:

```yaml
strategy:
  matrix:
    include:
      - labels: [self-hosted, linux, x64, build-linux-x64]
        target: linux/amd64
      - labels: [self-hosted, macOS, ARM64, build-mac-arm64]
        target: darwin/arm64
runs-on: ${{ matrix.labels }}
steps:
  # Check out the repository and provision Go/Node first.
  - run: bash packaging/build-release.sh release '${{ matrix.target }}'
```

Build targets and execution hosts are separate choices: Go can cross-compile the four binaries from one prepared host; native tests and platform-specific toolchains need the matching host. Sluicework's own `.github/workflows/platforms.yml` maps all four targets to explicit GitHub-hosted runner images.

## Linux runner service

In GitHub's **New self-hosted runner** screen select Linux and X64 or ARM64 to match the machine. Follow its verified download/configuration commands under a dedicated runner account, and register the desired unique label. Use the official `svc.sh` instructions to install/start its systemd service. Record the exact `actions.runner.*.service` unit and matching labels in Sluicework **Machines → GitHub Actions runner**. The worker reads its systemd state without elevated permissions. Inspect failures with `systemctl status <unit>` and the runner's GitHub page. Runner registration here does not register or relabel it on GitHub.

## macOS runner service

Choose a dedicated Mac for CI or isolate the runner in a separate environment. On a single-Mac pilot, use a dedicated non-administrator runner account and trusted workflow code. Jobs from forks and other untrusted sources belong on GitHub-hosted runners or separately isolated capacity. The runner account should have its own home, checkout, service, and credentials.

1. Sign in to the designated runner account on that Mac. In GitHub, open the repository or organisation's **Settings → Actions → Runners → New self-hosted runner**. Select macOS and ARM64.
2. Follow GitHub's current download, checksum, and configuration commands. Register against the intended scope. Choose a unique name such as `pilot-ci-01` and add a custom label such as `sluicework-pilot`. Keep GitHub's short-lived registration token in this setup session.
3. From the runner installation directory, run `./svc.sh install`, then `./svc.sh start` and `./svc.sh status`, as the runner user. The official macOS service generally runs as a user LaunchAgent. Check it after login and after reboot using the team's login/session policy.
4. Find that service's exact target. In the runner user's terminal, run `id -u` and list the `actions.runner.*.plist` files in `~/Library/LaunchAgents`. Read the correct plist's `Label` with `plutil -extract Label raw -o - /path/to/service.plist`. Verify `launchctl print gui/<UID>/<Label>`. For a separately managed system-domain runner, use `system/<Label>`.
5. In Sluicework **Machines**, edit the matching Mac's runner registration. Enter its GitHub name, owner or owner/repository scope, labels and exact launchd target. The worker reports the local service state on its heartbeat. An unavailable launchd login domain can report unloaded or unknown. Use the GitHub link to check runner availability and workflow history.

For the sample build, install Apple’s Xcode Command Line Tools on the runner Mac (`xcode-select --install`) and verify `xcrun --find clang` in the runner account. Use a manually dispatched workflow in a trusted pilot repository. Save this file as `.github/workflows/sluicework-pilot.yml` on the default branch, then choose **Actions → Sluicework pilot → Run workflow**. The custom runner label must match the one registered on the designated Mac.

```yaml
name: Sluicework pilot
on: workflow_dispatch
permissions: {}
jobs:
  verify-host:
    runs-on: [self-hosted, macOS, ARM64, sluicework-pilot]
    steps:
      - name: Show assigned host
        shell: bash
        run: |
          printf 'Runner %s\n' "$RUNNER_NAME"
          uname -m
      - name: Build and run a C program
        shell: bash
        run: |
          set -euo pipefail
          build_dir=$(mktemp -d "$RUNNER_TEMP/sluicework-ci.XXXXXX")
          trap 'rm -rf "$build_dir"' EXIT
          cat > "$build_dir/pilot.c" <<'C'
          #include <stdio.h>
          int main(void) {
              puts("Sluicework pilot build passed");
              return 0;
          }
          C
          xcrun clang -std=c17 -Wall -Wextra -Werror "$build_dir/pilot.c" -o "$build_dir/pilot"
          "$build_dir/pilot"
```

Match the runner name in the workflow log to the Sluicework machine registration and confirm the compiled program printed `Sluicework pilot build passed`. GitHub owns workflow results, logs, assignment and cancellation. CI capacity and Sluicework slots are configured independently. [GitHub runner reference](https://docs.github.com/en/actions/reference/runners/self-hosted-runners) · [GitHub secure use reference](https://docs.github.com/en/actions/reference/security/secure-use)

## Observe a runner in a separate login domain

If macOS prevents the worker account from inspecting the runner's launchd domain, install the bundled status reporter. It runs as the runner user and writes only the service target and state to a local file every 30 seconds.

As an administrator, replace `ci-user` with the dedicated local runner account and create its status directory. The group allows the Sluicework worker to read the published status.

```sh
sudo install -d -o ci-user -g _sluiceworker -m 2750 /Library/Sluicework/ci-status
```

In the runner user's logged-in terminal, replace the service label below with the exact `Label` from its official LaunchAgent.

```sh
/Library/Sluicework/bin/runner-status.sh install actions.runner.owner-repo.pilot-ci-01
```

The installer sets `runnerStatusFile` in `worker.json` to `/Library/Sluicework/ci-status/status.json`. The console registration must use the matching `gui/<UID>/<Label>`. The worker accepts recent status records for that exact target. After logout or an extended interruption, the status record expires.
