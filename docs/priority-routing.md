# Repository priority routing

The queue has two default classes. Existing running builds finish normally.
Higher-priority requests precede lower-priority requests waiting for the primary pool;
requests within a class use their submission order.

| Class | Repositories | Primary Linux deadline | Next targets |
| --- | --- | --- | --- |
| High | `example-org/application` | 15 minutes | Available Mac, then Spot, then hosted |
| Low | `example-org/documentation` | 60 minutes | Available Mac, then Spot, then hosted |

A Mac is considered only when GitHub reports available capacity. An unassigned
Mac attempt has a separate short queue window, normally two minutes. Once a
worker starts, its execution time does not consume a queue deadline. Test/code
failures are never retried on another machine. The optional Spot tier below can
recover from a positively verified AWS interruption.

## Two deployment modes, one scheduler

Workstation installations use an AWS Lambda dispatcher, a DynamoDB queue and a
one-minute EventBridge clock. The Mac can sleep or disconnect without stopping
queue decisions. An always-on Sluicework installation can run the same Go
scheduler with its local SQLite store instead. Each deployment has a distinct
backend ID; only the backend selected in repository policy processes requests.
Do not activate two different stores for the same backend ID.

Workflows upload a small immutable request artifact and exit. The dispatcher
polls explicitly allowed repositories, authenticates the parent run and artifact,
and records accepted requests durably. No hosted Actions executor sleeps through
a fifteen- or sixty-minute wait. Clock polling means transitions can occur up to
one tick after their deadline.

The dispatcher holds requests in its own queue until a target can accept work.
It reserves targets before dispatching and persists dispatch intent before each
GitHub API POST. A retry discovers the exact existing worker instead of issuing
another POST. Cancellation must prove an attempt was unassigned; an assignment
race fails closed rather than starting a duplicate build.

## Trust and completion

The GitHub App key remains in Secrets Manager in AWS mode, or a protected local
file in server mode. It is never exposed to build jobs. App installation tokens
are limited to the approved repositories and required queue permissions.

Parent artifacts bind repository, source, workflow revision, parent attempt and
required-check ID. The App publishes its selected worker in an App-owned metadata
check. Default-branch completion code authenticates both records and verifies the
worker, immutable artifact digest and bundle hashes. Only a current default-branch
source can deploy through the existing hosted AWS OIDC job. PRs cannot deploy.

The dispatcher needs Actions write, Checks write and Pull requests read in
addition to existing content/runner visibility. Prepare the infrastructure and
repository workflows before activating these grants and the queue policies.

## Admin controls

After installing a version 3 workflow integration, open **Actions runners →
Repository build placement**. Choose **High** (fifteen minutes) or **Low** (sixty
minutes), the preferred runner, the second runner, and its separate one-to-five
minute assignment window. Save publishes the protected `SLUICEWORK_ROUTING`
repository variable. Accepted requests keep their policy snapshot; edits affect
new requests. Previously saved five-minute high policies remain readable during
upgrades; saving High publishes fifteen minutes. Legacy version 1/2 policies retain their fifteen-minute maximum
and cannot accidentally start a sixty-minute hosted coordinator.

The included `infrastructure/priority-queue/example.json` is a synthetic two-repository
allowlist. Replace every App/installation/repository ID, repository name, heartbeat
commit, backend ID, start time and machine label before deploying. Use real IDs
from the GitHub API and a fixed, reviewed commit for each heartbeat workflow.
The App installation and restricted Mac runner group must include exactly the
repositories in your configuration. The service accepts requests only when their
reviewed integration selects the configured App/backend.

## Workstation deployment

Requirements: Go 1.26.11 or a newer fully patched Go release, Python 3, AWS CLI v2
and an authorized AWS profile. The build uses the reviewed compiler in `go.mod` by
default. Build:

```sh
scripts/build-queue-lambda.sh /private/tmp/sluicework-queue-release
```

The ZIP is Linux ARM64 and contains an executable `bootstrap`. No build command
contacts AWS or uploads keys. The runtime uses `provided.al2023` with a 55-second
timeout, reserved concurrency one, a 256 MiB allocation and a one-minute clock.
Templates have no public URL, API Gateway or inbound workstation connection.

1. Review and deploy `infrastructure/priority-queue/bootstrap.json` to a dedicated
   test stack. It creates a private, encrypted, versioned code bucket and an empty
   Secrets Manager secret. Both are retained on stack deletion.
