# Sluicework-controlled Codex clients

A coding workstation, Sluicework orchestrator, and inference host can be three
different machines. Codex reads files, edits, and runs commands **on the client**.
It sends model requests to Sluicework over authenticated HTTPS. Sluicework chooses
the configured Ollama/Herd connection and sends requests to that endpoint.
Neither Codex nor its tools receive the inference host URL or provider secrets.

## Set up the server in admin

1. In **Settings → Network access**, enable HTTPS with a stable hostname or private
   IP reachable from the coding workstation. Verify routing over your LAN or VPN.
   The URL is the Sluicework server, not the model host. An HTTP loopback origin is
   accepted only for explicit local development; remote clients require HTTPS.
2. In **AI connections**, create/test the Ollama or Herd connection and install
   the intended models on its host. A model host on another machine also needs
   verified HTTPS, as with other Sluicework AI connections. Existing system CA
   trust applies to that connection.
3. Open **Coding clients** on the same page. Enable the coding gateway and choose
   the shared local inference slot limit (1–4, default 1). Scheduled AI requests
   and coding requests share this admission limit. Direct Ollama and Open WebUI
   requests are outside this mechanism.
4. Authorize a named client, selecting its connection and permitted models. Enter
   the server's HTTPS origin explicitly. Set context, maximum output, timeout,
   concurrency and credential lifetime. Requests default to a 30-minute deadline
   (15–1,800 seconds). Existing clients can change their timeout in the Authorized
   clients table without downloading a new credential. Coding context defaults to 16,384 tokens,
   independent of the connection's scheduled-summary context. Keep-alive and
   thinking settings come from the selected connection.
5. Download the client file while the one-time receipt is visible. The server
   stores only its credential hash. Keep the file outside the project repository
   and transfer it privately to the coding workstation.

Changing connection settings or pausing/revoking access cancels affected active
coding requests. Disabling a connection or removing a permitted model also blocks
subsequent calls. Expired and revoked clients cannot use the gateway. These
credentials grant no console or worker access. Revoke and replace a client to
rotate its credential. Restoring a backup revokes all copied coding credentials.

## Run on any client Mac or Linux machine

Install Codex CLI on that machine and copy the matching Sluicework executable
there. If you copied a standalone executable, run `./sluicework install-cli`
and open a new terminal to expose the command on PATH. Codex CLI 0.171.0 is the tested client. Ollama, Open WebUI and a Sluicework
worker are not required on the coding workstation.

```sh
chmod 600 /private/path/sluicework-codex.json
sluicework codex --client /private/path/sluicework-codex.json --check
sluicework codex --client /private/path/sluicework-codex.json --cwd /path/to/project
```

For a private cluster CA, compare the SHA-256 fingerprint with the trusted admin
screen and add `--trust-ca THE_VERIFIED_FINGERPRINT` to both commands. The launcher
verifies both the CA and server hostname; it never disables certificate checks.
Publicly trusted HTTPS needs no additional CA flag.

For one noninteractive task:

```sh
sluicework codex --client /private/path/sluicework-codex.json \
  --cwd /path/to/project --exec -- "Fix the failing unit test and run it"
```

`--model` selects another model within the credential's allowlist. The default
is the first server-provided model. `--sandbox read-only` permits inspection;
`workspace-write` is the default for coding. The launcher does not weaken Codex
sandbox protections. It starts an independent Codex process, disables cloud web
search and connected apps for this local coding workflow, and uses an ephemeral
loopback relay on the **client** to verify remote HTTPS and keep the durable key
out of Codex's environment. Only its short-lived relay token reaches Codex. The
remote server address is always explicit in the private client file; it has no
localhost default. Local tools are governed by Codex's client-side sandbox and
approvals, not by a Sluicework worker lease.

## Give a coding session limited access

Start an interactive session with its project directory as the write boundary:

```sh
sluicework codex --client /private/path/sluicework-codex.json \
  --cwd /path/to/project --sandbox workspace-write
```

This is already the launcher's default: edits and routine commands run in the
project sandbox, with approvals on request for work outside its permissions.
Use `--sandbox read-only` for inspection. Filesystem reads follow Codex's read
policy; `workspace-write` is a write boundary, not a guarantee that all other
files are unreadable. The model host receives no direct filesystem access.
Review exceptions individually, such as network access to install dependencies;
do not grant full workstation access to fix a model or protocol error. See
[Codex sandbox and approval controls](https://learn.chatgpt.com/docs/sandboxing?surface=cli).

Prefer interactive mode when the task might need an approval: `--exec` uses
`never` approvals, so it cannot ask to expand access. Sluicework's admin controls
the model credential, inference limits and expiry; the client Codex process
enforces command, filesystem and network permissions.

## Headless server provisioning

An administrator with access to the orchestrator's configuration can provision
the same scoped client without a browser. Configure HTTPS first. This command
creates a new `0600` file and refuses to overwrite an existing path:

```sh
sluicework coding-client --config /path/to/server.json \
  --connection "Local AI" --models qwen3.5:4b --name "Developer laptop" \
  --server-url https://sluicework.example:8443 \
  --out /private/path/sluicework-codex.json --enable
```

## Protocol, limits and diagnostics

The authenticated endpoint is `/api/v1/harness/v1`. It exposes model discovery
and stateless Responses requests; it is not a general-purpose runtime proxy.
Only the selected local connection and current model allowlist can be used. Cloud
models and caller-supplied URLs, context options and keep-alive overrides are
rejected. Admission waits at most 30 seconds; excess demand returns HTTP 429.
Disconnecting or timing out a client cancels its model request and releases its
slot. Network failures are not automatically retried against another provider.

Responses messages, streamed events, function/custom calls and client-side tool
results are translated to native Ollama chat requests. Tool turns use constrained
JSON matching each tool's argument schema to avoid model-specific XML parser
failures on shell commands and patches. Text-only input is supported. Stateful
Responses IDs, hosted web tools, images and remote compaction are not supported.
Token and context limits remain server controlled. A larger model can improve
coding reliability; installing a model does not establish its coding quality.
Qwen 3.5 4B completed inference and invoked local Codex tools during testing, but
made command-quoting and editing mistakes. Treat it as an experimental coding
option and review its work; gateway compatibility is not a coding-quality guarantee.

Admin shows recent client/model, timing, status and token counts. The gateway does
not store prompts, file contents, tool results or response bodies. Codex itself
may keep its normal local session history (noninteractive `--exec` is ephemeral).
A failed or incomplete stream is never reported as a completed inference.

A **504 timeout** means inference did not complete within the coding client's
request deadline. The admin's recent request table shows the failure reason and
duration. For large files, ask Codex to inspect sections rather than sending the
entire file; reduce requested output or increase the existing client’s timeout in admin if
appropriate (maximum 30 minutes). Changes apply to subsequent requests.
After updating an older Sluicework launcher, restart its Codex session to pick up
the longer stream idle limit; the same private client file remains valid. A **502** instead indicates an upstream or response-protocol failure.
Neither error triggers cloud fallback.

An **unavailable tool during replay** error from older gateways is a protocol
compatibility bug, not evidence of missing tool permissions. Local compaction
can replay completed shell/edit calls with an empty current tool inventory.
Updated gateways retain that history without re-enabling those tools for new
calls. Upgrade the server to pick up the fix; a fresh session is only a temporary
workaround if a long task reaches compaction again. Keep the sandbox enabled.
