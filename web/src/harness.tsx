import { useEffect, useState } from 'react'
import { Download, KeyRound, RefreshCw } from 'lucide-react'
import { api } from './api'
import type { Connection, Page } from './model'
import { ErrorMessage, Field, Form } from './ui'

type Policy = { enabled: boolean; maxConcurrent: number }
type Client = { id: string; name: string; connectionId: string; models: string[]; contextLength: number; maxOutputTokens: number; maxConcurrent: number; timeoutSeconds: number; created: string; expires: string; revoked: boolean }
type Call = { id: string; clientName: string; model: string; started: string; durationMs: number; status: string; error?: string; inputTokens?: number; outputTokens?: number }
type Data = { policy: Policy; clients: Client[]; calls: Call[] }
type Receipt = { client: { version: number; serverUrl: string; token: string; models: string[]; caPem?: string; contextLength: number; maxOutputTokens: number; expires: string }; caFingerprint: string }
export function CodingClients({ connections, go }: { connections: Connection[]; go: (page: Page) => void }) {
  const [data, setData] = useState<Data>()
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [enabled, setEnabled] = useState(false)
  const [slots, setSlots] = useState(1)
  const [serverURL, setServerURL] = useState('')
  const [connectionID, setConnectionID] = useState('')
  const [models, setModels] = useState<string[]>([])
  const [receipt, setReceipt] = useState<Receipt>()
  const local = connections.filter(c => c.kind === 'ollama' && !c.disabled)
  const selected = local.find(c => c.id === connectionID)
  const load = async () => {
    try {
      const d = await api<Data>('/harness'); setData(d); setEnabled(d.policy.enabled); setSlots(d.policy.maxConcurrent)
    } catch (e) { setError((e as Error).message) }
  }
  useEffect(() => { void load(); void api<{ network: { url?: string } }>('/settings').then(s => { if (s.network.url) setServerURL(s.network.url) }).catch(() => {}) }, [])
  const download = () => {
    if (!receipt) return
    const url = URL.createObjectURL(new Blob([JSON.stringify(receipt.client, null, 2) + '\n'], { type: 'application/json' }))
    const a = document.createElement('a'); a.href = url; a.download = 'sluicework-codex.json'; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  return <section className="panel setup-panel coding-clients">
    <div className="panel-head"><h2><KeyRound size={19} /> Coding clients</h2><button onClick={() => void load()}><RefreshCw size={14} /> Refresh clients</button></div>
    <p>Run Codex on any Mac or Linux workstation through this Sluicework server. Project files and commands stay on the client. Prompts and tool results travel to the selected model host through the gateway.</p>
    {error && <ErrorMessage>{error}</ErrorMessage>}
    {notice && <p role="status" className="callout">{notice}</p>}
    <details><summary>Manage gateway and clients</summary>
    {data && <Form label="Save gateway policy" cancelLabel="Reset" close={() => { setEnabled(data.policy.enabled); setSlots(data.policy.maxConcurrent) }} submit={async () => {
      await api('/harness/policy', 'PUT', { enabled, maxConcurrent: slots }); setNotice('Gateway policy saved.'); await load()
    }}>
      <label className="check"><input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)} /> Enable authenticated coding gateway</label>
      <Field label="Shared local inference slots" hint="Scheduled AI requests and coding clients share this limit across Sluicework. Direct Ollama and Open WebUI requests have their own controls.">
        <select value={slots} onChange={e => setSlots(Number(e.target.value))}>{[1, 2, 3, 4].map(n => <option key={n} value={n}>{n}</option>)}</select>
      </Field>
      <p className="minor">Pausing the coding gateway cancels its active requests and rejects new ones. Scheduled AI requests continue using the shared slot limit.</p>
    </Form>}
    {receipt && <div className="callout" role="status">
      <h3>Client credential created</h3>
      <p>Download this private file now. Sluicework stores only its hash and cannot display the credential again. Keep the file on the coding workstation, outside the repository.</p>
      <button onClick={download}><Download size={14} /> Download Codex client file</button>
      <pre><code>{`chmod 600 ./sluicework-codex.json\nsluicework codex --client ./sluicework-codex.json${receipt.caFingerprint ? ` --trust-ca ${receipt.caFingerprint}` : ''} --cwd /path/to/project`}</code></pre>
      {receipt.caFingerprint && <p>Compare this cluster CA SHA-256 fingerprint with your trusted admin before accepting it on another machine: <code>{receipt.caFingerprint}</code></p>}
      <p>The remote server is <b>{receipt.client.serverUrl}</b>. Install Codex CLI and the Sluicework executable on the coding client. No Ollama installation is required there.</p>
      <button onClick={() => setReceipt(undefined)}>Dismiss credential</button>
    </div>}
    <h3>Authorize a coding client</h3>
    {!local.length ? <p>Create an enabled Ollama or Herd AI connection first.</p> : <Form label="Create coding client" cancelLabel="Clear" close={() => { setConnectionID(''); setModels([]) }} submit={async d => {
      if (!models.length) throw new Error('Select at least one permitted model')
      const r = await api<Receipt>('/harness/clients', 'POST', { name: String(d.get('name')), serverUrl: serverURL, connectionId: connectionID, models, contextLength: Number(d.get('contextLength')), maxOutputTokens: Number(d.get('maxOutputTokens')), maxConcurrent: Number(d.get('maxConcurrent')), timeoutSeconds: Number(d.get('timeoutSeconds')), lifetimeDays: Number(d.get('lifetimeDays')) })
      setReceipt(r); setNotice('Client created. Download its private configuration before leaving this page.'); await load()
    }}>
      <Field label="Coding client name"><input name="name" required maxLength={128} placeholder="Developer laptop" /></Field>
      <Field label="Sluicework server URL" hint="Use the HTTPS address reachable from the coding workstation. This is the orchestrator, which may be separate from both the client and model host.">
        <input value={serverURL} onChange={e => setServerURL(e.target.value)} required placeholder="https://sluicework.example:8443" />
      </Field>
      <button type="button" onClick={() => go('Settings')}>Configure server HTTPS in Settings</button>
      <Field label="Model connection"><select required value={connectionID} onChange={e => { setConnectionID(e.target.value); setModels([]) }}><option value="">Select a local inference connection</option>{local.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
      {selected && <fieldset><legend>Permitted models</legend>{selected.models.map(m => <label className="check" key={m}><input type="checkbox" checked={models.includes(m)} onChange={e => setModels(e.target.checked ? [...models, m] : models.filter(v => v !== m))} />{m}</label>)}</fieldset>}
      <Field label="Coding context tokens" hint="This server-controlled coding limit is separate from the connection's scheduled-summary context. Larger contexts consume more model memory."><input name="contextLength" type="number" min={4096} max={32768} step={1024} defaultValue={16384} required /></Field>
      <Field label="Maximum output tokens per request"><input name="maxOutputTokens" type="number" min={128} max={8192} defaultValue={2048} required /></Field>
      <Field label="Concurrent requests for this client"><input name="maxConcurrent" type="number" min={1} max={4} defaultValue={1} required /></Field>
      <Field label="Request timeout (seconds)" hint="30 minutes by default, per model request. A longer deadline permits more work; it does not increase model context or output limits."><input name="timeoutSeconds" type="number" min={15} max={1800} defaultValue={1800} required /></Field>
      <Field label="Credential lifetime (days)"><input name="lifetimeDays" type="number" min={1} max={90} defaultValue={7} required /></Field>
    </Form>}
    {!!data?.clients.length && <><h3>Authorized clients</h3><div className="table-wrap"><table><thead><tr><th>Client</th><th>Models</th><th>Limits</th><th>Timeout (seconds)</th><th>Expires</th><th>Access</th></tr></thead><tbody>{data.clients.map(c => <tr key={c.id}><td>{c.name}</td><td>{c.models.join(', ')}</td><td>{c.contextLength.toLocaleString()} context · {c.maxConcurrent} concurrent</td><td>{c.revoked ? `${c.timeoutSeconds}s` : <form key={c.timeoutSeconds} onSubmit={async e => { e.preventDefault(); const seconds = Number(new FormData(e.currentTarget).get('timeoutSeconds')); try { await api(`/harness/clients/${encodeURIComponent(c.id)}/timeout`, 'PUT', { timeoutSeconds: seconds }); setNotice('Timeout saved for subsequent requests. Credential and expiry are unchanged.'); await load() } catch (e) { setError((e as Error).message) } }}><input aria-label={`Request timeout in seconds for ${c.name}`} style={{width:'7rem'}} name="timeoutSeconds" type="number" min={15} max={1800} defaultValue={c.timeoutSeconds} required /><button>Save timeout</button></form>}</td><td>{new Date(c.expires).toLocaleString()}</td><td>{c.revoked ? 'Revoked' : <button onClick={async () => { try { await api(`/harness/clients/${encodeURIComponent(c.id)}/revoke`, 'POST'); setNotice('Client revoked. Active requests are cancelled.'); await load() } catch (e) { setError((e as Error).message) } }}>Revoke client</button>}</td></tr>)}</tbody></table></div></>}
    {!!data?.calls.length && <><h3>Recent coding requests</h3><p className="minor">Metadata only. Prompts, file contents, tool results and response text are not stored by the gateway.</p><div className="table-wrap"><table><thead><tr><th>Client</th><th>Model</th><th>Status</th><th>Duration</th><th>Tokens in / out</th></tr></thead><tbody>{data.calls.map(c => <tr key={c.id}><td>{c.clientName}</td><td>{c.model}</td><td>{c.status}{c.error && <div className="minor">{c.error}</div>}</td><td>{(c.durationMs / 1000).toFixed(1)}s</td><td>{c.inputTokens ?? '—'} / {c.outputTokens ?? '—'}</td></tr>)}</tbody></table></div></>}
    </details>
  </section>
}
