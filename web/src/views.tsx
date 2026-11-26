import { ProviderOptions } from './provider-auth'
import { PersonalAgents } from './personal-agents'
import {
  Activity,
  ArrowUpRight,
  CalendarClock,
  ChevronRight,
  CircleAlert,
  Cpu,
  Github,
  HardDrive,
  History as HistoryIcon,
  Layers,
  ListFilter,
  MessageSquare,
  Play,
  PlugZap,
  Plus,
  RefreshCw,
  Server,
  Settings2,
  SquareTerminal,
} from 'lucide-react'
import { useState, useEffect } from 'react'
import { api } from './api'
import type { ReactNode } from 'react'
import type { Cluster, Connection, Job, Machine, OpenWebUIStatus, Page, Run, Schedule } from './model'
import { attemptOf, kindNames, machineStatus, runStatus, stateNames, when, platformName } from './model'
import { GitHubActivityPanel, MachineBuilds } from './github'
import { CodingClients } from './harness'
import { WorkstationSummary, WorkstationCapacity } from './workstation'
import { Badge, Empty, ErrorMessage, Heading, LinkButton, Panel } from './ui'
export type Actions = {
  refresh: () => void
  go: (page: Page) => void
  openRun: (id: string) => void
  launch: (j?: Job) => void
  editJob: (j?: Job) => void
  schedule: (j?: Job, s?: Schedule) => void
  scheduleHistory: (s: Schedule) => void
  removeSchedule: (s: Schedule) => void
  enroll: () => void
  editMachine: (m: Machine) => void
  drain: (m: Machine) => void
  revoke: (m: Machine) => void
  runner: (m: Machine) => void
  editConnection: (c?: Connection) => void
  testConnection: (c: Connection) => void
  setupModels: (c?: Connection) => void
}
type Props = { cluster: Cluster; admin: boolean; actions: Actions }
const ordered = (runs: Run[]) => [...runs].sort((a, b) => b.createdAt.localeCompare(a.createdAt))
function JobIcon({ type }: { type: string }) {
  return (
    <span className={'item-icon ' + (type === 'ai' ? 'violet' : '')}>
      {type === 'ai' ? <Cpu size={17} /> : <SquareTerminal size={17} />}
    </span>
  )
}
function LaunchButton({ actions, jobs }: { actions: Actions; jobs: Job[] }) {
  return (
    <button className="primary" disabled={!jobs.length} onClick={() => actions.launch()}>
      <Play size={15} />
      Run a job
    </button>
  )
}
function Metric({
  label,
  value,
  note,
  icon,
}: {
  label: string
  value: ReactNode
  note: string
  icon: ReactNode
}) {
  return (
    <section className="metric">
      <div>
        {icon}
        <span>{label}</span>
      </div>
      <strong>{value}</strong>
      <small>{note}</small>
    </section>
  )
}
function ActivityChart({ hourly }: { hourly: Cluster['overview']['hourly'] }) {
  const buckets = hourly.map((b) => ({ time: new Date(b.start), n: b.runs, f: b.failures }))
  const max = Math.max(1, ...buckets.map((b) => b.n)),
    total = buckets.reduce((n, b) => n + b.n, 0)
  const fmt = (d: Date) => d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
  return (
    <section className="activity">
      <div className="activity-head">
        <div>
          <div className="eyebrow">
            <Activity size={14} />
            Sluicework job activity
          </div>
          <div className="chart-total">
            {total}
            <span>{total === 1 ? 'run' : 'runs'} in the last 24 hours</span>
          </div>
        </div>
        <span className="chart-unit">Hourly</span>
      </div>
      <div
        className="chart"
        role="img"
        aria-label={
          total +
          ' runs in the last 24 hours. Highest hourly count ' +
          Math.max(0, ...buckets.map((b) => b.n)) +
          '.'
        }
      >
        {buckets.map((b) => (
          <div
            key={b.time.toISOString()}
            className="bucket"
            title={fmt(b.time) + ' · ' + b.n + ' runs · ' + b.f + ' failures'}
          >
            <div
              className={'bar' + (!b.n ? ' zero' : '')}
              style={{ height: b.n ? (b.n / max) * 100 + '%' : '2px' }}
            >
              {!!b.f && <i style={{ height: (b.f / b.n) * 100 + '%' }} />}
            </div>
          </div>
        ))}
      </div>
      <div className="chart-times">
        {[0, 6, 12, 18, 23]
          .filter((i) => buckets[i])
          .map((i) => (
            <span key={i}>{fmt(buckets[i].time)}</span>
          ))}
      </div>
    </section>
  )
}
function RunList({
  runs,
  actions,
  machines,
  empty,
}: {
  runs: Run[]
  actions: Actions
  machines: Machine[]
  empty: string
}) {
  return runs.length ? (
    <div className="run-list">
      {runs.map((r) => (
        <button key={r.id} className="list-row" onClick={() => actions.openRun(r.id)}>
          <JobIcon type={r.job.type} />
          <span className="row-main">
            <b>{r.job.name}</b>
            <small>
              {machines.find((m) => m.id === attemptOf(r)?.machineId)?.name || 'Awaiting placement'}{' '}
              · {when(r.createdAt)}
            </small>
          </span>
          <Badge value={runStatus(r)} />
          <ChevronRight size={14} />
        </button>
      ))}
    </div>
  ) : (
    <Empty title={empty} />
  )
}
export function Overview({ cluster: c, actions: a, admin }: Props) {
  const online = c.machines.filter((m) => m.online && !m.revoked)
  const orderedRuns = ordered(c.runs),
    failed = c.overview.recentFailures || []
  const upcoming = c.schedules
    .filter((s) => s.enabled && s.nextRun)
    .sort((a, b) => a.nextRun!.localeCompare(b.nextRun!))
  return (
    <>
      <Heading
        title="Cluster overview"
        description="Health, workload activity, and upcoming work across your machines."
        action={<LaunchButton actions={a} jobs={c.jobs} />}
      />
      {c.overview.unhealthyConnections > 0 && (
        <div className="provider-warning">
          <CircleAlert size={16} />
          {c.overview.unhealthyConnections} AI{' '}
          {c.overview.unhealthyConnections === 1 ? 'connection needs' : 'connections need'}{' '}
          attention.
          {admin && (
            <LinkButton onClick={() => a.go('AI connections')}>View connections</LinkButton>
          )}
        </div>
      )}
      <div className="metrics">
        <Metric
          label="Online machines"
          icon={<Server />}
          value={
            <>
              {online.length}
              <em> / {c.machines.filter((m) => !m.revoked).length}</em>
            </>
          }
          note="Reporting a heartbeat"
        />
        <Metric
          label="Running Sluicework jobs"
          icon={<Activity />}
          value={c.overview.running}
          note="Active job attempts"
        />
        <Metric
          label="Sluicework queue"
          icon={<Layers />}
          value={c.overview.queued}
          note="Waiting for a worker"
        />
        <Metric
          label="Failed Sluicework jobs"
          icon={<CircleAlert />}
          value={c.overview.failed24h}
          note="Over the last 24 hours"
        />
      </div>
      <GitHubActivityPanel cluster={c} limit={3} />
      <div className="overview-grid">
        <ActivityChart hourly={c.overview.hourly || []} />
        <Panel
          title="Sluicework job capacity"
          action={<LinkButton onClick={() => a.go('Machines')}>Machines</LinkButton>}
        >
          {!c.machines.length ? (
            <Empty
              title="No workers enrolled"
              action={
                admin ? (
                  <button onClick={a.enroll}>
                    <Plus size={14} /> Enroll machine
                  </button>
                ) : undefined
              }
            />
          ) : (
            <div className="capacity-list">
              {c.machines.slice(0, 4).map((m) => (
                <div className="capacity" key={m.id}>
                  <div>
                    <Server size={15} />
                    <b>{m.name}</b>
                    <Badge value={machineStatus(m)} />
                  </div>
                  <div className="capacity-meter">
                    <i style={{ width: Math.min(100, (m.running / m.slots) * 100) + '%' }} />
                  </div>
                  <small>
                    {m.running} of {m.slots} slots in use
                  </small>
                </div>
              ))}
            </div>
          )}
        </Panel>
      </div>
      <div className="overview-grid">
        <Panel
          title="Recent Sluicework jobs"
          action={<LinkButton onClick={() => a.go('Runs')}>All runs</LinkButton>}
        >
          <RunList
            runs={orderedRuns.slice(0, 5)}
            actions={a}
            machines={c.machines}
            empty="No runs recorded"
          />
        </Panel>
        <Panel
          title="Upcoming schedules"
          action={<LinkButton onClick={() => a.go('Jobs')}>Jobs</LinkButton>}
        >
          {upcoming.length ? (
            upcoming.slice(0, 4).map((s) => (
              <div key={s.id} className="list-row">
                <span className="item-icon">
                  <CalendarClock size={17} />
                </span>
                <span className="row-main">
                  <b>{s.name}</b>
                  <small>
                    {when(s.nextRun, s.timezone)} · {s.timezone}
                  </small>
                </span>
              </div>
            ))
          ) : (
            <Empty title="No upcoming schedules" />
          )}
        </Panel>
      </div>
      {failed.length > 0 && (
        <Panel title="Recent failures">
          {failed.map((r) => (
            <button key={r.id} className="list-row" onClick={() => a.openRun(r.id)}>
              <JobIcon type={r.jobType} />
              <span className="row-main">
                <b>{r.jobName}</b>
                <small>
                  {c.machines.find((m) => m.id === r.machineId)?.name || 'Unassigned'} ·{' '}
                  {when(r.createdAt)}
                </small>
              </span>
              <Badge value={r.state} />
              <ChevronRight size={14} />
            </button>
          ))}
        </Panel>
      )}
    </>
  )
}
export function Machines({ cluster: c, admin, actions: a }: Props) {
  return (
    <>
      <Heading
        title="Machines"
        description="Manage worker placement, capacity, and CI runners."
        action={
          admin && (
            <button className="primary" onClick={a.enroll}>
              <Plus size={16} />
              Enroll machine
            </button>
          )
        }
      />
      {c.github.issue && <div className="provider-warning"><CircleAlert size={16} />{c.github.issue}</div>}
      {!c.machines.length ? (
        <Panel title="Worker registry">
          <Empty title="No workers enrolled">
            Enroll a Linux or Mac worker to start claiming jobs.
          </Empty>
        </Panel>
      ) : (
        <div className="machine-grid">
          {c.machines.map((m) => (
            <article className="panel machine" key={m.id}>
              <div className="machine-title">
                <span className="large-icon">
                  <Server size={23} />
                </span>
                <div>
                  <h2>{m.name}</h2>
                  <small>Worker {m.version}</small>
                  <small>{m.os && m.arch ? platformName(m) : 'Platform not reported — upgrade worker'}</small>
                </div>
                <Badge value={machineStatus(m)} />
              </div>
              <dl className="detail-grid">
                <div>
                  <dt>Heartbeat</dt>
                  <dd>{when(m.lastHeartbeat)}</dd>
                </div>
                <div>
                  <dt>Sluicework jobs / slots</dt>
                  <dd>
                    {m.running} / {m.slots}
                  </dd>
                </div>
                <div>
                  <dt>Roles</dt>
                  <dd>{m.roles?.join(', ') || '—'}</dd>
                </div>
                <div>
                  <dt>Labels</dt>
                  <dd>{m.labels?.join(', ') || '—'}</dd>
                </div>
              </dl>
              {!!Object.keys(m.roleLimits || {}).length && (
                <div className="minor">
                  Role caps ·{' '}
                  {Object.entries(m.roleLimits)
                    .map(([r, n]) => r + ' ' + n)
                    .join(', ')}
                </div>
              )}
              {c.runs
                .filter((r) => r.state === 'running' && attemptOf(r)?.machineId === m.id)
                .map((r) => (
                  <button key={r.id} className="link" onClick={() => a.openRun(r.id)}>
                    <Activity size={14} />
                    {r.job.name}
                    <ChevronRight size={14} />
                  </button>
                ))}
              {!m.revoked && (
                <div className="actions">
                  <button onClick={() => a.editMachine(m)}>
                    <Settings2 size={14} />
                    Configure
                  </button>
                  <button onClick={() => a.drain(m)}>
                    {m.draining ? 'Resume claims' : 'Drain'}
                  </button>
                  {admin && (
                    <button className="subtle-danger" onClick={() => a.revoke(m)}>
                      Revoke
                    </button>
                  )}
                </div>
              )}
              <section className="runner">
                <div className="runner-title">
                  <Github size={16} />
                  <b>GitHub runner</b>
                  {admin && !m.revoked && (
                    <button className="link" onClick={() => a.runner(m)}>
                      {m.runner ? 'Configure' : 'Set up'} <ChevronRight size={13} />
                    </button>
                  )}
                </div>
                {m.runner ? (
                  <>
                    <div className="runner-status">
                      <span>{m.runner.name}</span>
                      <Badge value={m.runnerStatus || 'unknown'} />
                    </div>
                    <small>
                      {m.runner.scope} · {m.runner.labels?.join(', ')}
                    </small>
                    {m.runner.workstation && c.github.repositories && c.github.repositories.length > 1 && (
                      <p className="muted">Shared workstation · {c.github.repositories.map((repository, index) => (
                        <span key={repository}>{index > 0 ? ' · ' : ''}<a href={'https://github.com/' + repository.split('/').map(encodeURIComponent).join('/') + '/actions'} target="_blank" rel="noreferrer">{repository}</a></span>
                      ))}</p>
                    )}
                    <MachineBuilds cluster={c} machine={m} />
                    <WorkstationSummary machine={m} />
                    <WorkstationCapacity machine={m} admin={admin} />
                    <a
                      href={
                        m.runner.scope.includes('/')
                          ? 'https://github.com/' +
                            m.runner.scope.split('/').map(encodeURIComponent).join('/') +
                            '/actions'
                          : 'https://github.com/organizations/' +
                            encodeURIComponent(m.runner.scope) +
                            '/settings/actions/runners'
                      }
                      target="_blank"
                      rel="noreferrer"
                    >
                      {m.runner.scope.includes('/') ? 'Workflow history' : 'Organization runners'}{' '}
                      <ArrowUpRight size={13} />
                    </a>
                  </>
                ) : (
                  <small>No runner registered.</small>
                )}
              </section>
            </article>
          ))}
        </div>
      )}
    </>
  )
}
export function Jobs({ cluster: c, admin, actions: a }: Props) {
  const [tab, setTab] = useState<'templates' | 'schedules'>('templates')
  return (
    <>
      <Heading
        title="Jobs"
        description="Run approved templates and manage recurring work."
        action={
          (admin || tab === 'schedules') && (
            <button
              className="primary"
              disabled={tab === 'schedules' && !c.jobs.length}
              onClick={() => (tab === 'templates' ? a.editJob() : a.schedule())}
            >
              <Plus size={16} />
              {tab === 'templates' ? 'Create template' : 'Add schedule'}
            </button>
          )
        }
      />
      <div className="tabs">
        <button
          className={tab === 'templates' ? 'selected' : ''}
          onClick={() => setTab('templates')}
        >
          <SquareTerminal size={15} />
          Templates <span>{c.jobs.length}</span>
        </button>
        <button
          className={tab === 'schedules' ? 'selected' : ''}
          onClick={() => setTab('schedules')}
        >
          <CalendarClock size={15} />
          Schedules <span>{c.schedules.length}</span>
        </button>
      </div>
      {tab === 'templates' ? (
        <Panel title="Approved templates">
          {!c.jobs.length ? (
            <Empty title="No approved templates">
              An administrator can approve a script or AI task to make it available to the team.
            </Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Template</th>
                    <th>Placement</th>
                    <th>Timeout</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {c.jobs.map((j) => (
                    <tr key={j.id}>
                      <td>
                        <div className="item-name">
                          <JobIcon type={j.type} />
                          <div>
                            <b>{j.name}</b>
                            <small>
                              {j.type === 'ai' ? 'AI task' : 'Script'} · revision {j.revision}
                            </small>
                          </div>
                        </div>
                      </td>
                      <td>
                        {j.role}
                        <small>{platformName(j)}</small>
                        <small>{j.labels?.join(', ')}</small>
                      </td>
                      <td>{j.timeoutSeconds}s</td>
                      <td>
                        <div className="actions">
                          <button onClick={() => a.launch(j)}>
                            <Play size={12} />
                            Run
                          </button>
                          <button onClick={() => a.schedule(j)}>Schedule</button>
                          {admin && <button onClick={() => a.editJob(j)}>Edit</button>}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      ) : (
        <Panel title="Cron schedules">
          {!c.schedules.length ? (
            <Empty title="No schedules configured">
              Create a schedule for an approved template to run it automatically.
            </Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Schedule</th>
                    <th>Timing</th>
                    <th>Next run</th>
                    <th>Last occurrence</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {c.schedules.map((s) => (
                    <tr key={s.id}>
                      <td>
                        <b>{s.name}</b>
                        <small>{c.jobs.find((j) => j.id === s.jobId)?.name}</small>
                      </td>
                      <td>
                        <code>{s.cron}</code>
                        <small>{s.timezone}</small>
                      </td>
                      <td>{s.enabled ? when(s.nextRun, s.timezone) : <Badge value="paused" />}</td>
                      <td>{s.latest ? <Badge value={s.latest.status} /> : '—'}</td>
                      <td>
                        <div className="actions">
                          <button
                            title="Occurrence history"
                            aria-label={'Occurrence history for ' + s.name}
                            onClick={() => a.scheduleHistory(s)}
                          >
                            <HistoryIcon size={15} />
                          </button>
                          <button onClick={() => a.schedule(undefined, s)}>Edit</button>
                          <button className="subtle-danger" onClick={() => a.removeSchedule(s)}>
                            Delete
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      )}
    </>
  )
}
export function Runs({ cluster: c, actions: a }: Props) {
  const [job, setJob] = useState(''),
    [machine, setMachine] = useState(''),
    [status, setStatus] = useState('')
  const [cursors, setCursors] = useState<string[]>([])
  const [filtered, setFiltered] = useState<Run[]>([]),
    [error, setError] = useState(''),
    [loading, setLoading] = useState(false)
  const before = cursors.at(-1) || ''
  const filtering = !!(job || machine || status || before)
  useEffect(() => {
    if (!filtering) return
    let active = true
    const params = new URLSearchParams({ job, machine, status, before })
    const load = async () => {
      try {
        const result = await api<Run[]>('/runs?' + params)
        if (active) {
          setFiltered(result || [])
          setError('')
        }
      } catch (err) {
        if (active) setError((err as Error).message)
      } finally {
        if (active) setLoading(false)
      }
    }
    setFiltered([])
    setLoading(true)
    void load()
    const timer = setInterval(load, 5000)
    return () => {
      active = false
      clearInterval(timer)
    }
  }, [filtering, job, machine, status, before])
  const runs = filtering ? filtered : c.runs
  const changeFilter = (setter: (value: string) => void, value: string) => {
    setCursors([])
    setter(value)
  }
  return (
    <>
      <Heading
        title="Runs"
        description="Inspect execution history, attempts, logs, and results."
        action={<LaunchButton actions={a} jobs={c.jobs} />}
      />
      <GitHubActivityPanel cluster={c} />
      <h2 className="section-heading">Sluicework jobs</h2>
      <div className="filter-row">
        <ListFilter size={17} />
        <label>
          Job
          <select
            aria-label="Job"
            value={job}
            onChange={(e) => changeFilter(setJob, e.target.value)}
          >
            <option value="">All jobs</option>
            {c.jobs.map((j) => (
              <option key={j.id} value={j.id}>
                {j.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          Machine
          <select
            aria-label="Machine"
            value={machine}
            onChange={(e) => changeFilter(setMachine, e.target.value)}
          >
            <option value="">All machines</option>
            {c.machines.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          Status
          <select
            aria-label="Status"
            value={status}
            onChange={(e) => changeFilter(setStatus, e.target.value)}
          >
            <option value="">All statuses</option>
            {Object.entries(stateNames)
              .filter(([v]) => !['skipped_overlap', 'cancellation_requested'].includes(v))
              .map(([v, k]) => (
                <option key={v} value={v}>
                  {k}
                </option>
              ))}
          </select>
        </label>
      </div>
      {filtering && error && <ErrorMessage>{error}</ErrorMessage>}
      <Panel
        title="Sluicework run history"
        action={
          <span className="minor">
            Page {cursors.length + 1} · {runs.length} {runs.length === 1 ? 'run' : 'runs'}
          </span>
        }
      >
        {filtering && loading ? (
          <div className="empty" role="status">
            Loading runs…
          </div>
        ) : !runs.length ? (
          <Empty title={c.runs.length ? 'No matching runs' : 'No runs recorded'}>
            {c.runs.length
              ? 'Adjust the filters to include more runs.'
              : 'Run an approved template to see its progress here.'}
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Job</th>
                  <th>Status</th>
                  <th>Machine</th>
                  <th>Queued at</th>
                  <th>Attempt</th>
                </tr>
              </thead>
              <tbody>
                {runs.map((r) => (
                  <tr key={r.id}>
                    <td>
                      <button className="item-name row-link" onClick={() => a.openRun(r.id)}>
                        <JobIcon type={r.job.type} />
                        <span>
                          <b>{r.job.name}</b>
                          <small className="mono">{r.id.slice(0, 12)}</small>
                        </span>
                      </button>
                    </td>
                    <td>
                      <Badge value={runStatus(r)} />
                    </td>
                    <td>{c.machines.find((m) => m.id === attemptOf(r)?.machineId)?.name || '—'}</td>
                    <td>{when(r.createdAt)}</td>
                    <td>{attemptOf(r)?.number || 1}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {(cursors.length > 0 || runs.length === 100) && (
          <div className="pagination">
            <button
              disabled={!cursors.length || loading}
              onClick={() => setCursors((xs) => xs.slice(0, -1))}
            >
              Newer runs
            </button>
            <button
              disabled={runs.length < 100 || loading}
              onClick={() => setCursors((xs) => [...xs, runs[runs.length - 1].id])}
            >
              Older runs
            </button>
          </div>
        )}
      </Panel>
    </>
  )
}
function OpenWebUICard({ admin, go }: { admin: boolean; go: (page: Page) => void }) {
  const [status, setStatus] = useState<OpenWebUIStatus | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const load = async () => {
    setLoading(true)
    setError('')
    try {
      setStatus(await api<OpenWebUIStatus>('/open-webui'))
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => {
    void load()
  }, [])
  if (loading && !status) return null
  if (error) {
    return (
      <Panel title="Open WebUI">
        <ErrorMessage>{error}</ErrorMessage>
      </Panel>
    )
  }
  if (!status?.configured) {
    return (
      <Panel
        title="Open WebUI"
        action={
          admin && (
            <button onClick={() => go('Settings')}>
              <Settings2 size={14} />
              Configure in Settings
            </button>
          )
        }
      >
        <p className="minor">
          Not configured yet. Enter an existing Open WebUI URL in Settings to chat with your Ollama
          and Qwen models.
        </p>
      </Panel>
    )
  }
  const badge = !status.enabled ? 'disabled' : status.healthy ? 'healthy' : 'unhealthy'
  return (
    <article className="panel machine">
      <div className="machine-title">
        <span className="large-icon">
          <MessageSquare size={22} />
        </span>
        <div>
          <h2>Open WebUI</h2>
          <small>Local chat companion</small>
        </div>
        <Badge value={badge} />
      </div>
      <dl className="detail-grid single">
        {status.url && (
          <div>
            <dt>URL</dt>
            <dd className="mono">{status.url}</dd>
          </div>
        )}
        {status.ollamaBaseUrl && (
          <div>
            <dt>Ollama backend</dt>
            <dd className="mono">{status.ollamaBaseUrl}</dd>
          </div>
        )}
      </dl>
      <p className="minor">
        Models installed into Ollama via Manage models (including Qwen presets) appear automatically
        in Open WebUI.
      </p>
      {status.error && <p className="error-text">{status.error}</p>}
      <div className="actions">
        {status.enabled && status.url && (
          <a
            className="button primary"
            href={status.url}
            target="_blank"
            rel="noreferrer"
          >
            Open WebUI
            <ArrowUpRight size={14} />
          </a>
        )}
        <button disabled={loading} onClick={() => void load()}>
          <RefreshCw size={14} />
          {loading ? 'Checking…' : 'Check health'}
        </button>
        {admin && (
          <button onClick={() => go('Settings')}>
            <Settings2 size={14} />
            Configure
          </button>
        )}
      </div>
    </article>
  )
}
export function Connections({ cluster: c, admin, actions: a }: Props) {
  return (
    <>
      <Heading
        title="AI connections"
        description="Configure local inference and customer-owned cloud providers."
        action={
          <div className="actions">
            {admin && <><button className="primary" onClick={() => a.setupModels()}>
              Set up local AI
            </button>
            <button onClick={() => a.editConnection()}>
              <Plus size={16} />
              Add connection
            </button></>}
          </div>
        }
      />
      <ProviderOptions admin={admin} changed={a.refresh} />
      <PersonalAgents />
      <OpenWebUICard admin={admin} go={a.go} />
      {admin && <CodingClients connections={c.connections} go={a.go} />}
      {!c.connections.length ? (
        <Panel title="Named connections">
          <Empty title="No connections configured">
            Add an Ollama endpoint or a cloud provider to use AI templates.
          </Empty>
        </Panel>
      ) : (
        <div className="machine-grid">
          {c.connections.map((item) => (
            <article className="panel machine" key={item.id}>
              <div className="machine-title">
                <span className="large-icon">
                  {item.kind === 'ollama' ? <HardDrive size={22} /> : <PlugZap size={22} />}
                </span>
                <div>
                  <h2>{item.name}</h2>
                  <small>{kindNames[item.kind]}</small>
                </div>
                <Badge value={item.disabled ? 'disabled' : item.health} />
              </div>
              <dl className="detail-grid single">
                <div>
                  <dt>Endpoint</dt>
                  <dd className="mono">{item.endpoint}</dd>
                </div>
                <div>
                  <dt>Permitted models</dt>
                  <dd className="tag-list">
                    {item.models.map((m) => (
                      <code key={m}>{m}</code>
                    ))}
                  </dd>
                </div>
                <div>
                  <dt>Last checked</dt>
                  <dd>{when(item.last_checked)}</dd>
                </div>
              </dl>

              {item.last_error && <p className="error-text">{item.last_error}</p>}
              {(admin || item.kind === 'chatgpt') && <div className="actions">
                {item.kind === 'ollama' && (
                  <button disabled={item.disabled} onClick={() => a.setupModels(item)}>
                    Manage models
                  </button>
                )}
                <button disabled={item.disabled} onClick={() => a.testConnection(item)}>
                  <Activity size={14} />
                  Test connection
                </button>
                <button onClick={() => a.editConnection(item)}>
                  <Settings2 size={14} />
                  Edit
                </button>
              </div>}
            </article>
          ))}
        </div>
      )}
    </>
  )
}
