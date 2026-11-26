import { useEffect, useState } from "react";
import { Check, Copy, Download } from "lucide-react";
import { api, base } from "./api";
import { Dialog, ErrorMessage, Field, Form } from "./ui";
import type { ServerSettings } from "./settings";

export function Enrollment({ close }: { close: () => void }) {
  const [settings, setSettings] = useState<ServerSettings>();
  const [result, setResult] = useState<{ token: string; expiresIn: number }>();
  const [name, setName] = useState("");
  const [url, setUrl] = useState(location.origin);
  const [os, setOs] = useState("darwin");
  const [arch, setArch] = useState("arm64");
  const [kind, setKind] = useState("ci");
  const [copied, setCopied] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    void api<ServerSettings>("/settings")
      .then((value) => {
        setSettings(value);
        setUrl(value.network.url || location.origin);
        setOs(value.os);
        setArch(value.arch);
      })
      .catch((e) => setError((e as Error).message));
  }, []);
  const download = async () => {
    setBusy(true);
    setError("");
    try {
      const query = new URLSearchParams({ name, url, os, arch, kind });
      const response = await fetch(base + "/settings/worker-bundle?" + query, {
        credentials: "same-origin",
      });
      if (!response.ok) {
        const body = await response.json();
        throw new Error(body.error || "Setup download failed");
      }
      const object = URL.createObjectURL(await response.blob());
      const link = document.createElement("a");
      link.href = object;
      link.download = "Sluicework-worker-setup.zip";
      link.click();
      setTimeout(() => URL.revokeObjectURL(object), 1000);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title="Enroll a machine" close={close}>
      {!result ? (
        <Form
          close={close}
          label="Prepare worker setup"
          submit={async () =>
            setResult(await api("/enrollment-tokens", "POST"))
          }
        >
          <p>
            Prepare this machine from the main admin. The new worker connects
            outbound and needs no separate admin account or GitHub App key.
          </p>
          {error && <ErrorMessage>{error}</ErrorMessage>}
          <Field label="Worker name">
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={128}
              required
              placeholder="Second MacBook"
            />
          </Field>
          <Field label="Worker type">
            <select value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="ci">Optional CI workstation</option>
              <option value="worker">Script and AI worker</option>
            </select>
          </Field>
          <Field label="Operating system">
            <select value={os} onChange={(e) => setOs(e.target.value)}>
              <option value="darwin">macOS</option>
              <option value="linux">Linux</option>
            </select>
          </Field>
          <Field label="Architecture">
            <select value={arch} onChange={(e) => setArch(e.target.value)}>
              <option value="arm64">ARM64 / Apple Silicon</option>
              <option value="amd64">x86-64 / Intel</option>
            </select>
          </Field>
          <Field
            label="Cluster URL"
            hint="Use the HTTPS URL configured in Settings → Network access. A loopback HTTP address is appropriate only with a local SSH tunnel."
          >
            <input
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              type="url"
              required
            />
          </Field>
          {!settings?.network.enabled && (
            <p className="callout">
              For direct WiFi access, first enable HTTPS in Settings → Network
              access. The existing loopback URL works only on its own machine or
              through a tunnel.
            </p>
          )}
        </Form>
      ) : (
        <div className="form-body">
          <p>
            <b>{name}</b> · {os === "darwin" ? "macOS" : "Linux"} ·{" "}
            {arch === "arm64" ? "ARM64" : "x86-64"}
          </p>
          <Field label="Cluster URL">
            <input readOnly value={url} />
          </Field>
          <Field
            label="One-use enrollment token"
            hint={`Expires ${Math.ceil(result.expiresIn / 60)} minutes after issue. Paste it at the worker’s hidden setup prompt; it is not included in the download.`}
          >
            <input
              readOnly
              className="mono"
              value={result.token}
              onFocus={(e) => e.target.select()}
            />
          </Field>
          <button
            onClick={async () => {
              try {
                await navigator.clipboard.writeText(result.token);
                setCopied(true);
              } catch {
                setError("Select and copy the token from the field above.");
              }
            }}
          >
            {copied ? <Check size={15} /> : <Copy size={15} />}
            {copied ? "Copied" : "Copy token"}
          </button>
          <button className="primary" disabled={busy} onClick={download}>
            <Download size={15} />
            {busy ? "Preparing download…" : "Download worker setup"}
          </button>
          <ol className="guidance">
            <li>
              Extract the setup ZIP into a new folder on the worker, under its
              normal non-root login.
            </li>
            {settings && (os !== settings.os || arch !== settings.arch) && (
              <li>
                This server runs on another platform. Obtain the matching
                Sluicework installer from the platform builds and copy its{" "}
                <code>sluicework</code> binary into the setup folder;
                configuration and helper are already included.
              </li>
            )}
            {kind === "ci" && (
              <li>
                Install rustup and Python 3.12. On macOS, also install Xcode
                Command Line Tools.
              </li>
            )}
            {kind === "worker" && (
              <li>Install Python 3.9+ for the setup helper.</li>
            )}
            <li>
              In the extracted folder, verify <code>SHA256SUMS</code>, run{" "}
              <code>chmod 700 ./sluicework ./manage</code>, then{" "}
              <code>python3 ./manage join</code>.
            </li>
            <li>
              Compare the public CA fingerprint with this main admin before
              accepting trust. Enter the one-use token at the hidden prompt.
            </li>
            <li>
              {kind === "ci"
                ? "Return to Machines here, approve its GitHub runner and resource profile, then choose Offer capacity."
                : "Return to Machines here to configure labels, slots, and approved jobs. The worker has no separate admin console."}
            </li>
          </ol>
          {settings?.network.fingerprint && (
            <>
              <p>Cluster CA SHA-256</p>
              <code className="fingerprint">
                {settings.network.fingerprint}
              </code>
            </>
          )}
          <p>
            Each workstation keeps local pause controls. Settings and approval
            come from this main admin.
          </p>
          <button
            onClick={() => {
              setResult(undefined);
              setCopied(false);
            }}
          >
            Prepare another worker or renew the token
          </button>
          {error && <ErrorMessage>{error}</ErrorMessage>}
        </div>
      )}
    </Dialog>
  );
}
