import { Fragment, useEffect, useState } from 'react'
import { api, idPath } from './api'
import { Dialog, ErrorMessage, Field, Form, Panel, checked, val } from './ui'

type Manifest = { name: string; prompt: string; model: string; cron: string; timezone: string; expires: string; maxRunsPerDay: number; timeoutSeconds: number; outputLimit: number; shareResults: boolean; webSearch?: { allowedDomains?: string[] }; desktopNotifications?: boolean }
type Proposal = { id: string; revision: number; owner: string; runtimeId: string; manifest: Manifest }
type Citation = { url: string; title: string; start: number; end: number }
type PersonalRun = { id: string; scheduleId: string; slot: string; started: string; state: string; message?: string; result?: string; citations?: Citation[]; webSearched?: boolean; notification?: string }
type Runtime = { id: string; name: string; owner: string; lastSeen: string; report: { paused?: boolean; signedIn?: boolean; models?: string[]; approvals?: { id: string; digest: string; paused: boolean; revision: number; cron: string; timezone: string; model: string; expires: string; webSearch?: { allowedDomains?: string[] }; desktopNotifications?: boolean }[]; runs?: PersonalRun[] } }
type Ticket = { id: string; code: string; expires: string }
const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'"

export function PersonalAgents() {
  const [runtimes, setRuntimes] = useState<Runtime[]>([])
  const [proposals, setProposals] = useState<Proposal[]>([])
  const [owner, setOwner] = useState('')
  const [ticket, setTicket] = useState<Ticket>()
  const [error, setError] = useState('')
  const [editor, setEditor] = useState<{ runtime: Runtime; proposal?: Proposal }>()
  const [preparing, setPreparing] = useState(false)
  const load = async () => {
    try {
      const [agents, tasks, session] = await Promise.all([api<Runtime[]>('/personal-agents'), api<Proposal[]>('/personal-tasks'), api<{ username: string }>('/session')])
      setRuntimes(agents); setProposals(tasks); setOwner(session.username); setError('')
    } catch (err) { setError((err as Error).message) }
  }
  useEffect(() => { void load(); const timer = window.setInterval(() => void load(), 15000); return () => clearInterval(timer) }, [])
  return <>
    <Panel title="Personal scheduled tasks" className="personal-agents">
      <div className="personal-body">
        <p>Run recurring prompts with your own ChatGPT subscription in your signed-in OS user’s private agent. This shared server stores proposals and receives signed status reports; it never receives subscription credentials or controls the agent.</p>
        <p>Only your local runtime can sign in, approve tasks, pause, resume, revoke approvals or stop. Another cluster user or administrator cannot perform those controls here. OpenAI deployment eligibility and your plan limits still apply.</p>
        <p>Use an OS account that does not execute other users’ worker or CI jobs. Programs sharing your OS identity have the same local authority; put shared CI in another account or VM.</p>
        {error && <ErrorMessage>{error}</ErrorMessage>}
        <button disabled={preparing} onClick={() => {
          setPreparing(true)
          void api<Ticket>('/personal-agents/tickets', 'POST').then(setTicket).catch((err) => setError(err.message)).finally(() => setPreparing(false))
        }}>Prepare personal agent setup</button>
        {ticket && <div className="callout">
          <p>In your own interactive terminal on the computer that will run the agent:</p>
          <pre>{`sluicework personal-agent enroll --server ${quote(window.location.origin)} --owner ${quote(owner)} --ticket ${quote(ticket.id)}`}</pre>
          <Field label="One-use enrollment code" hint="Paste at the hidden terminal prompt. Expires in ten minutes; keep it out of command history."><input readOnly type="password" value={ticket.code} autoComplete="off" onFocus={(e) => { e.target.type = 'text'; e.target.select() }} onBlur={(e) => { e.target.type = 'password' }} /></Field>
          <p>Use <code>--ca /path/to/verified-cluster-ca.pem</code> for a private CA. Then start the agent without sudo, and sign in from a second terminal:</p>
          <pre>{'sluicework personal-agent start\nsluicework personal-agent login'}</pre>
          <p>For login startup, optionally run <code>sluicework personal-agent install-service</code> instead of keeping a foreground terminal open. The agent starts paused. Prepare a task below, approve it locally, then use <code>sluicework personal-agent resume</code>.</p>
          <button onClick={() => setTicket(undefined)}>Hide setup code</button>
        </div>}
        {!runtimes.length && <p>No personal agent enrolled for your cluster user yet.</p>}
        {runtimes.map((runtime) => {
          const online = new Date(runtime.lastSeen).getTime() > Date.now() - 180000
          return <article key={runtime.id} className="personal-runtime">
            <div className="panel-head"><h3>{runtime.name}</h3><span>{!online ? 'Offline / not started' : runtime.report.paused ? 'Paused locally' : runtime.report.signedIn ? 'Ready for approved tasks' : 'Sign in locally'}</span></div>
            <p>{runtime.owner} · Private OS-user runtime · One task at a time</p>
            <small>Public runtime fingerprint: <code>{runtime.id}</code></small>
            <div className="actions"><button onClick={() => setEditor({ runtime })}>Prepare scheduled task</button><code>sluicework personal-agent status</code></div>
            <p>Local controls: <code>personal-agent pause</code>, <code>resume</code>, <code>sign-out</code> and <code>stop</code>. Tasks use the runtime’s clock. Sleep/offline time is skipped, with no catch-up burst.</p>
            {proposals.filter((p) => p.runtimeId === runtime.id).map((proposal) => {
              const approved = runtime.report.approvals?.find((item) => item.id === proposal.id)
              const expired = new Date(approved?.expires || proposal.manifest.expires).getTime() <= Date.now()
              return <div key={proposal.id} className="personal-task">
                <h4>{proposal.manifest.name}</h4>
                <p><code>{proposal.manifest.cron}</code> · {proposal.manifest.timezone} · {proposal.manifest.model} · Up to {proposal.manifest.maxRunsPerDay} runs/day</p>
                <p>{expired ? 'Approval/proposal expired' : approved ? `Locally approved revision ${approved.revision}: ${approved.cron} (${approved.timezone}), ${approved.model}. ${approved.revision === proposal.revision ? 'This proposal is locally approved.' : `Proposal revision ${proposal.revision} awaits fresh local approval; the previous version remains in force.`}` : 'Awaiting your local approval'}</p>
                <p>Proposed: {proposal.manifest.webSearch ? `Public web research${proposal.manifest.webSearch.allowedDomains?.length ? ` on ${proposal.manifest.webSearch.allowedDomains.join(', ')} (including subdomains)` : ' across public websites'}` : 'Web access disabled'} · Desktop notifications {proposal.manifest.desktopNotifications ? 'requested' : 'off'}</p>
                {approved && <p>Locally approved permissions: web research {approved.webSearch ? 'enabled' : 'off'} · Desktop notifications {approved.desktopNotifications ? 'enabled' : 'off'}</p>}
                <pre>{`sluicework personal-agent approve ${quote(proposal.id)}`}</pre>
                <div className="actions"><button onClick={() => setEditor({ runtime, proposal })}>Edit proposal</button><code>{`personal-agent revoke ${quote(proposal.id)}`}</code></div>
                <small>Saving here never activates or modifies a locally approved task.</small>
              </div>
            })}
            {!!runtime.report.runs?.length && <><h4>Recent task activity</h4>{runtime.report.runs.map((run) => <div key={run.id} className="personal-task"><p>{proposals.find((p) => p.id === run.scheduleId)?.manifest.name || run.scheduleId} · {run.slot} · {run.state}{run.message ? ` · ${run.message}` : ''}</p><p>{run.webSearched ? 'Web research completed · ' : ''}Desktop notification: {notificationLabel(run.notification)}</p>{run.result ? <details><summary>Report shared by your local approval</summary><PersonalReport text={run.result} citations={run.citations || []} /></details> : <small>Result stays in your runtime. Use <code>personal-agent results</code> locally.</small>}</div>)}</>}
          </article>
        })}
      </div>
    </Panel>
    {editor && <PersonalTaskEditor runtime={editor.runtime} proposal={editor.proposal} close={() => setEditor(undefined)} saved={() => { setEditor(undefined); void load() }} />}
  </>
}
function PersonalTaskEditor({ runtime, proposal, close, saved }: { runtime: Runtime; proposal?: Proposal; close: () => void; saved: () => void }) {
  const m = proposal?.manifest
  return <Dialog title={proposal ? 'Edit personal task proposal' : 'Prepare a personal scheduled task'} close={close} wide>
    <Form label="Save proposal for local review" close={close} submit={async (data) => {
      const manifest: Manifest = { name: val(data, 'name'), prompt: val(data, 'prompt'), model: val(data, 'model'), cron: val(data, 'cron'), timezone: val(data, 'timezone'), expires: new Date(val(data, 'expires') + 'T23:59:59Z').toISOString(), maxRunsPerDay: Number(val(data, 'cap')), timeoutSeconds: Number(val(data, 'timeout')), outputLimit: Number(val(data, 'output')), shareResults: checked(data, 'share'), ...(checked(data, 'web') ? { webSearch: { allowedDomains: val(data, 'domains').split(',').map((d) => d.trim().toLowerCase()).filter(Boolean) } } : {}), ...(checked(data, 'notify') ? { desktopNotifications: true } : {}) }
      await api(proposal ? idPath('/personal-tasks', proposal.id) : '/personal-tasks', proposal ? 'PUT' : 'POST', { runtimeId: runtime.id, manifest }); saved()
    }}>
      <p>This is an inert proposal for {runtime.owner}. Review and approve the exact task and account in your private runtime before background execution. Optionally authorize public web research and status-only desktop notifications. This does not execute shell commands or access signed-in websites.</p>
      <Field label="Task name"><input name="name" required maxLength={128} defaultValue={m?.name} /></Field>
      <Field label="Recurring prompt"><textarea name="prompt" rows={5} required maxLength={65536} defaultValue={m?.prompt} /></Field>
      <Field label="Model ID" hint={runtime.report.models?.length ? `Available in your personal runtime: ${runtime.report.models.join(', ')}` : 'Sign in with personal-agent login and use personal-agent models to list your account’s models.'}><input name="model" required defaultValue={m?.model || runtime.report.models?.[0] || ''} /></Field>
      <div className="form-grid">
        <Field label="Cron schedule" hint="Five fields. Example: 0 6 * * * for daily at 6am."><input name="cron" required defaultValue={m?.cron || '0 6 * * *'} /></Field>
        <Field label="Time zone"><input name="timezone" required defaultValue={m?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'} /></Field>
        <Field label="Approval expires (UTC date)"><input name="expires" type="date" required defaultValue={(m?.expires || new Date(Date.now() + 90 * 86400000).toISOString()).slice(0, 10)} /></Field>
        <Field label="Maximum runs per local day"><input name="cap" type="number" min={1} max={24} required defaultValue={m?.maxRunsPerDay || 1} /></Field>
        <Field label="Request deadline (seconds)"><input name="timeout" type="number" min={15} max={1800} required defaultValue={m?.timeoutSeconds || 300} /></Field>
        <Field label="Saved result limit (bytes)"><input name="output" type="number" min={128} max={65536} required defaultValue={m?.outputLimit || 32768} /></Field>
      </div>
      <fieldset><legend>Optional task permissions</legend>
        <label className="check"><input name="web" type="checkbox" defaultChecked={!!m?.webSearch} />Allow public web research with source citations</label>
        <Field label="Allowed web domains (optional)" hint="Comma-separated lowercase domains, for example openai.com, github.com. Includes subdomains. Leaving this blank allows public websites. Used only when web research is enabled; up to 20 domains."><input name="domains" defaultValue={m?.webSearch?.allowedDomains?.join(', ') || ''} /></Field>
        <p>Search queries and the approved prompt go to OpenAI. Search requires support from your selected model and account; failure never falls back to API billing. No browser cookies, private pages, downloads or local tools are available.</p>
        <label className="check"><input name="notify" type="checkbox" defaultChecked={m?.desktopNotifications || false} />Notify my runtime’s desktop after each success or failure</label>
        <p>Alerts contain status only, without the task name, prompt or report. They appear on the computer running your agent. A logged-in desktop and OS notification permission are required (Linux also needs notify-send). Headless runtimes keep status here.</p>
      </fieldset>
      <label className="check"><input name="share" type="checkbox" defaultChecked={m?.shareResults || false} />Save generated reports in the shared Sluicework server for my console account. Otherwise, upload status only.</label>
      <p>Local approval binds the prompt, model, schedule, deadline, limits, account, browsing and notification permissions, and sharing choice. ChatGPT plan usage has no per-request token cap; manage allowance at <a href="https://chatgpt.com/settings/usage" target="_blank" rel="noreferrer">ChatGPT Usage</a>. Pausing, signing out or changing approval requires your local OS login.</p>
    </Form>
  </Dialog>
}

function notificationLabel(value?: string) {
  switch (value) {
    case 'submitted': return 'submitted to the runtime desktop (display depends on OS settings)'
    case 'failed': return 'not submitted; check the runtime desktop session and notification permissions'
    case 'unknown': return 'outcome unknown after restart; no duplicate alert sent'
    case 'pending': case 'attempting': return 'submitting to the runtime desktop'
    default: return 'not requested'
  }
}
function safeLink(raw: string) {
  try { const u = new URL(raw); return ['http:', 'https:'].includes(u.protocol) && !u.username && !u.password } catch { return false }
}
function PersonalReport({ text, citations }: { text: string; citations: Citation[] }) {
  const chars = Array.from(text)
  let offset = 0
  const parts = citations.filter((c) => Number.isInteger(c.start) && Number.isInteger(c.end) && c.start >= 0 && c.end > c.start && c.end <= chars.length && safeLink(c.url)).sort((a, b) => a.start - b.start).map((c, i) => {
    if (c.start < offset) return null
    const before = chars.slice(offset, c.start).join(''); offset = c.end
    return <Fragment key={i}>{before}<a href={c.url} title={c.title} target="_blank" rel="noreferrer">{chars.slice(c.start, c.end).join('')}</a></Fragment>
  })
  return <pre>{parts}{chars.slice(offset).join('')}</pre>
}