2. Approve storing the existing GitHub App PEM in that secret. Use
   `aws secretsmanager put-secret-value --secret-id ARN --secret-string file://ABSOLUTE_PRIVATE_KEY_FILE`
   with an explicit profile/region. Never paste the key into CloudFormation,
   workflow variables, logs or a shell argument. The runtime refreshes its key
   every fifteen minutes; revoke an old key only after the new one is loaded.
3. Upload `dispatcher.zip` to the private code bucket. Record the S3 object
   version. Prepare a change set using `service.json`, with `CodeBucket`,
   `CodeKey`, `CodeVersion`, `AppKeySecretArn`, compact `QueueConfig` JSON and
   `Enabled=false`. Inspect its IAM policy before executing it.
4. Grant the App **Actions write**, **Checks write**, **Pull requests read**,
   **Contents read**, **Metadata read**, **Variables read** and runner visibility.
   Existing runner-management grants remain independent. Queue tokens request
   only read administration and runner permissions, never their write grants.
5. Merge the repository integrations, invoke the Lambda once and check its logs.
   Enable the EventBridge clock and wait for a fresh App-owned heartbeat.
   Let all in-flight legacy coordinators, workers and completion jobs finish
   before upgrading the policies, then publish each repository's `.github/sluicework-priority.json` through admin.
   Until publication, existing repos retain their routing and new repos use
   hosted validation. Activating the variable is the final rollout step.

DynamoDB uses on-demand capacity, point-in-time recovery and a seven-day TTL for
queue records; logs retain fourteen days. A CloudWatch error alarm is included;
connect its actions to the organization's existing incident channel separately.
Storage, Secrets Manager, EventBridge and Lambda have AWS charges. Hosted
submission and verification jobs still consume short Actions jobs. No hosted
executor waits through the queue deadline or the self-hosted build.

## Always-on server deployment

Use a dedicated backend ID and protected absolute state/key paths in a copy of
the example configuration:

```sh
sluicework github-queue --config /secure/sluicework/github-queue.json
```

Set `stateFile` and `privateKeyFile`, restrict the key and database to mode 600,
and supervise the command alongside the normal server. It runs the same engine
with SQLite and a thirty-second polling clock. It requires no AWS account. The
service must stay awake and online. Do not run it on an intermittently available
workstation. When changing backend/store, stop submissions, drain existing work,
and install the new backend binding before publishing its policies; never run
independent stores with the same backend ID.

## Operations and validation

`Sluicework queue` on the source commit shows the accepted class, state and exact
selected worker. A `blocked` state preserves the reservation when a dispatch or
cancellation cannot be proven; investigate the exact worker before any manual
retry. The dispatcher never blindly repeats an uncertain POST. A moved source or
superseded parent attempt is rejected before dispatch. High priority applies to
unstarted work routed through this dispatcher; unrelated direct GitHub jobs are
outside its queue. Sustained high-priority work can delay low-priority work until
its fallback deadline, but never interrupts a running build.

Validate locally with `go test ./...`, `go test -race ./internal/githubqueue ./internal/githubapp`, `go vet ./...`, `node --test integrations/github-actions/priority/*.test.mjs` and `npm ci --prefix web && npm run build --prefix web`. Run `cfn-lint` on both infrastructure templates and
`actionlint` on repository workflows. The shared `queue.mjs` and its tests under
`integrations/github-actions/priority/` are vendored verbatim into each consuming
repository; the other files provide build-only repository adapters. Preserve the authenticated request/receipt contract when updating.


## Public source validation

This public repository runs native Linux integration tests on disposable hosted
x86-64 and ARM64 VMs. Its workflows have no organization-specific App binding,
dispatcher, deployment credentials or self-hosted runner selectors. Linux-only
root/systemd tests must run on disposable machines, not a shared workstation.

Build macOS installers locally with `bash packaging/build-release.sh release darwin/arm64`
or `darwin/amd64`. Run the native Go and installer tests on a trusted Mac;
cross-compilation on Linux cannot validate Darwin execution. This repository
does not schedule GitHub-hosted macOS builds. Organizations can install the
generic integration under `integrations/github-actions/` in their own private
repositories after configuring trust, runner groups and deployment approvals.

## Optional AWS Spot runners

