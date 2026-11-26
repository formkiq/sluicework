import { useCallback, useEffect, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import {
  Activity,
  ArrowRight,
  Check,
  ChevronRight,
  Layers,
  LayoutDashboard,
  LockKeyhole,
  LogOut,
  Menu,
  Network,
  Radio,
  RefreshCw,
  Server,
  Settings2,
  GitBranch,
  X,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { api, ApiError, idPath } from './api'
import type {
  Cluster,
  Connection,
  Job,
  Machine,
  Occurrence,
  Page,
  Schedule,
  Session,
} from './model'
import { emptyCluster } from './model'
import { Brand, Dialog, ErrorMessage, Field, Form } from './ui'
import {
  Launch,
  JobEditor,
  ScheduleEditor,
  MachineEditor,
  ConnectionEditor,
  RunnerEditor,
  Enrollment,
  History,
} from './forms'
import { Machines, Runs, Jobs, Overview, Connections } from './views'
import type { Actions } from './views'
import { RunDetail } from './run'
import { ModelSetup } from './models'
import { Settings } from './settings'
import { ActionsRunners } from './actions-runners'
import './style.css'

const pages: { name: Page; icon: LucideIcon; path: string }[] = [
  { name: 'Overview', icon: LayoutDashboard, path: 'overview' },
  { name: 'Machines', icon: Server, path: 'machines' },
  { name: 'Actions runners', icon: GitBranch, path: 'actions-runners' },
  { name: 'Jobs', icon: Layers, path: 'jobs' },
  { name: 'Runs', icon: Activity, path: 'runs' },
  { name: 'AI connections', icon: Network, path: 'connections' },
  { name: 'Settings', icon: Settings2, path: 'settings' },
]
type Modal =
  | { kind: 'launch' | 'job'; job?: Job; connections?: Connection[] }
  | { kind: 'schedule'; job?: Job; s?: Schedule }
  | { kind: 'machine' | 'runner' | 'revoke'; m: Machine }
  | { kind: 'connection'; c?: Connection }
  | { kind: 'models'; c?: Connection }
  | { kind: 'enroll' }
  | { kind: 'history'; s: Schedule; items: Occurrence[] }
  | { kind: 'remove-schedule'; s: Schedule }
function Login({ done, initialError }: { done: () => void; initialError: string }) {
  const [error, setError] = useState(initialError),
    [busy, setBusy] = useState(false)
  return (
    <main className="login">
      <aside className="login-art">
        <Brand />
        <div
          className="topology"
          aria-label="Topology diagram showing one orchestrator and two workers"
        >
          <div className="topology-label">
            <Radio size={16} />
            Cluster topology
          </div>
          <div className="topology-orchestrator">
            <div className="schematic-node">
              <Network size={29} />
            </div>
            <b>Orchestrator</b>
          </div>
          <div className="wire-center" />
          <div className="wire-bridge" />
          <div className="worker-pair">
            {[1, 2].map((n) => (
              <div key={n}>
                <div className="schematic-worker">
                  <i />
                  <i />
                  <em />
                </div>
                <span>Worker {n}</span>
              </div>
            ))}
          </div>
        </div>
        <div className="login-art-foot">
          <span className="dot" />
          Self-hosted cluster console
        </div>
      </aside>
      <section className="login-main">
        <div className="login-form">
          <div className="login-mark">
            <LockKeyhole size={21} />
          </div>
          <h1>Sign in to Sluicework</h1>
          <p>Use your cluster account to continue.</p>
          <form
            onSubmit={async (e) => {
              e.preventDefault()
              if (busy) return
              const data = new FormData(e.currentTarget)
              setBusy(true)
              setError('')
              try {
                await api('/login', 'POST', {
                  username: data.get('username'),
                  password: data.get('password'),
                })
                done()
              } catch (err) {
                setError((err as Error).message)
              } finally {
                setBusy(false)
              }
            }}
          >
            <Field label="Username">
              <input autoComplete="username" name="username" required autoFocus />
            </Field>
            <Field label="Password">
              <input type="password" autoComplete="current-password" name="password" required />
            </Field>
            {error && <ErrorMessage>{error}</ErrorMessage>}
            <button className="primary" disabled={busy}>
              {busy ? 'Signing in…' : 'Sign in'}
              <ArrowRight size={16} />
            </button>
          </form>
        </div>
      </section>
    </main>
  )
}
function Console({ session, logout }: { session: Session; logout: () => void }) {
  const [hash, setHash] = useState(location.hash),
    [mobile, setMobile] = useState(false)
  const [cluster, setCluster] = useState<Cluster>(emptyCluster),
    [loaded, setLoaded] = useState(false),
    [stale, setStale] = useState(''),
    [error, setError] = useState(''),
    [notice, setNotice] = useState(''),
    [modal, setModal] = useState<Modal>(),
    [testing, setTesting] = useState('')
  const request = useRef(0),
    admin = session.role === 'admin',
    routes = pages.filter((p) => admin || (p.name !== 'Settings' && p.name !== 'Actions runners'))
  const [path, runId] = hash.replace(/^#\/?/, '').split('/')
  const page = routes.find((p) => p.path === path)?.name || 'Overview'
  const refresh = useCallback(async () => {
    const sequence = ++request.current
    try {
      const [overview, machines, jobs, runs, schedules, connections, github] = await Promise.all([
        api<Cluster['overview']>('/overview'),
        api<Machine[]>('/machines'),
        api<Job[]>('/jobs'),
        api<Cluster['runs']>('/runs'),
        api<Schedule[]>('/schedules'),
        api<Connection[]>('/connections'),
        api<Cluster['github']>('/github/activity'),
      ])
      if (sequence !== request.current) return
      setCluster({
        overview,
        machines: machines || [],
        jobs: jobs || [],
        runs: runs || [],
        schedules: schedules || [],
        connections: connections || [],
        github,
      })
      setStale('')
      setLoaded(true)
    } catch (err) {
      if (sequence === request.current) setStale((err as Error).message)
    }
  }, [admin])
  useEffect(() => {
    void refresh()
    const timer = setInterval(refresh, 5000)
    return () => {
      request.current++
      clearInterval(timer)
    }
  }, [refresh])
  useEffect(() => {
    const fn = () => {
      setHash(location.hash)
      setMobile(false)
      setModal(undefined)
      window.scrollTo(0, 0)
    }
    window.addEventListener('hashchange', fn)
    return () => window.removeEventListener('hashchange', fn)
  }, [])
  useEffect(() => {
    document.title = (runId && page === 'Runs' ? 'Run details' : page) + ' · Sluicework'
  }, [runId, page])
  useEffect(() => {
    if (!notice) return
    const t = setTimeout(() => setNotice(''), 5000)
    return () => clearTimeout(t)
  }, [notice])
  const go = (name: Page) => {
    location.hash = '/' + pages.find((p) => p.name === name)!.path
    setMobile(false)
  }
  const openRun = (id: string) => {
    setModal(undefined)
    location.hash = '/runs/' + encodeURIComponent(id)
  }
  const close = () => setModal(undefined),
    saved = () => {
      close()
      void refresh()
    }
  const action = async (fn: () => Promise<void>) => {
    setError('')
    try {
      await fn()
      void refresh()
    } catch (err) {
      setError((err as Error).message)
    }
  }
  const actions: Actions = {
    refresh: () => { void refresh() },
    go,
    openRun,
    launch: (job) => setModal({ kind: 'launch', job }),
    editJob: (job) => { void action(async () => {
      const connections = await api<Connection[]>('/connections?purpose=job-approval')
      setModal({ kind: 'job', job, connections })
    }) },
    enroll: () => setModal({ kind: 'enroll' }),
    editMachine: (m) => setModal({ kind: 'machine', m }),
    runner: (m) => setModal({ kind: 'runner', m }),
    drain: (m) =>
      action(async () => {
        await api(idPath('/machines', m.id), 'PATCH', { draining: !m.draining })
      }),
    revoke: (m) => setModal({ kind: 'revoke', m }),
    schedule: (job, s) => setModal({ kind: 'schedule', job, s }),
    removeSchedule: (s) => setModal({ kind: 'remove-schedule', s }),
    scheduleHistory: (s) =>
      action(async () => {
        const items = await api<Occurrence[]>(idPath('/schedules', s.id) + '/occurrences')
        setModal({ kind: 'history', s, items: items || [] })
      }),
    editConnection: (c) => setModal({ kind: 'connection', c }),
    setupModels: (c) => setModal({ kind: 'models', c }),
    testConnection: (c) =>
      action(async () => {
        if (testing) return
        setTesting(c.id)
        try {
          const result = await api<Connection>(idPath('/connections', c.id) + '/test', 'POST')
          setNotice(
            result.health === 'healthy'
              ? c.name + ' passed the connection test.'
              : c.name + ' · ' + (result.last_error || result.health),
          )
        } finally {
          setTesting('')
        }
      }),
  }
  const props = { cluster, admin, actions }
  const content =
    page === 'Machines' ? (
      <Machines {...props} />
    ) : page === 'Jobs' ? (
      <Jobs {...props} />
    ) : page === 'Runs' ? (
      runId ? (
        <RunDetail
          key={runId}
          id={runId}
          machines={cluster.machines}
          back={() => go('Runs')}
          change={refresh}
        />
      ) : (
        <Runs {...props} />
      )
    ) : page === 'Actions runners' ? (
      <ActionsRunners />
    ) : page === 'Settings' ? (
      <Settings />
    ) : page === 'AI connections' ? (
      <Connections {...props} />
    ) : (
      <Overview {...props} />
    )
  return (
    <div className="app">
      <a
        className="skip"
        href="#main-content"
        onClick={(e) => {
          e.preventDefault()
          document.getElementById('main-content')?.focus()
        }}
      >
        Skip to content
      </a>
      {mobile && (
        <button className="scrim" aria-label="Close navigation" onClick={() => setMobile(false)} />
      )}
      <aside className={'sidebar' + (mobile ? ' open' : '')}>
        <div className="brand-row">
          <Brand />
          <button
            className="icon mobile-only"
            onClick={() => setMobile(false)}
            aria-label="Close navigation"
          >
            <X size={18} />
          </button>
        </div>
        <div className="cluster-id">
          <span className="cluster-icon">
            <Network size={18} />
          </span>
          <div>
            <b>Cluster console</b>
            <small title={location.host}>{location.hostname}</small>
          </div>
        </div>
        <nav aria-label="Main navigation">
          {routes.map((p) => (
            <a
              key={p.name}
              href={'#/' + p.path}
              aria-current={page === p.name ? 'page' : undefined}
              className={page === p.name ? 'active' : ''}
              onClick={() => setMobile(false)}
            >
              <p.icon size={17} />
              {p.name}
              {page === p.name && <i />}
            </a>
          ))}
        </nav>
        <div className="user-block">
          <span className="avatar">{session.username.slice(0, 1).toUpperCase()}</span>
          <div>
            <b>{session.username}</b>
            <small>{admin ? 'Administrator' : 'Operator'}</small>
          </div>
          <button
            className="icon"
            aria-label="Sign out"
            title="Sign out"
            onClick={() =>
              action(async () => {
                await api('/logout', 'POST')
                logout()
              })
            }
          >
            <LogOut size={16} />
          </button>
        </div>
      </aside>
      <div className="workspace">
        <header className="topbar">
          <button
            className="icon mobile-only"
            aria-label="Open navigation"
            onClick={() => setMobile(true)}
          >
            <Menu size={20} />
          </button>
          <span>Workspace</span>
          <ChevronRight size={13} />
          <b>{page}</b>
          {runId && page === 'Runs' && (
            <>
              <ChevronRight size={13} />
              <span>Run details</span>
            </>
          )}
          <div className={'live' + (stale ? ' disconnected' : '')}>
            <i />
            {stale ? 'Disconnected' : loaded ? 'Connected' : 'Connecting'}
          </div>
        </header>
        <main id="main-content" tabIndex={-1} className="main-content">
          {stale && (
            <ErrorMessage>
              {stale} <button onClick={refresh}>Reconnect</button>
            </ErrorMessage>
          )}
          {error && (
            <ErrorMessage>
              {error} <button onClick={() => setError('')}>Dismiss</button>
            </ErrorMessage>
          )}
          {loaded ? content : !stale && <p role="status">Connecting to cluster…</p>}
        </main>
      </div>
      {(notice || testing) && (
        <div className="toast" role="status">
          {testing ? <RefreshCw size={15} className="spin" /> : <Check size={15} />}{' '}
          {testing ? 'Testing connection…' : notice}
        </div>
      )}
      {modal?.kind === 'launch' && (
        <Launch
          machines={cluster.machines}
          jobs={cluster.jobs}
          initial={modal.job}
          close={close}
          saved={saved}
          openRun={openRun}
        />
      )}
      {modal?.kind === 'job' && (
        <JobEditor job={modal.job} connections={modal.connections || cluster.connections} close={close} saved={saved} />
      )}
      {modal?.kind === 'schedule' && (
        <ScheduleEditor
          jobs={cluster.jobs}
          job={modal.job}
          schedule={modal.s}
          close={close}
          saved={saved}
        />
      )}
      {modal?.kind === 'machine' && (
        <MachineEditor machine={modal.m} admin={admin} close={close} saved={saved} />
      )}
      {modal?.kind === 'runner' && <RunnerEditor machine={modal.m} close={close} saved={saved} />}
      {modal?.kind === 'enroll' && <Enrollment close={close} />}
      {modal?.kind === 'connection' && (
        <ConnectionEditor item={modal.c} close={close} saved={saved} />
      )}
      {modal?.kind === 'models' && (
        <ModelSetup connection={modal.c} close={close} changed={refresh} />
      )}
      {modal?.kind === 'history' && (
        <History s={modal.s} items={modal.items} close={close} openRun={openRun} />
      )}
      {modal?.kind === 'revoke' && (
        <Dialog title="Revoke machine" close={close}>
          <Form
            close={close}
            label="Revoke credential"
            danger
            submit={async () => {
              await api(idPath('/machines', modal.m.id) + '/revoke', 'POST')
              saved()
            }}
          >
            <p>
              Revoke the worker credential for <b>{modal.m.name}</b>? Its active process may finish
              locally. To resume claims, enroll the worker again.
            </p>
          </Form>
        </Dialog>
      )}
      {modal?.kind === 'remove-schedule' && (
        <Dialog title="Delete schedule" close={close}>
          <Form
            close={close}
            label="Delete schedule"
            danger
            submit={async () => {
              await api(idPath('/schedules', modal.s.id), 'DELETE')
              saved()
            }}
          >
            <p>
              Delete <b>{modal.s.name}</b> and stop future occurrences?
            </p>
          </Form>
        </Dialog>
      )}
    </div>
  )
}
function App() {
  const [session, setSession] = useState<Session | null>(),
    [error, setError] = useState('')
  const check = async () => {
    try {
      setSession(await api<Session>('/session'))
      setError('')
    } catch (err) {
      setError(err instanceof ApiError && err.status === 401 ? '' : (err as Error).message)
      setSession(null)
    }
  }
  useEffect(() => {
    void check()
    const expire = () => setSession(null)
    window.addEventListener('session-expired', expire)
    return () => window.removeEventListener('session-expired', expire)
  }, [])
  return session === undefined ? (
    <div className="initial">
      <Brand />
      <p>Connecting to cluster…</p>
    </div>
  ) : session ? (
    <Console session={session} logout={() => setSession(null)} />
  ) : (
    <Login done={check} initialError={error} />
  )
}
createRoot(document.getElementById('root')!).render(<App />)
