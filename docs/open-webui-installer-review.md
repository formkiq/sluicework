# Managed Open WebUI runtime: review and maintenance

## Profile and security updates

The optional installer uses the official Open WebUI **0.13.4** wheel with the
[upstream slim requirements](https://github.com/open-webui/open-webui/blob/v0.13.4/backend/requirements-slim.txt).
`uv.toml` records that dependency list and its source checksum explicitly. Both
services set upstream's `USE_SLIM_DOCKER=true` flag and `OFFLINE_MODE=true`.
Despite its name, that flag selects Python runtime behavior, including lazy
external vector clients; it does not require Docker. This native packaging of
the slim profile is maintained by Sluicework.

Included: Ollama/Herd chat, accounts, SQLite application data and file storage.
Excluded: local PDF/Office extraction, local embeddings/reranking, speech models
and embedded vector storage. RAG requires external services. The full profile's
ChromaDB, accelerate, transformers, sentence-transformers, anthropic, pyarrow and
pypdf packages are absent, rather than waived. Existing full installations remain
available through **Use an existing Open WebUI endpoint**.

These are deliberate deviations from upstream's exact pins, recorded in
`pyproject.toml`; do not remove them when copying upstream requirements:

| Dependency | Selected version |
| --- | --- |
| Authlib | 1.9.10 |
| PyJWT | 2.15.1 |
| cryptography | 50.0.2 |
| joserfc | 1.8.0 |
| black | 26.10.1 |
| OpenTelemetry API/SDK/exporters | 1.45.2 |
| OpenTelemetry instrumentation family | 0.66b2 |

Authlib's [JWS signature bypass](https://github.com/advisories/GHSA-6v38-4h7w-rj9h)
and OpenTelemetry's [API finding](https://github.com/advisories/GHSA-mf5v-w3w3-f6cg)
are among the fixes. Updating the OTel family together preserves its version
constraints. Authentication stays enabled, the secret persists with mode `0600`,
and the service binds only to `127.0.0.1:8080`. Linux keeps the signing key in a
root-only `/etc/sluicework/open-webui.env`, outside the public systemd unit. No
remote listener is enabled.

## Reproduce validation

Run from the repository root on each native Mac/Linux target:

```sh
bash packaging/macos/tests/open_webui_runtime_test.sh
```

This downloads checksum-verified uv 0.13.9, provisions managed Python 3.12 and
installs the lock in temporary state. On macOS it exercises the actual installer
`prepare` path. It audits **every locked platform version**, then starts a private
WebUI on a random loopback port with a fake Ollama. It checks first-admin signup,
closed subsequent signup, invalid-password/token rejection, login/session,
model discovery, chat, file storage, external-extractor guidance for PDF files
and restart persistence. It never creates a
host service or contacts the workstation's model server. Both native CI
workflows run it; existing tests cover launchd/systemd plans and conflict handling.

Regenerate and audit with the same explicit configuration used by the installers:

```sh
uv lock --project packaging/macos/open-webui \
  --config-file packaging/macos/open-webui/uv.toml
python3.12 packaging/macos/open-webui/audit.py --report /tmp/webui-audit.json
```

The 2026-11-23 review of the final lock found **zero published PyPI release
advisories across 179 locked versions**, with no exceptions. This is a dated
advisory result, not a guarantee against undisclosed vulnerabilities. Audit
network failures fail the check. Binary-only installation plans are checked for macOS ARM64 and Linux ARM64/
x86-64. Managed installation is unavailable on Intel Macs: patched cryptography
50.x no longer publishes Intel macOS wheels. The installer explains this before
downloading anything; connecting an existing WebUI still works on Intel Macs.
Enterprise OAuth and externally configured RAG providers need their own tests.

## Existing installations, upgrades and rollback

Re-running the installer **preserves existing services and data**; it does not
silently switch a full installation to slim or upgrade its database. Environment
directories contain the version and the hash of the project, configuration and
lock. Keep old environments until an upgrade has been verified. Sluicework's
application backup does not include WebUI data.

For an intentional WebUI upgrade, use this maintenance procedure:

1. Prepare and smoke-test the candidate in disposable state first. On macOS the
   installed `open-webui-setup.sh` supports `prepare` instead of `serve`, creating
   an environment without changing the login agent. On Linux copy the three
   locked project files to a new versioned environment and run the `uv sync`
   command used by `packaging/linux/runtime.py`; never sync the active environment.
2. Stop the service and prevent writes: macOS, `launchctl bootout
   gui/$(id -u)/com.sluicework.open-webui`; Linux, `sudo systemctl stop
   sluicework-open-webui`. Verify the old process has exited before copying data.
3. Make a private, **cold backup** of the complete data directory, secret,
   service definition and ownership manifest. On macOS these are `data/`,
   `secret.key`, `launchd-manifest.json` under
   `~/Library/Application Support/Sluicework/open-webui`, plus
   `~/Library/LaunchAgents/com.sluicework.open-webui.plist`. Linux uses
   `/var/lib/sluicework/inference/open-webui-data`, `open-webui-secret.key`
   alongside it, `/etc/sluicework/open-webui.env`, and
   `/etc/systemd/system/sluicework-open-webui.service`.
   Include all SQLite/WAL files and uploads; do not use a live file copy.
   Protect backups with the original ownership and modes; they contain credentials.
4. Explicitly update the owned service's executable and profile environment for
   the candidate. Preserve `DATA_DIR`, Ollama URL, CA trust and the secret.
   For macOS, record the new plist SHA256 in its ownership manifest. Bootstrap
   the plist again; on Linux run `systemctl daemon-reload` and start the service.
5. Verify health, sign-in, existing chats/files and a test chat. If validation
   fails, stop the candidate, restore the **entire matching cold data backup**,
   secret, old service definition and manifest, then start the old environment.
   Never point the old binary at a database migrated by the candidate.

Automatic in-place upgrades are intentionally outside this installer. A profile
change involving document/vector features needs a separate migration review.