The optional fleet adds a cost-controlled stage after Linux runner and an available
Mac, before GitHub-hosted **Linux**. It uses the existing v3 worker contract;
consumer repositories need no new workflow version. Local-only placement never
starts an AWS machine. There is no hosted macOS fallback.

Each allocation starts one ARM64 VM for one source-bound build. The **global hard
limit is three**, including starting, uncertain and terminating allocations.
A full fleet waits five minutes by default before hosted fallback. Accepted
requests snapshot the profile and deadlines. Disabling admissions prevents new
VMs; existing builds finish within their original lifetime. Lowering the cap does
not interrupt running builds. A verified AWS Spot interruption may fall back only
after the original GitHub worker has stopped; a failing test is not retried.

| Profile | Machine | Resources | Compute bid ceiling |
| --- | --- | --- | --- |
| Small | `c6gd.medium` | 1 CPU, 2 GiB | $0.025/hour |
| Standard | `c8gn.large` | 2 CPUs, 4 GiB | $0.06/hour |

These are ceilings, not price quotes. AWS storage, public IPv4, transfer and the
short GitHub submission/completion jobs are additional. Start with Standard and
benchmark the repository: 4 GiB may be insufficient for large browser or Rust
builds. No instance runs merely because Spot is enabled.

### Provision once

1. Use the **same AWS account, region and backend ID** as the priority dispatcher.
   Workstation mode uses Lambda; an always-on server can use the same fleet with
   its normal AWS credential provider and a private SQLite state file. Run only
   one dispatcher for a backend; never give independent stores the same backend.
2. Create a dedicated organization runner group, **Sluicework Spot**, private and
   restricted to exactly the dispatcher's approved repositories. Grant the App
   organization Self-hosted runners write. Do not reuse an unrestricted group.
3. Pin a Canonical Ubuntu 24.04 ARM64 EBS AMI (owner `099720109477`, root
   `/dev/sda1`), or a tested account-owned equivalent. Resolve the Canonical image
   through `/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id`
   in the deployment region. Confirm that both instance types are available and
   the All Standard Spot vCPU quota allows at least six vCPUs for three Standard
   runners.
4. Review/deploy `infrastructure/priority-queue/spot.json` with the dispatcher's
   existing role name, backend and pinned image. Select `NetworkMode` explicitly:
   `dedicated` (recommended for new stacks) creates a VPC that hosts nothing but runners, with
   two public subnets, an outbound-only HTTP/HTTPS security group and scoped EC2
   permissions, **no VMs**. Never peer that VPC or attach it to a Transit Gateway.
   `NetworkMode=existing` reuses `VpcId` and one to three **public** subnets in
   it instead; build code then runs as a non-root user but can reach any host in
   that VPC, or a peered network, on TCP 80/443. Set `CreateSpotServiceRole=true`
   only if `AWSServiceRoleForEC2Spot` does not already exist. No SSH, instance IAM
   role or NAT gateway is needed.

   There is no default mode, so a routine update cannot implicitly replace the
   running fleet’s network. Upgrading a stack deployed before `NetworkMode` existed: pass
   `NetworkMode=existing` with the same `VpcId` and subnets to keep its resources.
   Switching to `dedicated` replaces the security group and subnets, so first
   disable admissions and confirm zero reservations, then update `QUEUE_CONFIG`
   with the new outputs.

   Before every launch the dispatcher refuses to start a runner unless its
   security group has no inbound rules, allows only outbound TCP 80/443 to
   address ranges, and is in the same VPC as the chosen subnet.
