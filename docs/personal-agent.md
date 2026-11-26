# Personal scheduled tasks

Personal ChatGPT subscriptions run in a private agent under your normal macOS or
Linux login. The shared Sluicework server prepares proposals and receives signed
status reports. It never receives subscription tokens or sends execution commands.
Shared API-key connections continue to use the existing broker and scheduler.

## Setup

1. Install the Sluicework CLI on the machine that will run your personal tasks.
   Sign in to the shared console with your own cluster user. Open **AI connections
   → Personal scheduled tasks → Prepare personal agent setup**.
2. Run the generated `sluicework personal-agent enroll --server ... --owner ...
   --ticket ...` command in your own interactive terminal. Paste the enrollment
   code at its hidden prompt. It expires after ten minutes and is single-use.
3. For a private CA, verify its public certificate with the administrator and add
   `--ca /path/to/cluster-ca.pem`. Remote clusters require verified HTTPS.
4. Run `sluicework personal-agent start` without sudo. In another terminal, run
   `sluicework personal-agent login` and complete official ChatGPT sign-in in your
   local browser. Deployment/account eligibility still applies; this does not
   bypass provider approval, plan limits or workspace policy.
5. For background startup, stop the foreground agent with `personal-agent stop`,
   then run `sluicework personal-agent install-service`. This installs only your
   macOS LaunchAgent or Linux `systemd --user` service. It never installs a system
   daemon, changes login lingering, or configures automatic wake.

The default private directory is `~/.local/share/sluicework/personal-agent`.
Use `--dir /absolute/private/path` consistently to choose another runtime.
Enrolled owner/server cannot be reassigned remotely. The agent starts paused.
Do not put it in a shared service account or an account running untrusted builds.

## Prepare and approve a task

In the console, choose **Prepare scheduled task** on your personal runtime. Set
an exact text prompt, account model, five-field cron expression, IANA timezone,
approval expiry, daily run limit, deadline (15–1800 seconds) and saved-result limit. For example,
`0 6 * * *` in `America/Winnipeg` runs daily at 6am.

Each runtime allows up to 100 proposals with a combined 1 MiB definition budget.
Saving creates an inert proposal. Run `sluicework personal-agent approve PROPOSAL_ID`
in your own terminal. Review the entire prompt, model, schedule, account, limits
result-sharing choice, and optional browsing/notification permissions. Type `approve` to authorize background use. The agent
rechecks the proposal and account before committing consent. Then run
`sluicework personal-agent resume` to enable approved schedules.

A proposal edit on the shared server never changes an existing local approval.
The previously approved version remains in effect until you reapprove it or revoke
it locally. Expired approvals cannot run. One task runs at a time; each approval
permits 1–24 attempts per local calendar day. Failed/interrupted attempts count.
There are no automatic retries, burst catch-up runs or duplicate fall-back DST slots.
Sleeping/offline runtimes miss those slots; the host must be awake to run work.

## Local controls

| Command after `sluicework personal-agent` | Purpose |
| --- | --- |
| `status` | Read local status and synchronization errors. |
| `models` | Refresh the signed-in account's model catalog. |
| `preview ID` / `approve ID` | Inspect a proposal / grant exact local consent. |
| `pause` / `resume` | Persistently stop/start locally approved schedules. |
| `revoke ID` | Remove local approval and cancel that task if active. |
| `results` | Read recent private reports in your own terminal. |
| `sign-out` | Pause, cancel inference, clear tokens and attempt remote revocation. |
| `stop` | Pause and stop the current agent process. |

These controls use an owner-only Unix socket and kernel peer-UID verification on
both sides. The runtime directory is mode `0700`; credentials, identity key and
control socket are private to that UID. Root/setuid execution is refused. A file
lock prevents duplicate processes. No TCP administration listener is exposed.
Another cluster user or administrator has no approve/pause/resume/config/execute
API. OS root/admin privileges and other processes under your **same OS account**
remain outside this isolation boundary. Keep that account and machine trusted.
Shared workers or native CI runners executing as this same UID can exercise that
UID’s authority, regardless of their Sluicework role. Put shared CI/worker execution
in a separate OS account or VM before relying on strong personal-agent isolation.

