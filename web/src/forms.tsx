import { useEffect, useState } from 'react'
import { cloudSetup } from './provider-info'
import type { PlanModel } from './provider-auth'
import { Plus, Trash2, ExternalLink } from 'lucide-react'
import { api, idPath } from './api'
import { kindNames, lines, split, platformName, matchesJob, runnerLabels } from './model'
import { WorkstationFields, WorkstationSummary, settingsFromForm } from './workstation'
import type { Job, Input, Connection, Machine, Schedule, Run, Occurrence } from './model'
import { Dialog, Form, Field, checked, val } from './ui'
import { when } from './model'
type Props = { close: () => void; saved: () => void }
function InputValues({ job, initial }: { job: Job; initial?: Record<string, string> }) {
  return (job.inputs || []).map((input) => (
    <Field
      key={input.name}
      label={input.name}
      hint={input.paths?.length ? 'Read-only file on the assigned machine.' : undefined}
    >
      {input.paths?.length ? (
        <select
          name={'input:' + input.name}
          required={input.required}
          defaultValue={initial?.[input.name] || ''}
        >
          <option value="">Select a file</option>
          {input.paths.map((path) => (
            <option key={path}>{path}</option>
          ))}
        </select>
      ) : (
        <textarea
          name={'input:' + input.name}
          rows={3}
          defaultValue={initial?.[input.name]}
          required={input.required}
        />
      )}
    </Field>
  ))
}
const inputValues = (d: FormData, j: Job) =>
  Object.fromEntries(
    (j.inputs || [])
      .filter((i) => d.get('input:' + i.name) !== '')
      .map((i) => [i.name, String(d.get('input:' + i.name) ?? '')]),
  )