5. Add the resulting immutable infrastructure to the existing `QUEUE_CONFIG`
   (or the server's queue configuration), preserving `since` and all repositories:

```json
"spot": {
  "accountId": "123456789012", "region": "us-east-2",
  "rootDeviceName": "/dev/sda1", "imageId": "ami-REPLACE",
  "subnetIds": ["subnet-REPLACE"], "securityGroupId": "sg-REPLACE",
  "runnerGroupId": 4, "controlRepository": "example-org/application",
  "volumeGiB": 32
}
```

6. Deploy the reviewed dispatcher build. In **Actions runners → AWS Spot runners**,
   verify its fresh App-owned heartbeat, select repository profiles, enable Spot,
   and set a maximum of 1–3. Operational settings live in the primary repository's
   `SLUICEWORK_SPOT` variable and remain available while this Mac sleeps. The admin
   manages all operational settings; immutable AWS networking/IAM remains a
   reviewed infrastructure deployment. Settings changes apply to future requests.

Machines have encrypted disposable gp3 roots, IMDSv2, no reusable AWS/App
credentials, a checksummed official runner, and an independent shutdown deadline
(default 90 minutes, setup included). Workflow steps run as the unprivileged
`runner` account with no supplementary groups, inherited `no_new_privs` and
no access to instance metadata. A pre-existing sudoers grant on a custom image
cannot elevate workflow commands. Custom images must be trusted, minimal Ubuntu
images without privileged services exposed to the runner (including Docker
sockets). A workflow that needs extra packages must use a custom image, not `sudo apt-get`. A job-start hook checks repository,
workflow, source, request and runner labels before workflow steps. The dispatcher
reaps expired/orphaned VMs even when GitHub discovery fails. Uncertain launches
retain their slot and immutable EC2 client token/subnet; never erase fleet state
to recover capacity. Inspect AWS and reconcile the original allocation first.

The fleet's one-job JIT grant is private state: exclude its DynamoDB/SQLite body,
EC2 user data and cloud-init logs from screenshots and support exports. The admin
and check receipts show only public allocation status. Delete a backend's
infrastructure only after disabling admissions and verifying zero remaining
reservations and tagged EC2 instances.

The Spot `region` may differ from the dispatcher region. Deploy `spot.json` in
the **runner region**, attaching its scoped policy to the existing dispatcher
role. DynamoDB and Secrets Manager remain in the dispatcher region. Choose regions explicitly for your deployment and verify all resource ARNs before activation.

For an isolated Linux fallback check, use a reviewed consumer workflow that
exposes placement `cloud-only`. This skips local tiers for that run and tries the
configured Spot profile before hosted Linux. Mac-only jobs must not use this path.

When moving an existing dispatcher, create the replacement with `Enabled=false`.
Stop the old clock and wait for its last invocation to finish. Copy the complete
queue with consistent reads (including accepted requests and receipts), preserving
the backend ID, discovery cursor and repository policies. Verify the copy before
starting only the replacement clock. Retain a private rollback snapshot until
completion checks and runner cleanup have been verified. Never run both clocks.

## Architecture-independent Linux pool

The version 3 primary selector `{"platform":"linux","machineLabel":"self-hosted"}`
uses the existing `Default` runner group and requires only `[self-hosted, Linux]`. It includes available repository-authorized
Linux runners such as `linux-runner-01` and `linux-runner-02` without a custom tag or architecture
label. Offline and busy runners do not count as free capacity. Runner-group
repository permissions still apply, and workflow-restricted groups are not offered
by admin as pool capacity. Build tools use the host's actual architecture;
architecture-specific output targets (such as ARM64 Lambdas) remain explicit. The group boundary excludes reserved Spot registrations and opt-in managed workstation groups, preserving their later-tier admission and pause controls.

Select **Any self-hosted Linux runner** in **Actions runners → Repository build
placement** after upgrading each repository integration and the dispatcher.
The selected Mac, priority class, 15/60-minute windows, short Mac queue timeout,
Spot and hosted fallback remain unchanged. Existing accepted requests retain their
old selectors; activate the new policy only after the supporting code is deployed.
No runner labels, group permissions or credentials need changing.

Confirmed Spot termination receipts remain valid after EC2 ages terminated instances out of its inventory. Cleanup still waits for busy runner registrations and never infers termination from a missing instance. GitHub rejection logs expose only categorized errors and numeric quota/backoff headers.

## GitHub API polling budget

The workstation Lambda processes one clock minute per warm runtime and ignores scheduled events more than two minutes old. Replayed clock events carry no jobs; a fresh clock discovers durable request artifacts and advances saved queue records. Existing durable reservations remain the execution authority across cold starts.

A primary rate-limit response pauses GitHub polling until the provider's reset time. Secondary limits honor `Retry-After`, with a one-minute fallback. Permission and organization IP-policy denials remain errors. This backoff returns successfully to Lambda so asynchronous error retries cannot multiply GitHub calls while quota is exhausted.

Registered coordinator and worker workflow IDs are cached for five minutes, scoped to each repository and filename. Authenticated heartbeat lookups are cached only until their existing five-minute refresh deadline. Repository manifests and protected placement policy are still checked on the next poll. Admin build-history synchronization runs once per minute; runner heartbeat and admission checks retain their existing cadence.
