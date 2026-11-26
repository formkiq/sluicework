import { useEffect, useState } from 'react'
import { Download, RefreshCw } from 'lucide-react'
import { api, idPath } from './api'
import type { Connection } from './model'
import { Badge, Dialog, ErrorMessage, Field, Form, val } from './ui'

type Preset = {
  model: string
  name: string
  download: string
  description: string
  minMemoryGB: number
  optional: boolean
  installable: boolean
  source: string
}
type Installation = {
  id: string
  model: string
  status: string
  message: string
  total: number
  completed: number
}
type Inventory = {
  catalog: Preset[]
  installed: { name: string; size: number }[]
  installations: Installation[]
  runtimeError?: string
}
const bytes = (n: number) => (n / 1e9).toFixed(2) + ' GB'

export function ModelSetup({
  connection,
  close,
  changed,
}: {
  connection?: Connection
  close: () => void
  changed: () => void
}) {
  const [current, setCurrent] = useState(connection)
  const [catalog, setCatalog] = useState<Preset[]>([])
  const [inventory, setInventory] = useState<Inventory>()
  const [memory, setMemory] = useState(16)
  const [shared, setShared] = useState(true)
  const [model, setModel] = useState('qwen3.5:4b')
  const [optional, setOptional] = useState(false)
  const [error, setError] = useState('')
  const [loadError, setLoadError] = useState('')
  const [refresh, bump] = useState(0)
  const [busy, setBusy] = useState(false)
  const recommended = memory >= 24 ? 'qwen3.5:9b' : 'qwen3.5:4b'

  useEffect(() => {
    let active = true
    void api<Preset[]>('/model-catalog')
      .then((items) => {
        if (active) setCatalog(items)
      })
      .catch((err) => {
        if (active) setError(err.message)
      })
    return () => {
      active = false
    }
  }, [])
  useEffect(() => {
    if (!current) return
    let active = true
    let timer = 0
    const load = async () => {
      try {
        const result = await api<Inventory>(idPath('/connections', current.id) + '/models')
        if (!active) return
        setInventory(result)
        setLoadError('')
        changed()
      } catch (err) {
        if (active) setLoadError((err as Error).message)
      } finally {
        if (active) timer = window.setTimeout(load, 3000)
      }
    }
    void load()
    return () => {
      active = false
      clearTimeout(timer)
    }
  }, [current?.id, refresh, changed])

  const start = async (c: Connection, tag: string) => {
    await api(idPath('/connections', c.id) + '/models', 'POST', { model: tag })
    bump((n) => n + 1)
  }
  const running = inventory?.installations.find((i) => i.status === 'running')
  const selected = catalog.find((p) => p.model === model)
  const installed = inventory?.installed.some((m) => m.name === model)
  const choices = (
    <>
      <div className="form-grid">
        <Field
          label="Memory on the inference machine"
          hint="Choose the machine running Ollama, which may be different from this browser or worker."
        >
          <select
            value={memory}
            onChange={(e) => {
              const value = Number(e.target.value)
              setMemory(value)
              setModel(value >= 24 ? 'qwen3.5:9b' : 'qwen3.5:4b')
            }}
          >
            {[8, 16, 24, 32, 48, 64, 96, 128].map((n) => (
              <option key={n} value={n}>
                {n} GB
              </option>
            ))}
          </select>
        </Field>
        <Field label="Workload">
          <select
            value={shared ? 'shared' : 'dedicated'}
            onChange={(e) => setShared(e.target.value === 'shared')}
          >
            <option value="shared">Shares this machine with CI or other work</option>
            <option value="dedicated">Dedicated to inference</option>
          </select>
        </Field>
      </div>
      <p className="minor">
        Download sizes are approximate. Runtime memory also depends on context length and concurrent
        requests. Presets were sized for Macs; on Linux, also check GPU VRAM. Recommendations are planning guidance.
      </p>
      <div className="model-presets">
        {catalog
          .filter((p) => optional || !p.optional)
          .map((p) => (
            <label
              key={p.model}
              className={'model-preset' + (model === p.model ? ' selected' : '')}
            >
              <input
                type="radio"
                name="preset"
                value={p.model}
                checked={model === p.model}
                disabled={!p.installable}
                onChange={() => setModel(p.model)}
              />
              <span>
                <b>
                  {p.name} {p.model === recommended && <span className="minor">· Recommended</span>}
                </b>
                <code>{p.model}</code>
                <small>
                  {p.download} download
                  {p.minMemoryGB > 0 ? ` · Plan for ${p.minMemoryGB} GB+ memory` : ''}
                </small>
                <p>{p.description}</p>
                <a href={p.source} target="_blank" rel="noreferrer">
                  Model details and license
                </a>
              </span>
            </label>
          ))}
      </div>
      <label className="check">
        <input
          type="checkbox"
          checked={optional}
          onChange={(e) => {
            setOptional(e.target.checked)
            if (!e.target.checked && selected?.optional) setModel(recommended)
          }}
        />
        Show optional models and future capabilities
      </label>
      {selected && memory < selected.minMemoryGB && (
        <div className="warning">
          This model is above the suggested memory budget for the selected machine. Choose Lightweight
          or use a larger inference machine.
        </div>
      )}
      {model === 'qwen3.6:27b-coding' && shared && memory < 48 && (
        <div className="warning">
          Allow 48 GB when the Coding model shares the machine with CI or needs longer contexts.
        </div>
      )}
    </>
  )

  return (
    <Dialog
      title={current ? 'Manage local models · ' + current.name : 'Set up local AI'}
      close={close}
      wide
    >
      {!current ? (
        <Form
          close={close}
          label="Create connection and install model"
          submit={async (d) => {
            const c = await api<Connection>('/connections', 'POST', {
              name: val(d, 'name'),
              kind: 'ollama',
              endpoint: val(d, 'endpoint'),
              models: [model],
              contextLength: Number(val(d, 'contextLength')),
              keepAliveSeconds: shared ? 0 : 300,
            })
            setCurrent(c)
            changed()
            try {
              await start(c, model)
            } catch (err) {
              setError((err as Error).message)
            }
          }}
        >
          <div className="callout">
            The macOS runtime setup and Linux installer’s inference command can install Ollama or optional
            Herd routing. You can also start{' '}
            <a href="https://ollama.com/download/mac" target="_blank" rel="noreferrer">
              Ollama
            </a>{' '}
            on the inference machine first, or run <code>ollama serve</code> if you use its CLI.
            Sluicework installs models into that running runtime. A router must support Ollama
            model-management endpoints.
          </div>
          <Field label="Connection name">
            <input name="name" defaultValue="Local AI" required maxLength={128} />
          </Field>
          <Field
            label="Ollama endpoint"
            hint="Loopback means the machine running the Sluicework server. Use trusted HTTPS for another host."
          >
            <input name="endpoint" type="url" defaultValue="http://127.0.0.1:11434" required />
          </Field>
          {choices}
          <Field
            label="Context length (tokens)"
            hint="Start at 4096 on a shared machine. Longer contexts consume more memory."
          >
            <select name="contextLength" defaultValue="4096">
              {[2048, 4096, 8192, 16384, 32768].map((n) => (
                <option key={n}>{n}</option>
              ))}
            </select>
          </Field>
          <p className="minor">
            {shared
              ? 'Release model memory after each request to leave room for CI.'
              : 'Keep the model loaded for five minutes between requests.'}{' '}
            Curated models start with thinking disabled for direct answers. You can change these
            settings in Edit connection.
          </p>
          <p>
            Installation downloads several GB from Ollama’s registry onto the endpoint machine. Existing
            model files stay in place. Check free disk space on that machine before starting.
          </p>
        </Form>
      ) : (
        <>
          <div className="form-body">
            <div className="callout">
              Models download onto <b>{current.endpoint}</b>. Closing this dialog keeps the download
              running. Reopen Manage models to follow it.
            </div>
            {error && <ErrorMessage>{error}</ErrorMessage>}
            {loadError && <ErrorMessage>{loadError}</ErrorMessage>}
            {inventory?.runtimeError && (
              <ErrorMessage>
                {inventory.runtimeError}{' '}
                <a href="https://ollama.com/download/mac" target="_blank" rel="noreferrer">
                  Install and start Ollama
                </a>
              </ErrorMessage>
            )}
            {!inventory && !error && <p role="status">Checking installed models…</p>}
            {!!inventory?.installations.length && (
              <section className="model-downloads" aria-label="Model downloads">
                {inventory.installations.slice(0, 5).map((i) => (
                  <div key={i.id} className="model-download">
                    <div className="actions">
                      <b>{i.model}</b>
                      <Badge value={i.status} />
                    </div>
                    <p role={i.status === 'running' ? 'status' : undefined}>{i.message}</p>
                    {i.status === 'running' && (
                      <>
                        <progress
                          aria-label={'Download progress for ' + i.model}
                          max={i.total || undefined}
                          value={i.total ? i.completed : undefined}
                        />
                        {i.total > 0 && (
                          <small>
                            Current layer: {bytes(i.completed)} / {bytes(i.total)}
                          </small>
                        )}
                        <button
                          disabled={busy}
                          onClick={async () => {
                            setBusy(true)
                            try {
                              await api(
                                idPath('/connections', current.id) +
                                  '/models/' +
                                  encodeURIComponent(i.id) +
                                  '/cancel',
                                'POST',
                              )
                              bump((n) => n + 1)
                            } catch (err) {
                              setError((err as Error).message)
                            } finally {
                              setBusy(false)
                            }
                          }}
                        >
                          Cancel download
                        </button>
                      </>
                    )}
                  </div>
                ))}
              </section>
            )}
            {!!inventory?.installed.length && (
              <section>
                <h3>Installed on this endpoint</h3>
                <ul>
                  {inventory.installed.map((m) => (
                    <li key={m.name}>
                      <code>{m.name}</code> · {bytes(m.size)}
                    </li>
                  ))}
                </ul>
              </section>
            )}
            {choices}
            <button
              className="primary"
              disabled={!inventory || !!running || busy || !selected?.installable}
              onClick={async () => {
                setBusy(true)
                setError('')
                try {
                  await start(current, model)
                } catch (err) {
                  setError((err as Error).message)
                } finally {
                  setBusy(false)
                }
              }}
            >
              <Download size={15} />
              {installed ? 'Update and permit model' : 'Install selected model'}
            </button>
            <p className="minor">
              A successful download verifies the model and adds it to this connection’s permitted
              models. Use it in an approved AI job template. Embeddings are deferred until retrieval
              is available.
            </p>
          </div>
          <footer className="dialog-foot">
            <button onClick={() => bump((n) => n + 1)}>
              <RefreshCw size={14} /> Refresh
            </button>
            <button onClick={close}>Done</button>
          </footer>
        </>
      )}
    </Dialog>
  )
}
