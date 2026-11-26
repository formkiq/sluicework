# Install and configure local AI models

As a cluster administrator, open **AI connections → Set up local AI** to choose and install models. The macOS installer can prepare the runtime first; an existing Ollama or Herd endpoint also works.

## Install the runtime

Linux hosts use the optional `install.sh inference` flow with systemd services. See [Linux setup](linux.md#optional-ollama-and-herd). macOS hosts use the flow below. Model management works through the same endpoints on either OS. The memory presets were sized for Macs; on Linux also account for GPU VRAM and the selected inference backend.

After **Create a cluster** or **Join a cluster**, the installer offers optional runtime setup. On an existing installation, rerun the current installer and choose **6. Configure local AI runtime (Ollama / Herd / Open WebUI)**. Upgrade an older Sluicework executable first if it does not provide `runtime-check` or `open-webui-check`.

- **Use an existing Ollama or Herd endpoint:** check its API and version, then register it on the orchestrator. Existing model permissions, resource limits and disabled state are preserved. A conflicting connection name is never overwritten. Remote endpoints require verified HTTPS.
- **Install and start Ollama:** on macOS 14+, download the pinned official app, verify its SHA-256, developer signature and Gatekeeper assessment, and publish it to `/Applications/Ollama.app`. Launch it as the active desktop user. Existing applications, CLI installations, model files and CLI links are preserved. Complete any macOS first-launch prompts yourself. The app controls its normal updates and login behavior.
- **Configure later or use another Mac:** continue without installing or starting a runtime.
- **Install optional Herd routing:** choose a local router plus node, a node connected to an existing HTTPS router, or reuse an existing Herd endpoint. See below.
- **Install or connect Open WebUI:** install a managed Open WebUI **0.13.4** chat interface (`127.0.0.1:8080`) connected to your local Ollama or Herd runtime, or register an existing Open WebUI endpoint. See [Optional Open WebUI](#optional-open-webui) below.

On the orchestrator, successful setup creates a **Local AI** or **Herd routing** connection with the Lightweight preset permitted and a 4096-token context. It does not download model files. Open that connection's **Manage models** to install the desired preset. A failed optional setup leaves Sluicework available and can be retried. On a worker-only Mac, the installer prints the endpoint; configure verified HTTPS access from the orchestrator before adding it there.

The installer pins Ollama **0.40.16** from its [official release](https://github.com/ollama/ollama/releases/tag/v0.40.16). Current Ollama requires [macOS 14+](https://docs.ollama.com/macos.md); Sluicework itself can still run on macOS 12–13 using an existing or remote inference runtime. New app installation and automatic startup require an active desktop login. This flow does not install a system Ollama daemon or provide inference before login.

### Optional Herd

[Ollama Herd](https://ollamaherd.com/download) is a separate routing layer that still requires Ollama on inference hosts. Sluicework pins Herd **0.12.9** and uv **0.13.9**. A managed Python **3.12** runtime and the locked dependencies are downloaded into `~/Library/Application Support/Sluicework/inference`. No system Python packages, Homebrew installation or shell profile are changed. Existing Herd installations and startup configuration are preserved; manage them with their original installer.

The local router listens on `127.0.0.1:11435`. Its node explicitly uses that address rather than mDNS. A node-only installation takes an existing HTTPS router URL and, when needed, an explicitly trusted PEM CA. The installer shows the certificate fingerprint for out-of-band verification. The orchestrator must separately trust that CA in its System keychain before using the router.

**Herd's current node agent automatically opens a LAN proxy for Ollama on TCP 11434.** This exposes inference and model-management operations without authentication. The installer requires a separate affirmative choice before installing and starting Herd agents. Proceed only on an appropriately trusted network. A loopback-only router does not make this node hop encrypted or authenticated. To accept other Macs at a new router, explicitly configure an HTTPS entry point and the required network protections; this installer does not expose its router to the LAN.

Managed services use `com.sluicework.herd` and `com.sluicework.herd-node` LaunchAgents under the desktop user's `~/Library/LaunchAgents`. They start immediately and at that user's login, not before login. If startup fails, newly created agents are rolled back. Re-running setup preserves existing agent configuration. Standard launchd output is under the managed state's `logs/`; Herd may also write its normal `~/.fleet-manager/logs/`. The managed agents use an isolated environment file and data directories. Telemetry, automatic model pulls, model preloading, fleet-intelligence inference and model substitution are disabled. Sluicework's requested context and output-token settings are passed through.

Inspect services as the desktop user with `launchctl print gui/$(id -u)/com.sluicework.herd` and its `herd-node` equivalent. To stop them and prevent next-login startup, boot out each label, then move its plist out of `~/Library/LaunchAgents`. Preserve the model files owned by Ollama. Keep the managed environment directories when investigating a failure; model downloads and user-agent logs are not included in the orchestrator's application backup.

### Optional Open WebUI

[Open WebUI](https://docs.openwebui.com/) provides a local chat interface on top of your Ollama or Herd runtime. Sluicework pins Open WebUI **0.13.4**, using upstream’s **slim external-inference profile**, in an isolated managed Python **3.12** environment using the same pinned `uv` (`0.13.9`) installer as Herd (`~/Library/Application Support/Sluicework/open-webui` on macOS, `/var/lib/sluicework/inference/open-webui-0.13.4-<hash>` on Linux). Existing Open WebUI installations and service definitions are preserved.

- **Supported managed hosts:** Apple Silicon Macs and Linux ARM64/x86-64. Intel Macs can connect an existing WebUI; patched cryptography has no Intel macOS wheel, so the optional installer does not attempt an unreviewed source build.
- **Scope:** Local Ollama/Herd chat, authentication and file storage are included. Local PDF/Office extraction, embedded vector search, Whisper and local embedding/reranking models are not installed. RAG requires separately configured external services; use an existing full WebUI installation if you need its local processing features. Slim mode and offline model-download defaults are explicit. The [dependency review and maintenance guide](open-webui-installer-review.md) records the patched pins, audit commands and backup/rollback process.
- **Binding and security:** The managed service listens on `127.0.0.1:8080` with `WEBUI_AUTH=true` (first-time visit creates the local admin account), `ENABLE_OLLAMA_API=true`, `ENABLE_OPENAI_API=false`, telemetry disabled (`ANONYMIZED_TELEMETRY=false`, `DO_NOT_TRACK=true`, `SCARF_NO_ANALYTICS=true`), and a per-install `0600` `WEBUI_SECRET_KEY`. For LAN access, put Open WebUI behind a verified HTTPS reverse proxy at root `/` (Open WebUI does not recommend subpath hosting) and configure that HTTPS URL in **Settings → Open WebUI**.
- **Ollama and Qwen model sharing:** Open WebUI connects to `http://127.0.0.1:11435` if local Herd is running, otherwise `http://127.0.0.1:11434`. Any Qwen or Gemma models installed via **AI connections → Manage models** (`qwen3.5:4b`, `qwen3.5:9b`, `qwen3.6:27b-coding`, `gemma4:12b`) appear automatically in Open WebUI.
- **Shared CI memory note:** Sluicework's per-connection `keep_alive_seconds` (0s for shared CI Macs) and `context_length` apply only to Sluicework job requests, not interactive Open WebUI chats. On a shared CI Mac, adjust keep-alive inside Open WebUI if you want interactive sessions to release model memory immediately.
- **Console integration:** Registered Open WebUI endpoints appear on **AI connections** with live `/health` status, the connected Ollama backend URL, and a direct launch link. Administrators can update or test the endpoint in **Settings → Open WebUI**.
- **macOS service inspection:** `launchctl print gui/$(id -u)/com.sluicework.open-webui`; logs live under `~/Library/Application Support/Sluicework/open-webui/logs/`. On Linux, inspect `sluicework-open-webui` with `systemctl` and `journalctl`.

## Choose and install a model

If configuring manually, start [Ollama](https://ollama.com/download/mac) on the inference Mac first. If using its CLI distribution, run `ollama serve`. In the console, choose **Set up local AI** or open an existing connection's **Manage models**.

1. Name the connection and enter its endpoint. `http://127.0.0.1:11434` means the Mac running the Sluicework **server**, regardless of which Mac hosts your browser or worker. For another Mac, use verified HTTPS as described in [macOS setup](macos.md#connect-local-inference-over-the-lan). A router must support `/api/tags` and `/api/pull` to manage models.
2. Select the inference Mac's memory and whether it shares resources with CI. These are planning inputs, not detected hardware. Check free disk space on that Mac. Download sizes below are approximate model files; runtime memory also includes context and concurrent requests.
3. Choose a preset and context length. Start at **4096 tokens**. Shared setup uses **0 seconds keep-alive** to release model memory after each request; dedicated setup keeps it warm for **300 seconds**. The connection's **Edit** dialog can change both settings. Curated models start with thinking disabled to produce direct answers within a small output-token budget. Enable thinking in Edit for more involved tasks and increase their token budget; thinking text is not retained as the result. These settings apply to Sluicework inference requests, not global Ollama settings. Existing local connections default to releasing memory after each request after upgrading.
4. Choose **Create connection and install model**. Ollama downloads the selected model from its registry onto the endpoint Mac. Installing model files does not run inference. Closing the dialog leaves downloads running. Reopen **Manage models** to follow per-layer progress, cancel or retry. Retrying uses any partial layers retained by Ollama.
5. After success, use the permitted model in an approved **Jobs** AI template. Run a small test report before scheduling it. Adding a model does not change existing job templates.

| Option | Model | Approximate download | Guidance |
| --- | --- | --- | --- |
| Lightweight | `qwen3.5:4b` | 3.3–4.0 GB | Suggested for a 16 GB Mac sharing with CI; summaries, classification and simple extraction. |
| General purpose | `qwen3.5:9b` | 6.6–7.6 GB | Suggested at 24 GB and up; scheduled tasks, more involved extraction and light coding help. |
| General-purpose alternative | `gemma4:12b` | 7.7–8.0 GB | Optional. Compare against Qwen 9B with representative document tasks before switching. |
| Coding | `qwen3.6:27b-coding` | 18–19 GB | Optional for a dedicated 32 GB inference Mac; plan for 48 GB when sharing with CI or using longer contexts. |
| Embeddings | `qwen3-embedding:0.6b` | 639 MB | Listed for future retrieval features; installation is deferred. Summaries and extraction do not need it. |

Model sizes and capabilities are linked from each preset: [Qwen 4B](https://ollama.com/library/qwen3.5:4b), [Qwen 9B](https://ollama.com/library/qwen3.5:9b), [Gemma](https://ollama.com/library/gemma4/tags), [Coding](https://ollama.com/library/qwen3.6/tags), and [Embeddings](https://ollama.com/library/qwen3-embedding/tags). Memory guidance is a starting budget, not a guarantee. Sluicework's current jobs accept text, including selected UTF-8 files; screenshot input and retrieval are not implemented.

## Downloads and recovery

Only administrators can inspect or install models. Downloads use the same private-address, verified TLS and no-redirect rules as local inference. The runtime needs registry internet access for installation; prompts and provider credentials are not sent with the pull request. Download history and settings are stored in SQLite and included in application backups. Model files remain owned by Ollama and need separate provisioning after a restore.

The server allows one download per endpoint and four across the cluster, with a two-hour timeout. Normal shutdown cancels active downloads; a crash leaves an interrupted record at next startup. Retry explicitly to resume. If settings change during installation, a downloaded model is not automatically permitted; verify the current connection and retry. Existing models are never deleted by this flow.

If a runtime is unavailable, start it and check the configured URL, trusted TLS certificate, free disk space and Ollama version. An HTTP router that only supports inference cannot install models. An Ollama registry error or unsupported model version leaves a failed record without exposing raw runtime diagnostics. The console refreshes inventory about every three seconds.
