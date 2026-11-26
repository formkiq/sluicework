# Manage GitHub Actions runners

GitHub workflow files define jobs, trust rules, permissions and deployment.
Sluicework's **Actions runners** page manages runner registrations and the
published placement policy of compatible workflows. It uses the configured
repository's GitHub App; no personal token is copied to a runner.

## Enable the required App permissions

Keep **Administration: read/write**, **Metadata: read** and **Actions: read**.
For repository placement, also grant **Contents: read** and **Variables: write**.
Approve the updated permissions on the App installation in GitHub.

For an organization runner such as a self-hosted Linux machine, grant the App organization
**Self-hosted runners: read/write**, approve that installation, then enable
**Settings → GitHub App connection → Manage organization GitHub runners** and
save. This is an explicit opt-in: organization label changes and removals can
affect other repositories. Missing optional permissions are reported separately
and do not disable the existing workstation broker.

## Discover and select an existing external runner

Open **Actions runners** and refresh. Repository and organization runners display
their actual GitHub name, OS, labels, online/offline status and busy state.
Organization cards also show their runner group and whether that group allows
the configured repository. An organization runner need not appear in the
repository-only REST listing; organization discovery checks the group API.

Use **Edit labels** to give an external runner a unique custom label such as
`linux-runner-01`. GitHub's default labels are read-only. For a particular Linux x86-64
host, the complete selector is:

```yaml
jobs:
  test-linux-runner-01:
    runs-on: [self-hosted, Linux, X64, linux-runner-01]
    steps:
      - run: hostname
```

All labels must match. `[self-hosted, linux]` alone can select another Linux
runner. The admin excludes ambiguous labels, groups without repository access,
and workflow-restricted groups from automatic placement targets. Review group
access/restrictions in GitHub. Sluicework-owned runner registrations are edited
through **Machines**, preserving their controller's approval and pause behavior.

## Publish placement for supported workflows

The repository must include the reviewed version 1 or 2 `.github/sluicework.json`
integration manifest and a workflow that consumes `vars.SLUICEWORK_ROUTING`.
The generic companion integration supports Mac ARM64, Linux ARM64 and
Linux x86-64 build hosts. Each consumer controls its build artifacts and targets.

In **Repository build placement**, select the preferred runner and, for version 2
workflows, an optional **Second runner**. The displayed order is deterministic:
for example, Linux runner → Personal Mac CI → GitHub-hosted ARM64. Choose whether
GitHub-hosted fallback is enabled and set the queue timeout, which applies
separately to each selected runner. A later runner is tried only after GitHub
confirms the earlier queued job was cancelled without assignment. Assigned or
failed builds are never retried on another host. **Save placement to GitHub** publishes one versioned JSON repository
variable, so the selection does not depend on the local admin staying online.
An already dispatched build retains its inputs. Existing PR trust gates,
source/artifact verification and deployment permissions remain workflow code.
Other workflows retain their own `runs-on`; use the displayed workflow example
and a normal pull request to connect additional jobs.

Existing workflow defaults and legacy placement variables remain active until
the first publication. Explicit manual inputs can override a run. Refresh before
editing if someone has changed the policy in GitHub; stale edits are rejected.
An invalid manually edited variable is reported instead of silently substituting
another machine. Correct or remove it in GitHub's repository Actions variables.

## Register or remove external runners

**Add external runner** prepares repository- or organization-scoped GitHub
registration for Mac/Linux ARM64 or x86-64. Choose its actual platform, name and
unique label. The admin returns GitHub's official download URL and SHA-256 plus
a short-lived registration token. The commands stop on download/checksum errors
and prompt for the token instead of putting it in shell history. Run them under
the intended non-root host account in a new directory after installing GitHub's
OS prerequisites. The token is masked, not persisted in SQLite or audit records,
and expires at the displayed time. Refresh the inventory after registration.

External runners do not need a Sluicework agent, administrator, or workstation
configuration. Their host operator controls service startup, concurrency and
CPU/memory limits. Organization group access still governs which jobs they can
receive. GitHub retains job logs and results.

**Remove registration** requires the exact runner name and an idle, offline runner.
Stop its service on the host first; the server rechecks busy/online state before
deletion. Organization removal affects
every repository using that runner. Update placement and workflow selectors
first; uninstall the service on the host separately when appropriate. Removal does not stop
host processes. Inherited enterprise runners are managed by their owner.

All management actions require a Sluicework administrator and are audited.
Registration/removal never grants a runner access to another repository by
changing an organization runner group's policy automatically.


## Share a managed workstation between repositories

In **Settings → GitHub App connection**, enable organization runner management,
then open **Share workstation runners across repositories**. Select a dedicated
organization runner group and enter the exact private `owner/repository` list,
including the primary repository. Install the App on each repository. The group
must have **Selected repositories** access matching this list, allow no public
repositories, and have no workflow restrictions or enterprise inheritance.
Create the group in GitHub using the provided link, then refresh the group list.
Drain existing workstation registrations before changing its group or scope.

The controller retains one listener and one build at a time. Its pause, expiry,
CPU/memory admission rules and build-thread preferences apply across all listed
repositories. GitHub chooses jobs that match that machine's explicit labels;
Sluicework validates the group's exact access before issuing each JIT registration.
The primary repository remains the worker's control-plane approval scope.

Use the **Repository** selector on **Actions runners** to manage each repository's
placement and inspect runner access. Only approved repositories are selectable;
each published policy has its own revision and repository-scoped token. Machine
build history includes verified jobs from all configured repositories. GitHub
workflow files still control which PRs may execute on self-hosted capacity.