export function Launch({
  jobs,
  machines,
  initial,
  close,
  saved,
  openRun,
}: Props & { jobs: Job[]; machines: Machine[]; initial?: Job; openRun: (id: string) => void }) {
  const [id, setId] = useState(initial?.id || jobs[0]?.id || '')
  const job = jobs.find((j) => j.id === id)
  return (
    <Dialog title="Run a job" close={close}>
      <Form
        close={close}
        label="Queue run"
        submit={async (d) => {
          if (!job) throw Error('Select an approved template.')
          const r = await api<Run>(idPath('/jobs', id) + '/runs', 'POST', {
            inputs: inputValues(d, job),
          })
          saved()
          openRun(r.id)
        }}
      >
        <Field label="Approved template">
          <select required value={id} onChange={(e) => setId(e.target.value)}>
            {jobs.map((j) => (
              <option key={j.id} value={j.id}>
                {j.name} · revision {j.revision}
              </option>
            ))}
          </select>
        </Field>
        {job && (
          <>
            <div className="callout">
              Target role <b>{job.role}</b>
              {!!job.labels?.length && <> · {job.labels.join(', ')}</>}
              <p>{platformName(job)}</p>
              <p>Matching machines: {machines.filter((m) => matchesJob(m, job) && m.online && !m.draining).map((m) => m.name).join(', ') || 'None currently online'}. The run waits for a matching worker; it never falls back to another platform.</p>
            </div>
            <div className="field-stack" key={id}>
              <InputValues job={job} />
            </div>
          </>
        )}
      </Form>
    </Dialog>
  )
}
export function JobEditor({
  job,
  connections,
  close,
  saved,
}: Props & { job?: Job; connections: Connection[] }) {
  const [type, setType] = useState<Job['type']>(job?.type || 'script')
  const [conn, setConn] = useState(job?.connection || '')
  const [inputs, setInputs] = useState<Input[]>((job?.inputs || []).map((i) => ({ ...i })))
  const [safe, setSafe] = useState(job?.safeToRepeat || false)
  const [prompt, setPrompt] = useState(job?.prompt || '')
  const connection = connections.find((c) => c.id === conn)
  const editInput = (n: number, key: keyof Input, v: string | boolean | string[]) =>
    setInputs((items) => items.map((item, index) => (n === index ? { ...item, [key]: v } : item)))
  return (
    <Dialog title={job ? 'Edit job template' : 'Create job template'} close={close} wide>
      <Form
        close={close}
        label={job ? 'Approve revision' : 'Approve template'}
        submit={async (d) => {
          const body = {
            name: val(d, 'name'),
            type,
            role: val(d, 'role'),
            labels: split(val(d, 'labels')),
            os: val(d, 'os') === 'any' ? '' : val(d, 'os'),
            arch: val(d, 'arch') === 'any' ? '' : val(d, 'arch'),
            inputs: inputs.map((i) => ({
              ...i,
              name: i.name.trim(),
              paths: i.paths?.map((p) => p.trim()).filter(Boolean),
            })),
            timeoutSeconds: Number(val(d, 'timeout')),
            outputLimit: Number(val(d, 'limit')),
            discardOutput: checked(d, 'discard'),
            safeToRepeat: safe,
            maxAutoRetries: safe ? Number(val(d, 'retries')) : 0,
            ...(type === 'script'
              ? {
                  command: val(d, 'command'),
                  args: lines(String(d.get('args') || '')),
                  workDir: val(d, 'workDir'),
                }
              : {
                  prompt: String(d.get('prompt') || ''),
                  connection: conn,
                  model: val(d, 'model'),
                  allowCloud: checked(d, 'cloud'),
                  outputFormat: val(d, 'format'),
                  maxTokens: connection?.kind === 'chatgpt' ? (job?.maxTokens || 1024) : Number(val(d, 'tokens')),
                }),
          }
          await api(job ? idPath('/jobs', job.id) : '/jobs', job ? 'PUT' : 'POST', body)
          saved()
        }}
      >
        <Field label="Template name">
          <input name="name" required defaultValue={job?.name} />
        </Field>
        <div className="form-grid">
          <Field label="Job type">
            <select value={type} onChange={(e) => setType(e.target.value as Job['type'])}>
              <option value="script">Script</option>
              <option value="ai">AI task</option>
            </select>
          </Field>
          <Field label="Worker role">
            <input
              name="role"
              key={type}
              defaultValue={job?.type === type ? job.role : type === 'ai' ? 'agent' : 'script'}
              required
            />
          </Field>
        </div>
        <Field label="Required machine labels" hint="Separate labels with commas.">
          <input name="labels" defaultValue={job?.labels?.join(', ')} />
        </Field>
        <div className="form-grid">
          <Field label="Operating system" hint="Placement is restricted to this platform.">
            <select name="os" required defaultValue={job ? job.os || 'any' : ''}>
              <option value="" disabled>Choose an operating system</option>
              <option value="linux">Linux</option>
              <option value="darwin">macOS</option>
              <option value="any">Either — portable job</option>
            </select>
          </Field>
          <Field label="CPU architecture">
            <select name="arch" required defaultValue={job ? job.arch || 'any' : ''}>
              <option value="" disabled>Choose an architecture</option>
              <option value="arm64">ARM64</option>
              <option value="amd64">x86-64</option>
              <option value="any">Either — portable job</option>
            </select>
          </Field>
        </div>
        {type === 'script' ? (
          <>
            <Field label="Executable" hint="Use the absolute path to the approved executable.">
              <input
                className="mono"
                name="command"
                placeholder="/usr/bin/uptime"
                defaultValue={job?.command}
                required
              />
            </Field>
            <Field
              label="Arguments"
              hint="One argument per line. Use {{name}} for a declared input."
            >
              <textarea
                className="mono"
                name="args"
                rows={3}
                defaultValue={job?.args?.join('\n')}
              />
            </Field>
            <Field
              label="Working directory"
              hint="Relative to the fresh directory created for each attempt."
            >
              <input name="workDir" defaultValue={job?.workDir || '.'} />
            </Field>
          </>
        ) : (
          <>
            {!job && (
              <button
                type="button"
                onClick={() => {
                  setPrompt(
                    'Summarize the test failures below. Group repeated failures, name the failing tests and distinguish observed errors from suspected causes.\n\n{{report}}',
                  )
                  setInputs([
                    {
                      name: 'report',
                      required: true,
                      paths: ['/Library/Sluicework/inputs/failures.txt'],
                    },
                  ])
                }}
              >
                Use test failure summary template
              </button>
            )}
            <Field label="Prompt template" hint="Use {{name}} to insert a declared input.">
              <textarea
                name="prompt"
                rows={4}
                required
                value={prompt}
                onChange={(e) => setPrompt(e.target.value)}
              />
            </Field>
            <div className="form-grid">
              <Field label="AI connection">
                <select required value={conn} onChange={(e) => setConn(e.target.value)}>
                  <option value="">Select a connection</option>
                  {connections
                    .filter((c) => !c.disabled || c.id === conn)
                    .map((c) => (
                      <option key={c.id} value={c.id}>
                        {c.name}{c.owner ? ` (${c.owner} only)` : ''}
                      </option>
                    ))}
                </select>
              </Field>
              <Field label="Model">
                <select
                  name="model"
                  key={conn}
                  defaultValue={job?.connection === conn ? job?.model : ''}
                  required
                >
                  <option value="">Select a model</option>
                  {connection?.models.map((m) => <option key={m}>{m}</option>)}
                </select>
              </Field>
            </div>
            {connection?.kind === 'chatgpt' && <p className="callout">Using ChatGPT plan · <a href="https://chatgpt.com/settings/usage" target="_blank" rel="noreferrer">Manage usage</a>. Only {connection.owner} can launch this template.</p>}
            {connection && connection.kind !== 'ollama' && (
              <label className="check">
                <input name="cloud" type="checkbox" required defaultChecked={job?.allowCloud} />
                Allow this template to send inputs to {kindNames[connection.kind]}.
              </label>
            )}
            <div className="form-grid">
              <Field label="Result format">
                <select name="format" defaultValue={job?.outputFormat || 'text'}>
                  <option value="text">Text</option>
                  <option value="json">JSON</option>
                </select>
              </Field>
              <Field label="Maximum output tokens" hint={connection?.kind === 'chatgpt' ? 'ChatGPT plan usage does not support a per-request token cap. The job deadline and result size limit still apply; manage plan allowance in ChatGPT Settings.' : undefined}>
                <input
                  name="tokens"
                  disabled={connection?.kind === 'chatgpt'}
                  type="number"
                  min={1}
                  defaultValue={job?.maxTokens || 1024}
                  required
                />
              </Field>
            </div>
          </>
        )}
        <section className="subform">
          <div className="panel-head">
            <h3>Permitted inputs</h3>
            <button
              type="button"
              onClick={() =>
                setInputs((items) => [...items, { name: '', required: true, paths: [] }])
              }
            >
              <Plus size={14} />
              Add input
            </button>
          </div>
          {inputs.map((i, n) => (
            <div className="input-row" key={n}>
              <div className="inline-fields">
                <Field label="Input name">
                  <input
                    required
                    value={i.name}
                    onChange={(e) => editInput(n, 'name', e.target.value)}
                  />
                </Field>
                <label className="check">
                  <input
                    type="checkbox"
                    checked={i.required}
                    onChange={(e) => editInput(n, 'required', e.target.checked)}
                  />
                  Required
                </label>
                <button
                  className="icon"
                  type="button"
                  aria-label={'Remove input ' + (n + 1)}
                  onClick={() => setInputs((list) => list.filter((_, k) => k !== n))}
                >
                  <Trash2 size={15} />
                </button>
              </div>
              <Field
                label="Permitted file paths"
                hint="One absolute path per line. Leave empty for a text input."
              >
                <textarea
                  className="mono"
                  rows={2}
                  value={(i.paths || []).join('\n')}
                  onChange={(e) => editInput(n, 'paths', lines(e.target.value))}
                />
              </Field>
            </div>
          ))}
        </section>
        <div className="form-grid">
          <Field label="Timeout (seconds)">
            <input
              name="timeout"
              type="number"
              min={1}
              defaultValue={job?.timeoutSeconds || 300}
              required
            />
          </Field>
          <Field label="Output limit (bytes)">
            <input
              name="limit"
              type="number"
              min={1}
              defaultValue={job?.outputLimit || 65536}
              required
            />
          </Field>
        </div>
        <label className="check">
          <input type="checkbox" name="discard" defaultChecked={job?.discardOutput} />
          Discard the result after completion
        </label>
        <label className="check">
          <input
            type="checkbox"
            name="safe"
            checked={safe}
            onChange={(e) => setSafe(e.target.checked)}
          />
          Safe to repeat
        </label>
        {safe && (
          <Field label="Automatic retry limit" hint="Use zero to allow manual retries only.">
            <input
              name="retries"
              type="number"
              min={0}
              max={3}
              required
              defaultValue={job?.maxAutoRetries || 0}
            />
          </Field>
        )}
      </Form>
    </Dialog>
  )
}
export function ScheduleEditor({
  jobs,
  job: selected,
  schedule,
  close,
  saved,
}: Props & { jobs: Job[]; job?: Job; schedule?: Schedule }) {
  const [id, setId] = useState(schedule?.jobId || selected?.id || jobs[0]?.id || '')
  const job = jobs.find((j) => j.id === id)
  return (
    <Dialog title={schedule ? 'Edit schedule' : 'Schedule a job'} close={close}>
      <Form
        close={close}
        label="Save schedule"
        submit={async (d) => {
          if (!job) throw Error('Select a job template.')
          await api(
            schedule ? idPath('/schedules', schedule.id) : '/schedules',
            schedule ? 'PUT' : 'POST',
            {
              name: val(d, 'name'),
              jobId: id,
              cron: val(d, 'cron'),
              timezone: val(d, 'zone'),
              inputs: inputValues(d, job),
              enabled: checked(d, 'enabled'),
              allowOverlap: checked(d, 'overlap'),
            },
          )
          saved()
        }}
      >
        <Field label="Schedule name">
          <input name="name" required defaultValue={schedule?.name || selected?.name} />
        </Field>
        <Field label="Job template">
          <select value={id} required onChange={(e) => setId(e.target.value)}>
            {jobs.map((j) => (
              <option key={j.id} value={j.id}>
                {j.name}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Cron expression" hint="Minute, hour, day of month, month, day of week.">
          <input
            className="mono"
            name="cron"
            required
            defaultValue={schedule?.cron || '0 9 * * 1-5'}
          />
        </Field>
        <Field label="IANA time zone">
          <input
            name="zone"
            required
            defaultValue={
              schedule?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
            }
          />
        </Field>
        {job && (
          <div className="field-stack" key={id}>
            <InputValues job={job} initial={schedule?.inputs} />
          </div>
        )}
        <label className="check">
          <input name="enabled" type="checkbox" defaultChecked={schedule?.enabled ?? true} />
          Schedule enabled
        </label>
        <label className="check">
          <input name="overlap" type="checkbox" defaultChecked={schedule?.allowOverlap} />
          Allow overlapping runs
        </label>
      </Form>
    </Dialog>
  )
}
export function History({
  s,
  items,
  close,
  openRun,
}: {
  s: Schedule
  items: Occurrence[]
  close: () => void
  openRun: (id: string) => void
}) {
  return (
    <Dialog title={s.name + ' · occurrences'} close={close}>
      <div className="form-body">
        {items.length ? (
          items.map((o) => (
            <div className="occurrence" key={o.scheduledAt}>
              <div>
                <b>{o.localSlot}</b>
                <small>
                  {when(o.scheduledAt)} · {o.status.replaceAll('_', ' ')}
                </small>
                {o.details && <p>{o.details}</p>}
              </div>
              {o.runId && <button onClick={() => openRun(o.runId!)}>View run</button>}
            </div>
          ))
        ) : (
          <p>No recorded occurrences yet.</p>
        )}
      </div>
    </Dialog>
  )
}
export function ConnectionEditor({ item, initialKind, close, saved }: Props & { item?: Connection; initialKind?: string }) {
  const [kind, setKind] = useState(item?.kind || initialKind || 'ollama')
  const [planModels, setPlanModels] = useState<PlanModel[]>([])
  const [catalogError, setCatalogError] = useState('')
  useEffect(() => {
    if (item?.kind !== 'chatgpt' || item.disabled) return
    let active = true
    void api<PlanModel[]>(idPath('/connections', item.id) + '/chatgpt/models').then((items) => { if (active) setPlanModels(items) }).catch((err) => { if (active) setCatalogError(err.message) })
    return () => { active = false }
  }, [item?.id, item?.disabled])
  return (
    <Dialog title={item ? 'Edit AI connection' : 'Add AI connection'} close={close}>
      <Form
        close={close}
        label="Save connection"
        submit={async (d) => {
          const key = String(d.get('key') || '')
          await api(
            item ? idPath('/connections', item.id) : '/connections',
            item ? 'PUT' : 'POST',
            {
              name: val(d, 'name'),
              kind,
              endpoint: kind === 'ollama' ? val(d, 'endpoint') : '',
              models: split(val(d, 'models')),
              disabled: checked(d, 'disabled'),
              ...(kind === 'ollama'
                ? {
                    contextLength: Number(val(d, 'contextLength')),
                    keepAliveSeconds: Number(val(d, 'keepAliveSeconds')),
                    thinking: checked(d, 'thinking'),
                  }
                : {}),
              ...(key ? { api_key: key } : {}),
            },
          )
          saved()
        }}
      >
        <Field label="Connection name">
          <input name="name" required defaultValue={item?.name} />
        </Field>
        <Field label="Provider">
          <select value={kind} disabled={!!item} onChange={(e) => setKind(e.target.value)}>
            {Object.entries(kindNames).filter(([value]) => value !== 'chatgpt' || !!item).map(([value, label]) => (
              <option value={value} key={value}>
                {label}
              </option>
            ))}
          </select>
        </Field>
        {kind === 'ollama' ? (
          <Field
            label="Inference endpoint"
            hint="Use the base URL for Ollama or Herd. LAN endpoints require HTTPS and a certificate trusted by the orchestrator."
          >
            <input
              type="url"
              name="endpoint"
              required
              defaultValue={item?.endpoint || 'http://127.0.0.1:11434'}
            />
          </Field>
        ) : kind === 'chatgpt' ? (
          <p>Your own ChatGPT plan. Reconnect or sign out on the connection card. Sluicework never falls back to API-key billing.</p>
        ) : (
          <Field
            key={'key:' + kind}
            label={item ? 'Replacement API key' : 'Provider API key'}
            hint={
              item
                ? 'Leave blank to keep the existing key.'
                : 'The key is stored on the orchestrator.'
            }
          >
            <input type="password" name="key" autoComplete="new-password" required={!item} />
          </Field>
        )}
        {cloudSetup[kind] && <p>{cloudSetup[kind].hint} <a href={cloudSetup[kind].keys} target="_blank" rel="noreferrer">Get a {cloudSetup[kind].name} API key ↗</a> Keys are stored in private files on the orchestrator and never sent to workers.</p>}
        {kind === 'chatgpt' && <>
          {catalogError && <p className="error-text">{catalogError}</p>}
          {planModels.length > 0 && <p>Available to this account: {planModels.map((model) => <code key={model.id}>{model.id} </code>)}</p>}
        </>}
        <Field key={'models:' + kind} label="Permitted models" hint="Enter the exact model IDs, separated by commas.">
          <input name="models" required defaultValue={item?.models?.join(', ')} />
        </Field>
        {kind === 'ollama' && (
          <div className="form-grid">
            <Field
              label="Context length (tokens)"
              hint="0 uses the runtime default. Larger contexts require more memory."
            >
              <input
                name="contextLength"
                type="number"
                min={0}
                max={32768}
                defaultValue={item?.contextLength ?? 4096}
                required
              />
            </Field>
            <Field
              label="Keep loaded (seconds)"
              hint="0 releases model memory after each request; up to 300 seconds keeps it warm."
            >
              <input
                name="keepAliveSeconds"
                type="number"
                min={0}
                max={300}
                defaultValue={item?.keepAliveSeconds ?? 0}
                required
              />
            </Field>
          </div>
        )}
        {kind === 'ollama' && (
          <label className="check">
            <input type="checkbox" name="thinking" defaultChecked={item?.thinking} />
            Enable thinking on supported models (uses more time and output tokens)
          </label>
        )}
        <label className="check">
          <input type="checkbox" name="disabled" defaultChecked={item?.disabled} />
          Disable this connection
        </label>
      </Form>
    </Dialog>
  )
}
export function MachineEditor({
  machine: m,
  admin,
  close,
  saved,
}: Props & { machine: Machine; admin: boolean }) {
  return (
    <Dialog title={'Configure ' + m.name} close={close}>
      <Form
        close={close}
        label="Save capacity"
        submit={async (d) => {
          const roleLimits = Object.fromEntries(
            (m.roles || [])
              .filter((r) => val(d, 'role:' + r) !== '')
              .map((r) => [r, Number(val(d, 'role:' + r))]),
          )
          await api(idPath('/machines', m.id), 'PATCH', {
            ...(admin ? { labels: split(val(d, 'labels')) } : {}),
            slots: Number(val(d, 'slots')),
            roleLimits,
          })
          saved()
        }}
      >
        {admin && (
          <Field
            label="Machine labels"
            hint="Separate labels with commas. Templates can require one or more labels."
          >
            <input name="labels" defaultValue={m.labels?.join(', ')} />
          </Field>
        )}
        <Field label="Host-wide job slots">
          <input type="number" name="slots" defaultValue={m.slots} min={1} required />
        </Field>
        {(m.roles || []).map((r) => (
          <Field key={r} label={r + ' role limit'} hint="Leave blank to use the host-wide cap.">
            <input
              type="number"
              min={0}
              name={'role:' + r}
              defaultValue={m.roleLimits?.[r] ?? ''}
            />
          </Field>
        ))}
      </Form>
    </Dialog>
  )
}
export function RunnerEditor({ machine: m, close, saved }: Props & { machine: Machine }) {
  const machineLabel = 'sluicework-' + m.id.slice(0, 12)
  const [workstation, setWorkstation] = useState(m.runner?.workstation || false)
  const [resources, setResources] = useState({
    restore: false,
    revision: 0,
    settings: m.runner?.settings || m.workstation?.local,
  })
  const compatible = m.workstation?.settingsVersion === 1
  return (
    <Dialog title="GitHub Actions runner" close={close}>
      <Form
        close={close}
        label={workstation ? (m.runner?.workstation ? 'Save workstation settings' : 'Approve workstation') : 'Record runner'}
        submit={async (d) => {
          await api(idPath('/machines', m.id), 'PATCH', {
            runner: {
              workstation,
              settings: workstation && !resources.restore ? (compatible ? settingsFromForm(d) : m.runner?.settings || null) : null,
              name: val(d, 'name'),
              scope: val(d, 'scope'),
              labels: split(val(d, 'labels')),
              service: workstation ? '' : val(d, 'service'),
            },
          })
          saved()
        }}
      >
        <label className="check">
          <input type="checkbox" checked={workstation} onChange={(e) => setWorkstation(e.target.checked)} />
          Manage as an optional workstation runner
        </label>
        {workstation ? (
          <p>Allow Sluicework to register one-job runners for this repository when you enable the workstation. Each session starts paused and accepts one build at a time while resource limits allow. Pause locally or drain this machine to finish its current build. Use your normal login for trusted workflows or a separate account for additional isolation.</p>
        ) : <>
        <ol className="guidance">
          <li>
            Open <b>Settings → Actions → Runners</b> for your repository or organization. Select{' '}
            <b>New self-hosted runner</b> and choose {m.os && m.arch ? platformName(m) : 'the operating system and architecture of this machine'}.
          </li>
          <li>
            On {m.name}, follow GitHub’s download and configuration commands in an isolated runner
            account. Add a unique machine label, then install and start the runner service.
          </li>
          <li>
            Set the workflow’s <code>runs-on</code> to all matching platform, architecture and machine labels. GitHub dispatches only to runners matching every label. Use separately isolated
            capacity for untrusted workflows. Keep cluster and provider credentials in their own
            service accounts.
          </li>
        </ol>
        </>}
        {m.os && m.arch ? (
          <div className="callout">
            {workstation ? 'Sluicework adds the machine label ' : 'Register the custom label '}<code>{machineLabel}</code>{workstation ? '. Target it explicitly in your workflow:' : ' on this GitHub runner, then use:'}
            <pre className="runner-example">runs-on: {JSON.stringify([...runnerLabels(m), machineLabel])}</pre>
            This targets {platformName(m)} on {m.name}. {workstation ? 'Use the pilot workflow to choose whether cloud fallback is allowed.' : 'If it is unavailable, the workflow waits.'}
          </div>
        ) : (
          <p>Upgrade this worker to report its OS and architecture before configuring platform-specific workflows.</p>
        )}
        <a
          href={workstation ? 'https://github.com/formkiq/sluicework/blob/main/docs/workstation.md' : 'https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/adding-self-hosted-runners'}
          target="_blank"
          rel="noreferrer"
        >
          {workstation ? 'Workstation setup guide' : 'GitHub runner setup guide'} <ExternalLink size={13} />
        </a>
        <Field label={workstation ? 'Workstation display name' : 'Registered runner name'}>
          <input name="name" defaultValue={m.runner?.name} required />
        </Field>
        <Field label={workstation ? 'Repository' : 'Repository or organization'} hint={workstation ? 'Use owner/repository. Only this approved scope can request runner credentials.' : 'Use owner/repository or owner.'}>
          <input
            name="scope"
            defaultValue={m.runner?.scope}
            placeholder="team/repository"
            required
          />
        </Field>
        <Field label="Runner labels">
          <input
            name="labels"
            defaultValue={m.runner?.labels?.join(', ') || [...runnerLabels(m), machineLabel].join(', ')}
            required
          />
        </Field>
        {workstation && <>
          <WorkstationSummary machine={m} />
          <h3>Resource settings</h3>
          {!compatible && <p>Update and restart the workstation controller to enable central resource settings. Its current installation settings remain in effect.</p>}
          {compatible && <>
            <p>Edit the settings below and save to apply them to this machine.</p>
            <div className="field-stack" key={resources.revision} onChange={() => setResources(current => current.restore ? { ...current, restore: false } : current)}>
              <WorkstationFields machine={m} settings={resources.settings!} />
            </div>
          </>}
          {(compatible || m.runner?.settings) && <>
            <button type="button" onClick={() => setResources(current => ({
              restore: true,
              revision: current.revision + 1,
              settings: m.workstation?.local,
            }))}>Restore installation defaults</button>
            {resources.restore && <p role="status">Installation defaults selected. Save workstation settings to apply them.</p>}
          </>}
        </>}
        {!workstation && <Field label={m.os === 'linux' ? 'Systemd service unit' : 'Launchd service label'}>
          <input
            name="service"
            defaultValue={m.runner?.service}
            placeholder={m.os === 'linux' ? 'actions.runner.owner-repo.build-linux.service' : 'gui/501/actions.runner.owner-repo.build-mac'}
            required
          />
        </Field>}
      </Form>
    </Dialog>
  )
}
export { Enrollment } from './enroll'
