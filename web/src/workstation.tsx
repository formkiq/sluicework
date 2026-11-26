import { useState } from "react";
import { api, idPath } from "./api";
import type { Machine, WorkstationSettings } from "./model";
import { Field } from "./ui";

export const sameSettings = (
  a?: WorkstationSettings,
  b?: WorkstationSettings,
) =>
  !!a &&
  !!b &&
  (
    [
      "buildThreads",
      "maxLoadPerCpu",
      "minAvailableMemoryMb",
      "minIdleSeconds",
      "requireAcPower",
      "enableMinutes",
    ] as const
  ).every((key) => a[key] === b[key]);

export function WorkstationSummary({ machine: m }: { machine: Machine }) {
  if (!m.runner?.workstation) return null;
  const status = m.workstation;
  if (!status)
    return (
      <p className="muted">
        Update this controller to view and manage resource settings in admin.
      </p>
    );
  const requested = m.runner.settings || status.local;
  const pending = !sameSettings(requested, status.effective);
  return (
    <div className="workstation-summary">
      <p>
        <b>{status.effective.buildThreads} build threads</b> ·{" "}
        {m.online ? "Reported by controller" : "Last reported"}
        {" · "}
        {m.runner.settings ? "Managed in admin" : "Installation defaults"}
      </p>
      {pending && (
        <p className="callout">
          Settings pending
          {requested.buildThreads !== status.effective.buildThreads
            ? `: ${requested.buildThreads} build threads requested`
            : ""}
          . The controller applies them between builds when it is online.
        </p>
      )}
      {status.enabledUntil && Date.parse(status.enabledUntil) > 0 && (
        <p>
          Availability{" "}
          {Date.parse(status.enabledUntil) > Date.now() ? "until" : "expired"}{" "}
          {new Date(status.enabledUntil).toLocaleString()}
        </p>
      )}
      <details>
        <summary>Resource settings{pending ? " · change pending" : ""}</summary>
        <table>
          <thead>
            <tr>
              <th>Setting</th>
              <th>Requested</th>
              <th>Reported</th>
            </tr>
          </thead>
          <tbody>
            {(
              [
                ["Build threads", "buildThreads", ""],
                ["Free memory", "minAvailableMemoryMb", " MiB"],
                ["Idle time", "minIdleSeconds", " seconds"],
                ["Enable window", "enableMinutes", " minutes"],
              ] as const
            ).map(([name, key, unit]) => (
              <tr key={key}>
                <td>{name}</td>
                <td>
                  {requested[key]}
                  {unit}
                </td>
                <td>
                  {status.effective[key]}
                  {unit}
                </td>
              </tr>
            ))}
            <tr>
              <td>CPU load threshold</td>
              <td>{requested.maxLoadPerCpu * 100}%</td>
              <td>{status.effective.maxLoadPerCpu * 100}%</td>
            </tr>
            <tr>
              <td>Require AC power</td>
              <td>{requested.requireAcPower ? "Yes" : "No"}</td>
              <td>{status.effective.requireAcPower ? "Yes" : "No"}</td>
            </tr>
          </tbody>
        </table>
      </details>
    </div>
  );
}

