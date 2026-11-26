# Back up and restore the orchestrator

## Make a recoverable copy

Run the SQLite-aware backup on the orchestrator. Use a new directory for each snapshot. Take a copy before upgrades and at the team's recovery interval.

```bash
base=/Library/Sluicework
snapshot="$base/backups/cluster-$(date -u +%Y%m%dT%H%M%SZ)"
sudo -u _sluiceserver "$base/bin/sluicework" backup \
  --config "$base/etc/server.json" --out "$snapshot"
```

The new directory contains a verified `state.db`, a copy of `server.json` and a SHA-256 `manifest.json`. The database snapshot includes job revisions, machine registrations, users, schedules, occurrences, attempts, logs and retained results. Copy the snapshot to encrypted storage with tightly controlled access. A live database can have committed data in its WAL; use the backup command for a consistent snapshot.

Archive the root-owned TLS files from the `tlsCert` and `tlsKey` paths in `server.json`, the public CA certificate, and the non-secret worker settings separately. If using the generated CA, escrow `/Library/Sluicework/pki` in the team's encrypted signing-key storage. Protect backups of browser account hashes and internal run data. Keep provider keys in the team's credential manager. The application backup excludes the provider secret directory. Re-provision file inputs, the inference runtime and any GitHub runner with their respective installers and data sources.

## Recover on a replacement Mac

Perform this rehearsal on an isolated network or after stopping the previous orchestrator. Two active servers restored from one snapshot could dispatch the same schedule independently.

1. Run **Create a cluster** on the replacement Mac with the release that created the snapshot, using loopback during setup. This creates the service accounts and LaunchDaemons. Keep the original cluster's DNS name directed away from the replacement during setup.
2. Stop both services. Preserve the disposable bootstrap data and credential, and prepare the empty data directory. The following commands assume a fresh replacement with no existing `.bootstrap` paths.

   ```bash
   base=/Library/Sluicework
   sudo launchctl bootout system/com.sluicework.worker
   sudo launchctl bootout system/com.sluicework.server
   sudo mv "$base/data" "$base/data.bootstrap"
   sudo install -d -o _sluiceserver -g _sluiceserver -m 0700 "$base/data"
   sudo mv "$base/worker/credential.json" "$base/worker/credential.bootstrap"
   ```

3. Copy the saved backup directory to `/Library/Sluicework/backups/restoring` and grant `_sluiceserver` read access. Keep the replacement's loopback server settings for the first startup. Restore into the empty data directory, then start the server. Sign in using an account from the restored snapshot.

   ```bash
   sudo -u _sluiceserver "$base/bin/sluicework" restore \
     --config "$base/etc/server.json" --from "$base/backups/restoring"
   sudo launchctl bootstrap system /Library/LaunchDaemons/com.sluicework.server.plist
   ```

4. After the loopback console responds, enrol the replacement's local worker and start it.

   ```bash
   sudo -u _sluiceserver "$base/bin/sluicework" enroll-token --config "$base/etc/server.json" | \
     sudo -u _sluiceworker "$base/bin/sluicework" join --config "$base/etc/worker.json"
   sudo launchctl bootstrap system /Library/LaunchDaemons/com.sluicework.worker.plist
   ```

5. In **Machines**, copy the old local worker’s labels, runner registration and caps to the new identity. Restore retains all machine and attempt history, revokes the restored machine credentials, clears enrolment tokens and browser sessions, and marks formerly running attempts interrupted with unknown outcomes. Queued attempts from the snapshot are cancelled for manual review because they may already have run since the backup was taken. Check external effects before retrying. Recovery does not automatically repeat restored attempts. Sign in again to issue fresh tokens.
6. In **AI connections**, rotate each cloud key from the team's credential manager and test every connection. Explicitly review the approved models and enabled schedules as they stood at backup time. Confirm that you can open a known completed run, see its attempts and read its retained output.
7. Move the stable DNS name to the replacement while the previous host remains offline. To preserve trust, either restore the original CA key and certificate to the root-only `pki` directory before issuing a new leaf or provide a valid customer certificate for that name. Use **Enable or update cluster HTTPS**. If you change the CA, distribute and verify its fingerprint and update each remote worker's CA file and browser trust.
8. For each remote Mac, issue a fresh token and choose **Re-enrol an existing worker after restore** in the installer. The worker uses its configured HTTPS origin and CA. Set labels, runner registration and caps on its new machine identity. Watch remote heartbeats and run a known safe template. Reconcile any interrupted work with unknown outcomes before requesting a retry. Check schedule history for missed occurrences and verify the next local execution time. Record backup time, replacement time and the run and schedule IDs recovered.

A server outage suspends new claims, schedules and brokered inference until recovery. GitHub continues managing its own connected runners. Installed Sluicework workers retain their local workspaces only for active attempts.

## Admin-managed setup material

Managed server settings and per-worker profiles are stored in SQLite and included
in the normal database snapshot. Separately escrow `<dataDir>/configuration-secrets/`
in encrypted storage: it contains the generated cluster CA, TLS signing material,
and uploaded GitHub App keys. These secrets are not in worker setup ZIPs or API
responses. Preserve bootstrap configuration and any legacy installer TLS/App files.
After restore, use **Settings** to inspect connection issues, upload a missing App
key, or explicitly replace network trust and re-enroll workers. A managed HTTPS
listener failure leaves the original primary listener available for recovery.
