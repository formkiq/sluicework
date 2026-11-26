import { useEffect, useRef, useState } from "react";
import { Download, ExternalLink, RefreshCw } from "lucide-react";
import { api } from "./api";
import { ErrorMessage, Field, Form } from "./ui";

export interface ServerSettings {
  hostname: string;
  os: string;
  arch: string;
  dataDir: string;
  primaryListen: string;
  logLimit: number;
  githubIssue?: string;
  github?: {
    organizationRunners: boolean;
    workstationGroupId: number;
    workstationRepositories: string[];
    appId: number;
    installationId: number;
    repository: string;
    keyConfigured: boolean;
  };
  openWebUI?: {
    enabled: boolean;
    url: string;
    ollamaBaseUrl?: string;
  };
  network: {
    enabled: boolean;
    hostname: string;
    port: number;
    url?: string;
    caPem?: string;
    fingerprint?: string;
    issue?: string;
    certificateExpires?: string;
  };
}
export function downloadText(name: string, value: string) {
  const url = URL.createObjectURL(
    new Blob([value], { type: "application/x-pem-file" }),
  );
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
function NetworkSettings({
  initial,
  saved,
}: {
  initial: ServerSettings;
  saved: (s: ServerSettings) => void;
}) {
  const [enabled, setEnabled] = useState(initial.network.enabled);
  const n = initial.network;
  return (
    <section className="panel setup-panel">
      <h2>Network access</h2>
      <p>
        Connect other Macs and Linux workers over verified HTTPS. Your current
        primary connection remains available while this additional listener is
        managed here.
      </p>
      {n.issue && <ErrorMessage>{n.issue}</ErrorMessage>}
      {n.enabled && n.url && (
        <div className="callout">
          <b>Cluster URL</b>
          <p>
            <a href={n.url} target="_blank" rel="noreferrer">
              {n.url} <ExternalLink size={12} />
            </a>
          </p>
        </div>
      )}
      <Form
        cancelLabel="Reset"
        label="Save network access"
        close={() => setEnabled(n.enabled)}
        submit={async (d) =>
          saved(
            await api<ServerSettings>("/settings/network", "PUT", {
              enabled,
              hostname: String(d.get("hostname") || ""),
              port: Number(d.get("port") || 8443),
              rotateCa: d.has("rotateCa"),
            }),
          )
        }
      >
        <label className="check">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          Enable HTTPS for other machines
        </label>
        {enabled && (
          <>
            <Field
              label="Server hostname"
              hint="Use a stable name that resolves to this server from your WiFi, such as your Mac’s LocalHostName.local. A private IP is also supported."
            >
              <input
                name="hostname"
                defaultValue={
                  n.hostname ||
                  (initial.os === "darwin"
                    ? initial.hostname.replace(/\.local$/, "") + ".local"
                    : initial.hostname)
                }
                required
              />
            </Field>
            <Field
              label="HTTPS port"
              hint="Allow Sluicework’s incoming connections on this port in the host firewall. Router port forwarding is unnecessary for local WiFi."
            >
              <input
                name="port"
                type="number"
                min={1024}
                max={65535}
                defaultValue={n.port || 8443}
                required
              />
            </Field>
            {(n.caPem || n.enabled) && (
              <label className="check">
                <input type="checkbox" name="rotateCa" />
                Replace the cluster CA (all connected workers must trust the new
                CA)
              </label>
            )}
            <p>
              Sluicework creates a private cluster CA and a hostname-matched
              server certificate. Saving validates the port before changing the
              listener. Changes take effect without restarting the existing
              server.
            </p>
          </>
        )}
        {!enabled && n.enabled && (
          <p>
            Disabling network access disconnects workers using this HTTPS
            endpoint. The primary listener stays available.
          </p>
        )}
      </Form>
      {n.caPem && (
        <div className="setup-certificate">
          <h3>Trust the cluster certificate</h3>
          {n.certificateExpires && (
            <p>
              Server certificate expires{" "}
              {new Date(n.certificateExpires).toLocaleDateString()}. Sluicework
              renews it automatically before expiry while preserving the cluster
              CA.
            </p>
          )}
          <p>
            Transfer only the public CA to each new machine. Compare this
            SHA-256 fingerprint against the server’s trusted admin screen before
            accepting it.
          </p>
          <code className="fingerprint">{n.fingerprint}</code>
          <button
            onClick={() => downloadText("Sluicework-cluster-ca.pem", n.caPem!)}
          >
            <Download size={15} /> Download public CA
          </button>
          <p>
            On macOS, import the verified CA into Keychain Access and configure
            trust for SSL before opening the remote admin in Safari. Worker
            setup uses its own CA file and does not alter system trust.
          </p>
          <a
            href="https://support.apple.com/guide/keychain-access/change-the-trust-settings-of-a-certificate-kyca11871/mac"
            target="_blank"
            rel="noreferrer"
          >
            Apple certificate trust instructions <ExternalLink size={12} />
          </a>
        </div>
      )}
    </section>
  );
}
function GitHubSettings({
  initial,
  saved,
}: {
  initial: ServerSettings;
  saved: (s: ServerSettings) => void;
}) {
  const [enabled, setEnabled] = useState(!!initial.github);
  const [key, setKey] = useState("");
  const [message, setMessage] = useState("");
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState("");
  const keyInput = useRef<HTMLInputElement>(null);
  const g = initial.github;
  const [groups, setGroups] = useState<{id: number; name: string}[]>([]);
  const [groupIssue, setGroupIssue] = useState('');
  const loadGroups = async () => {
    try { setGroups(await api('/github/runner-groups')); setGroupIssue(''); }
    catch (e) { setGroupIssue((e as Error).message); }
  };
  useEffect(() => { if (g?.organizationRunners) void loadGroups(); }, [g?.organizationRunners]);
  return (
    <section className="panel setup-panel">
      <h2>GitHub App connection</h2>
      <p>
        The server holds the App key. Workers receive short-lived runner
        credentials for their approved repository; they need no GitHub App
        configuration or separate administrator.
      </p>
      {initial.githubIssue && (
        <ErrorMessage>{initial.githubIssue}</ErrorMessage>
      )}
      <ol className="guidance">
        <li>Install your GitHub App on the intended repository.</li>
        <li>
          Grant Administration read/write, Metadata read, and Actions read.
          Approve permission changes on its GitHub installation.
        </li>
        <li>
          Enter the App ID, installation ID and repository, then upload its PEM
          private key. This server-to-server connection needs no OAuth callback
          or webhook.
        </li>
      </ol>
      <Form
        cancelLabel="Reset"
        label="Save GitHub connection"
        close={() => {
          setKey("");
          setEnabled(!!g);
          setMessage("");
          setError("");
        }}
        submit={async (d) => {
          setMessage("");
          setError("");
          const result = await api<ServerSettings>("/settings/github", "PUT", {
            enabled,
            appId: Number(d.get("appId")),
            installationId: Number(d.get("installationId")),
            repository: String(d.get("repository") || ""),
            privateKeyPem: key,
            organizationRunners: d.has('organizationRunners'),
            workstationGroupId: Number(d.get('workstationGroupId') || 0),
            workstationRepositories: String(d.get('workstationRepositories') || '').split(/[\s,]+/).filter(Boolean),
          });
          setKey("");
          if (keyInput.current) keyInput.current.value = "";
          saved(result);
        }}
      >
        <label className="check">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          Enable GitHub runner management
        </label>
        {enabled && (
          <>
            <label className="check">
              <input type="checkbox" name="organizationRunners" defaultChecked={g?.organizationRunners || false} />
              Manage organization GitHub runners
            </label>
            <p>Requires organization Self-hosted runners: read/write on the App, approved by the organization. Organization runner changes can affect other repositories. Placement management additionally requires repository Contents: read and Variables: write.</p>
            <Field label="GitHub App ID">
              <input
                name="appId"
                type="number"
                min={1}
                defaultValue={g?.appId}
                required
              />
            </Field>
            <Field label="Installation ID">
              <input
                name="installationId"
                type="number"
                min={1}
                defaultValue={g?.installationId}
                required
              />
            </Field>
            <Field
              label="Repository"
              hint="Primary repository used for worker approval and the default placement view."
            >
              <input
                name="repository"
                defaultValue={g?.repository}
                placeholder="example-org/application"
                required
              />
            </Field>
            <details>
              <summary>Share workstation runners across repositories</summary>
              <p>Use one organization runner group limited to the private repositories below. One Mac listener serves the group, so its pause, expiry and one-build limit apply across all of them. Install the App on every listed repository. Drain existing listeners before changing this scope.</p>
              <Field label="Workstation runner group" hint="Only dedicated groups with selected private repositories are offered. Choose repository-only and clear the list below to disable sharing.">
                <select name="workstationGroupId" defaultValue={g?.workstationGroupId || 0} key={groups.map((group) => group.id).join(',')}>
                  <option value={0}>Repository-only runners</option>
                  {!!g?.workstationGroupId && !groups.some((group) => group.id === g.workstationGroupId) && <option value={g.workstationGroupId}>Current group {g.workstationGroupId} (unavailable)</option>}
                  {groups.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}
                </select>
              </Field>
              <button type="button" onClick={loadGroups}>Refresh runner groups</button>
              {groupIssue && <ErrorMessage>{groupIssue}</ErrorMessage>}
              <Field label="Shared workstation repositories" hint="One owner/repository per line, including the primary repository. The group must match this exact list; public and inherited groups are rejected.">
                <textarea name="workstationRepositories" defaultValue={g?.workstationRepositories?.join('\n') || ''} placeholder={'example-org/application\nexample-org/documentation'} />
              </Field>
              <a href={g?.repository ? `https://github.com/organizations/${g.repository.split('/')[0]}/settings/actions/runner-groups` : 'https://github.com/settings/organizations'} target="_blank" rel="noreferrer">Open GitHub runner groups <ExternalLink size={12} /></a>
            </details>
            <Field
              label="App private key (PEM)"
              hint={
                g?.keyConfigured
                  ? "A key is configured. Choose a file only to replace it; existing key contents are never returned."
                  : "Upload the private key downloaded from the GitHub App settings."
              }
            >
              <input
                ref={keyInput}
                type="file"
                accept=".pem"
                required={!g?.keyConfigured}
                onChange={async (e) => {
                  const file = e.target.files?.[0];
                  if (!file) {
                    setKey("");
                    return;
                  }
                  if (file.size > 16384) {
                    setKey("");
                    setError("The PEM key must be no larger than 16 KiB.");
                    e.target.value = "";
                    return;
                  }
                  setError("");
                  setKey(await file.text());
                }}
              />
            </Field>
          </>
        )}
        <p>
          Drain workstation runners before changing the App, installation or
          repository. Replacing a key for the same connection preserves existing
          runner registrations.
        </p>
      </Form>
      {g && (
        <button
          disabled={testing}
          onClick={async () => {
            setTesting(true);
            setError("");
            setMessage("");
            try {
              await api("/settings/github/test", "POST");
              setMessage(
                "Runner registration and Actions read access verified.",
              );
            } catch (e) {
              setError((e as Error).message);
            } finally {
              setTesting(false);
            }
          }}
        >
          <RefreshCw size={15} />{" "}
          {testing ? "Checking…" : "Test saved connection"}
        </button>
      )}
      {message && <p role="status">{message}</p>}
      {error && <ErrorMessage>{error}</ErrorMessage>}
    </section>
  );
}
function OpenWebUISettings({
  initial,
  saved,
}: {
  initial: ServerSettings;
  saved: (s: ServerSettings) => void;
}) {
  const current = initial.openWebUI;
  const [enabled, setEnabled] = useState(Boolean(current?.enabled));
  const [testing, setTesting] = useState(false);
  const [testMessage, setTestMessage] = useState("");
  const [testError, setTestError] = useState("");
  const test = async () => {
    setTesting(true);
    setTestMessage("");
    setTestError("");
    try {
      await api("/settings/open-webui/test", "POST", {});
      setTestMessage("Open WebUI responded to /health.");
    } catch (err) {
      setTestError((err as Error).message);
    } finally {
      setTesting(false);
    }
  };
  return (
    <section className="panel setup-panel">
      <h2>Open WebUI</h2>
      <p>
        Connect an existing Open WebUI chat interface to
        surface its live health and launch link in AI connections. Models
        installed into Ollama via Manage models appear automatically in Open
        WebUI.
      </p>
      {current?.enabled && current.url && (
        <div className="callout">
          <b>Open WebUI URL</b>
          <p>
            <a href={current.url} target="_blank" rel="noreferrer">
              {current.url} <ExternalLink size={12} />
            </a>
          </p>
        </div>
      )}
      {testError && <ErrorMessage>{testError}</ErrorMessage>}
      {testMessage && (
        <p role="status" className="callout">
          {testMessage}
        </p>
      )}
      <Form
        cancelLabel="Reset"
        label="Save Open WebUI"
        close={() => {
          setEnabled(Boolean(current?.enabled));
          setTestMessage("");
          setTestError("");
        }}
        submit={async (d) =>
          saved(
            await api<ServerSettings>("/settings/open-webui", "PUT", {
              enabled,
              url: String(d.get("url") || "").trim(),
              ollamaBaseUrl: String(d.get("ollamaBaseUrl") || "").trim(),
            }),
          )
        }
      >
        <label className="check">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          Enable Open WebUI in the console
        </label>
        <Field
          label="Open WebUI URL"
          hint="Use http://127.0.0.1:8080 for the local managed service, or a verified HTTPS origin for LAN access."
        >
          <input
            name="url"
            defaultValue={current?.url || "http://127.0.0.1:8080"}
            placeholder="http://127.0.0.1:8080"
            required={enabled}
          />
        </Field>
        <Field
          label="Ollama backend URL"
          hint="Optional record of the Ollama or Herd endpoint Open WebUI uses (typically http://127.0.0.1:11434 or http://127.0.0.1:11435)."
        >
          <input
            name="ollamaBaseUrl"
            defaultValue={current?.ollamaBaseUrl || "http://127.0.0.1:11434"}
            placeholder="http://127.0.0.1:11434"
          />
        </Field>
      </Form>
      {current?.url && (
        <div className="actions">
          <button type="button" disabled={testing} onClick={() => void test()}>
            <RefreshCw size={14} />
            {testing ? "Testing…" : "Test connection"}
          </button>
        </div>
      )}
    </section>
  );
}

export function Settings() {
  const [settings, setSettings] = useState<ServerSettings>();
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const load = async () => {
    try {
      setSettings(await api<ServerSettings>("/settings"));
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  };
  useEffect(() => {
    void load();
  }, []);
  const saved = (next: ServerSettings) => {
    setSettings(next);
    setNotice("Settings saved.");
  };
  return (
    <>
      <div className="heading">
        <div>
          <h1>Settings</h1>
          <p>
            Set up this cluster and manage connected machines from one admin.
          </p>
        </div>
      </div>
      {error && (
        <ErrorMessage>
          {error} <button onClick={load}>Retry</button>
        </ErrorMessage>
      )}
      {notice && (
        <p className="callout" role="status">
          {notice}
        </p>
      )}
      {settings ? (
        <div className="setup-grid">
          <NetworkSettings
            key={JSON.stringify(settings.network)}
            initial={settings}
            saved={saved}
          />
          <GitHubSettings
            key={JSON.stringify(settings.github)}
            initial={settings}
            saved={saved}
          />
          <OpenWebUISettings
            key={JSON.stringify(settings.openWebUI)}
            initial={settings}
            saved={saved}
          />
          <section className="panel setup-panel">
            <h2>Retained output</h2>
            <Form
              cancelLabel="Reset"
              label="Save output limit"
              close={() => setNotice("")}
              submit={async (d) =>
                saved(
                  await api<ServerSettings>("/settings/log-limit", "PUT", {
                    bytes: Math.round(Number(d.get("limit")) * 1048576),
                  }),
                )
              }
            >
              <Field
                label="Output limit per attempt (MiB)"
                hint="Maximum retained log/output bytes for each Sluicework attempt. GitHub build logs remain on GitHub."
              >
                <input
                  name="limit"
                  type="number"
                  min={1 / 1048576}
                  max={64}
                  step="any"
                  defaultValue={settings.logLimit / 1048576}
                  required
                />
              </Field>
            </Form>
          </section>
          <section className="panel setup-panel">
            <h2>Installation details</h2>
            <dl className="detail-grid">
              <div>
                <dt>Server</dt>
                <dd>{settings.hostname}</dd>
              </div>
              <div>
                <dt>Platform</dt>
                <dd>
                  {settings.os} · {settings.arch}
                </dd>
              </div>
              <div>
                <dt>Primary listener</dt>
                <dd>{settings.primaryListen}</dd>
              </div>
              <div>
                <dt>Cluster storage</dt>
                <dd className="path-value">{settings.dataDir}</dd>
              </div>
            </dl>
            <p>
              The primary listener, storage location and worker identities
              belong to initial installation. Use Network access above to add
              WiFi access, and Enroll machine to prepare another worker. Moving
              cluster storage requires backup and restore.
            </p>
          </section>
        </div>
      ) : (
        !error && <p role="status">Loading settings…</p>
      )}
    </>
  );
}