export function WorkstationFields({
  machine: m,
  settings,
}: {
  machine: Machine;
  settings: WorkstationSettings;
}) {
  return (
    <>
      <p>
        Changes apply between builds without restarting the server or
        controller. An idle runner may take about 75 seconds to refresh. Local
        pause and expiry still control whether this Mac offers capacity.
      </p>
      <Field
        label="Build threads"
        hint="Parallelism preference for supported build tools; one build at a time. This is not a hard CPU cap."
      >
        <select name="buildThreads" defaultValue={settings.buildThreads}>
          {Array.from({ length: 8 }, (_, i) => (
            <option key={i + 1} value={i + 1}>
              {i + 1}
            </option>
          ))}
        </select>
      </Field>
      <Field
        label="CPU load admission threshold (%)"
        hint="A new build requires the one-minute load average divided by logical CPU count to stay below this percentage."
      >
        <input
          name="maxLoadPercent"
          type="number"
          min={0.1}
          max={100}
          step="any"
          defaultValue={settings.maxLoadPerCpu * 100}
          required
        />
      </Field>
      <Field
        label="Minimum available memory (MiB)"
        hint="Admission check before a build starts; does not cap a running build’s memory."
      >
        <input
          name="minAvailableMemoryMb"
          type="number"
          min={512}
          max={1048576}
          defaultValue={settings.minAvailableMemoryMb}
          required
        />
      </Field>
      {m.os !== "linux" && (
        <Field
          label="Required idle time (seconds)"
          hint="Use 0 to allow builds while typing and using other apps."
        >
          <input
            name="minIdleSeconds"
            type="number"
            min={0}
            max={86400}
            defaultValue={settings.minIdleSeconds}
            required
          />
        </Field>
      )}
      <label className="check">
        <input
          type="checkbox"
          name="requireAcPower"
          defaultChecked={settings.requireAcPower}
        />
        Require AC power before starting a build
      </label>
      <Field
        label="Availability window (minutes)"
        hint="Used the next time capacity is enabled locally. Saving this does not extend an existing availability window."
      >
        <input
          name="enableMinutes"
          type="number"
          min={1}
          max={1440}
          defaultValue={settings.enableMinutes}
          required
        />
      </Field>
    </>
  );
}

export function settingsFromForm(data: FormData): WorkstationSettings {
  return {
    buildThreads: Number(data.get("buildThreads")),
    maxLoadPerCpu: Number(data.get("maxLoadPercent")) / 100,
    minAvailableMemoryMb: Number(data.get("minAvailableMemoryMb")),
    minIdleSeconds: Number(data.get("minIdleSeconds") || 0),
    requireAcPower: data.has("requireAcPower"),
    enableMinutes: Number(data.get("enableMinutes")),
  };
}

export function WorkstationCapacity({
  machine: m,
  admin,
}: {
  machine: Machine;
  admin: boolean;
}) {
  const [busy, setBusy] = useState(false);
  const [receipt, setReceipt] = useState<{
    id: string;
    operation: "enable" | "pause";
  }>();
  const [error, setError] = useState("");
  if (!admin || !m.runner?.workstation || m.workstation?.controlVersion !== 1)
    return null;
  const minutes =
    m.runner.settings?.enableMinutes || m.workstation.local.enableMinutes;
  const request = async (operation: "enable" | "pause") => {
    setBusy(true);
    setError("");
    setReceipt(undefined);
    try {
      const next = await api<Machine>(
        idPath("/machines", m.id) + "/workstation/availability",
        "POST",
        { operation, minutes },
      );
      if (next.runner?.control)
        setReceipt({ id: next.runner.control.id, operation });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="workstation-capacity">
      <button
        type="button"
        disabled={busy || !m.online || m.draining || m.revoked}
        onClick={() => request("enable")}
      >
        Offer capacity ({minutes} min)
      </button>
      <button
        type="button"
        disabled={busy || !m.online || m.revoked}
        onClick={() => request("pause")}
      >
        Pause capacity
      </button>
      {!m.online && (
        <p>
          Start this controller locally before offering capacity from admin.
        </p>
      )}
      {m.draining && <p>Clear the machine drain before offering capacity.</p>}
      <p>
        New sessions start paused. A newer local pause takes precedence over an
        earlier admin request.
      </p>
      {receipt && (
        <p role="status">
          {receipt.id === m.workstation.appliedControl
            ? "Controller received the request. See its reported availability above."
            : receipt.operation === "enable"
              ? "Capacity offer requested. Waiting for the controller."
              : "Pause requested. Assigned work will finish."}
        </p>
      )}
      {error && <p className="error-message">{error}</p>}
    </div>
  );
}
