import { useEffect, useState } from 'react'
import { api } from './api'
import { ErrorMessage, Field, Form } from './ui'

type Settings = {
  version: number
  backendId: string
  enabled: boolean
  maxConcurrent: number
  queueMinutes: number
  startupMinutes: number
  runtimeMinutes: number
  repositories: Record<string, string>
}
type Profile = {
  id: string
  instanceType: string
  cpus: number
  memoryMiB: number
  maxPrice: string
}
type Fleet = {
  repository: string
  repositories: string[]
  settings: Settings
  profiles: Profile[]
  revision: string
  ready: boolean
  issue?: string
  health?: {
    region: string
    at: string
    allocations: {
      id: string
      instanceId?: string
      state: string
      instanceType: string
      expires: string
    }[]
  }
}
export function SpotRunners() {
  const [data, setData] = useState<Fleet>()
  const [settings, setSettings] = useState<Settings>()
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const load = async () => {
    try {
      const value = await api<Fleet>('/github/spot')
      setData(value)
      setSettings(value.settings)
      setError('')
    } catch (e) {
      setError((e as Error).message)
    }
  }
  useEffect(() => {
    void load()
  }, [])
  return (
    <section className="panel setup-panel spot-fleet">
      <div className="heading">
        <div>
          <h2>AWS Spot runners</h2>
          <p>Shared across connected repositories; up to three runners in total.</p>
        </div>
        <button onClick={load}>Refresh Spot fleet</button>
      </div>
      <p>
        Preferred self-hosted pool → available Mac → Spot → GitHub-hosted Linux. Local-only policies keep cloud runners
        disabled. Spot machines start for one build and terminate afterward.
      </p>
      {error && <ErrorMessage>{error}</ErrorMessage>}
      {notice && (
        <p role="status" className="callout">
          {notice}
        </p>
      )}
      {data && settings && (
        <>
          {data.issue && <p className="callout">{data.issue}</p>}
          <p>
            Settings are saved in <b>{data.repository}</b> and stay available when this admin is
            offline.{' '}
            {data.health && (
              <>
                Region: {data.health.region}. Last reported{' '}
                {new Date(data.health.at).toLocaleString()}.
              </>
            )}
          </p>
          <Form
            label="Save Spot settings"
            cancelLabel="Reset"
            close={() => setSettings(data.settings)}
            submit={async () => {
              const saved = await api<Fleet>('/github/spot', 'PUT', {
                settings,
                revision: data.revision,
              })
              setData(saved)
              setSettings(saved.settings)
              setNotice(
                'Spot settings saved. New requests use this policy; running builds keep their existing limits.',
              )
            }}
          >
            <label className="check">
              <input
                type="checkbox"
                checked={settings.enabled}
                disabled={!data.ready && !settings.enabled}
                onChange={(e) => setSettings({ ...settings, enabled: e.target.checked })}
              />{' '}
              Enable on-demand Spot runners
            </label>
            <div className="form-grid">
              <Field
                label="Maximum simultaneous Spot runners"
                hint="Counts starting, busy and terminating machines across every repository."
              >
                <select
                  value={settings.maxConcurrent}
                  onChange={(e) =>
                    setSettings({ ...settings, maxConcurrent: Number(e.target.value) })
                  }
                >
                  {[0, 1, 2, 3].map((n) => (
                    <option key={n} value={n}>
                      {n === 0 ? '0 — pause new runners' : n}
                    </option>
                  ))}
                </select>
              </Field>
              <Field
                label="Wait for a Spot slot (minutes)"
                hint="After this wait, use GitHub-hosted Linux if no machine was reserved."
              >
                <input
                  type="number"
                  min={1}
                  max={30}
                  required
                  value={settings.queueMinutes}
                  onChange={(e) =>
                    setSettings({ ...settings, queueMinutes: Number(e.target.value) })
                  }
                />
              </Field>
              <Field label="Startup timeout (minutes)">
                <input
                  type="number"
                  min={1}
                  max={15}
                  required
                  value={settings.startupMinutes}
                  onChange={(e) =>
                    setSettings({ ...settings, startupMinutes: Number(e.target.value) })
                  }
                />
              </Field>
              <Field
                label="Maximum machine lifetime (minutes)"
                hint="Includes setup and the build; the VM shuts down even if the dispatcher is unavailable."
              >
                <input
                  type="number"
                  min={15}
                  max={180}
                  required
                  value={settings.runtimeMinutes}
                  onChange={(e) =>
                    setSettings({ ...settings, runtimeMinutes: Number(e.target.value) })
                  }
                />
              </Field>
            </div>
            <h3>Repository profiles</h3>
            <p>
              The standard profile has 2 CPUs and 4 GiB RAM. Use the small profile only for light
              builds. Profile selection applies to future queue requests.
            </p>
            {data.repositories.map((repo) => (
              <Field key={repo} label={repo}>
                <select
                  value={settings.repositories[repo] || ''}
                  onChange={(e) => {
                    const repos = { ...settings.repositories }
                    if (e.target.value) repos[repo] = e.target.value
                    else delete repos[repo]
                    setSettings({ ...settings, repositories: repos })
                  }}
                >
                  <option value="">Skip Spot</option>
                  {data.profiles.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.instanceType} · {p.cpus} CPU · {p.memoryMiB / 1024} GiB · compute ceiling $
                      {p.maxPrice}/hour
                    </option>
                  ))}
                </select>
              </Field>
            ))}
            <p className="muted">
              AWS charges separately for compute, storage, public IPv4 and network traffic. Price
              ceilings apply to compute. Interrupted Spot builds may retry on hosted Linux after the
              original worker has stopped.
            </p>
          </Form>
          <h3>Fleet activity</h3>
          {data.health?.allocations.length ? (
            <ul>
              {data.health.allocations.map((a) => (
                <li key={a.id}>
                  {a.instanceType} · {a.state} · {a.instanceId || 'starting'} · expires{' '}
                  {new Date(a.expires).toLocaleTimeString()}
                </li>
              ))}
            </ul>
          ) : (
            <p>No reserved Spot machines.</p>
          )}
        </>
      )}
    </section>
  )
}