The runtime binds its OS UID to the cluster user that requested enrollment.
Closing or signing out of the shared console does not revoke previously approved
background work; use local pause, revoke or sign-out. Optional login services
honor persisted pauses and do not install as another user.

## Privacy and provider behavior

Prompts are proposals stored on the shared server and visible to their owner.
Provider access/refresh tokens and signing keys stay in the private runtime.
Reports contain status only unless the exact local approval enables sharing output
with the shared server. Existing shared reports may remain on that server; sharing
is not end-to-end encryption against its OS/database administrator.

The agent signs each outbound status report. The server validates the runtime
identity, signature, timestamp and one-use nonce. It cannot forge local approval
or make the agent execute changed prompts by returning a different proposal.
The agent uses its own clock and immutable local approval, including during a
controller outage. Only its owner can read personal proposals/results through
console APIs; administrator roles do not override that ownership.

ChatGPT usage follows the [SIWC terms](https://openai.com/policies/sign-in-with-chatgpt-terms/),
including express consent for background work and user-controlled execution.
There is no automatic API-key fallback. Provider quota/authentication failures
pause the agent. SIWC does not support a per-request token cap: deadlines and
saved-result limits do not cap provider token usage. Review
[ChatGPT Usage](https://chatgpt.com/settings/usage) for allowance/credit settings.

## Web research

Enable **Allow public web research with source citations** in a task proposal,
then approve that exact revision locally. It is off by default, including for
existing approvals. Optionally restrict search results to up to 20 public domains
such as `openai.com, github.com`; restrictions include subdomains. An empty list
allows public websites. The agent sends the approved prompt to OpenAI; the
provider performs search and page reading. It never opens your signed-in browser,
uses its cookies, or fetches model-provided URLs on your local network.

This uses the Responses `web_search` tool, subject to the selected model and
account/workspace policy ([supported SIWC tools](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)).
If search is unavailable or does not complete, the task fails clearly with no
silent text-only or API-key fallback. The saved deadline still applies; no API
background mode or unlimited search budget is enabled. Search may consume plan
allowance; the result-size limit is not a provider usage cap.

The console displays source annotations as clickable inline citations. Citation
URLs stay private alongside the report unless output sharing is locally approved.
The local `personal-agent results` response includes source titles, URLs and
Unicode character ranges. Citations outside a truncated report are removed.
Retrieved pages cannot enable tools, change consent or choose notification content.

## Desktop notifications

Enable **Notify my runtime’s desktop** and approve that revision locally. The
private agent submits one generic status-only alert after a success or failure;
no task name, prompt, report, links or account details enter the alert.
Notifications appear on the computer running that agent, which may differ from
the computer viewing the console. macOS uses the system `osascript` notification
command; Linux needs `notify-send` and an active desktop/D-Bus session. Allow OS
notifications for the runtime/Script Editor if prompted. Headless/SSH sessions
may not support desktop delivery. No email or webhook is configured or sent.

Task history and the console record `submitted`, `failed`, or `unknown` delivery
status. Submitted means the desktop command succeeded; OS permissions, Focus/Do
Not Disturb and lock-screen settings determine actual display. Delivery failure
never reruns inference or changes a successful report to failed. Delivery attempts
are recorded before submission and are never automatically replayed; a crash can
leave an unknown outcome rather than creating duplicate alerts. Private report
content remains available through local `personal-agent results`.

## Existing ChatGPT tasks and automatic import

“Automatic import” would copy existing ChatGPT task prompts and schedules into
Sluicework proposals for your review. It would not copy your subscription or
activate tasks without local approval. It is not implemented: Sign in with
ChatGPT is not permission to read your ChatGPT conversations or scheduled tasks.
For now, copy a task's prompt and timing into a new proposal and approve it locally.
Once its replacement works, pause the original task yourself to avoid duplicate
runs. Sluicework does not modify or disable existing ChatGPT tasks.
