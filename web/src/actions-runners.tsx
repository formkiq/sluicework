import { useEffect, useRef, useState } from 'react'
import { Copy, ExternalLink, Plus, RefreshCw } from 'lucide-react'
import { api } from './api'
import { SpotRunners } from './spot'
import { Dialog, ErrorMessage, Field, Form } from './ui'

type Target = {
  name: string
  platform: string
  machineLabel: string
  managed: boolean
  online: boolean
  busy: boolean
}
type Runner = {
  id: number
  name: string
  os: string
  status: string
  busy: boolean
  scope: string
  managed: boolean
  available: boolean
  machineId?: string
  labels: { name: string; type: string }[]
  group?: {
    id: number
    name: string
    inherited: boolean
    restricted_to_workflows: boolean
  }
}
type Inventory = {
  configured: boolean
  repository: string
  organizationEnabled: boolean
  organizationIssue?: string
  runners: Runner[]
  targets: Target[]
}
type Policy = {
  version: number
  placement: string
  platform: string
  machineLabel: string
  priority?: 'high' | 'low'
  fallbackQueueMinutes?: number
  queueMinutes: number
  fallbackRunner?: { platform: string; machineLabel: string }
}
type Routing = {
  repository: string
  integration: { version: number; workflow: string; platforms: string[] }
  policy: Policy
  source: string
  revision: string
}
type Setup = {
  scope: string
  url: string
  name: string
  label: string
  token: string
  expires_at: string
  application: {
    filename: string
    download_url: string
    sha256_checksum: string
  }
}
const targetKey = (t: { platform: string; machineLabel: string }) =>
  t.platform + '|' + t.machineLabel
const repositoryQuery = (repository: string) => '?repository=' + encodeURIComponent(repository)
const runnerPath = (r: Runner, repository: string, suffix = '') => `/github/runners/${r.scope}/${r.id}${suffix}${repositoryQuery(repository)}`
function WorkflowExample({
  runner,
  targets,
}: {
  runner: Runner
  targets: Target[]
}) {
  const target = targets.find(
    (t) =>
      (t.name === runner.name || (runner.managed && t.managed)) &&
      runner.labels.some((l) => l.name === t.machineLabel),
  )
  return (
    <details>
      <summary>Workflow example</summary>
      {target ? (
        <pre className="runner-example">{`runs-on: ${JSON.stringify([...runner.labels.filter((l) => l.type === 'read-only').map((l) => l.name), target.machineLabel])}`}</pre>
      ) : runner.available && runner.group?.name === 'Default' && runner.labels.some((l) => l.name.toLowerCase() === 'linux') && runner.labels.some((l) => l.name.toLowerCase() === 'self-hosted') && !runner.group?.restricted_to_workflows ? (
        <pre className="runner-example">{'runs-on:\n  group: Default\n  labels: [self-hosted, Linux]'}</pre>
      ) : runner.group?.restricted_to_workflows ? (
        <p>
          Review this group's workflow restrictions in GitHub. Restricted groups
          are excluded from automatic placement here.
        </p>
      ) : (
        <p>
          Add a unique custom label, confirm OS/architecture labels, and allow
          this repository in the runner group before selecting this machine.
        </p>
      )}
      <p>
        All labels must match. Workflow YAML changes are reviewed in the
        repository.
      </p>
    </details>
  )
}

function Placement({
  routing: initial,
  inventory,
  saved,
}: {
  routing: Routing
  inventory: Inventory
  saved: (r: Routing) => void
}) {
  const [policy, setPolicy] = useState(
    initial.policy.version === 3 && initial.policy.priority === 'high'
      ? { ...initial.policy, queueMinutes: 15 }
      : initial.policy,
  )
  const targets = inventory.targets.filter((t) => initial.integration.platforms.includes(t.platform) && (t.platform !== 'linux' || policy.version === 3))
  return (
    <section className="panel setup-panel">
      <h2>Repository build placement</h2>
      <p>
        Publishes to GitHub for future runs of <code>{initial.integration.workflow}</code>. GitHub keeps using the saved
        policy when this admin is offline. Other workflows keep their own <code>runs-on</code>.
      </p>
      <p className="muted">
        Current source: {initial.source}. Accepted queue requests and running builds keep their original placement.
      </p>
      {initial.integration.version >= 3 && (
        <p className="callout">
          Priority placement needs the selected dispatcher online. Workstation installations use the AWS service so this
          Mac can sleep without stopping queue decisions.
        </p>
      )}
      <Form
        label="Save placement to GitHub"
        cancelLabel="Reset"
        close={() => setPolicy(initial.policy)}
        submit={async () =>
          saved(
            await api('/github/routing' + repositoryQuery(initial.repository), 'PUT', {
              policy,
              revision: initial.revision,
            }),
          )
        }
      >
        <Field label="Build placement">
          <select
            value={policy.placement}
            onChange={(e) =>
              setPolicy(
                e.target.value === 'cloud-only' && policy.version < 3
                  ? { ...policy, version: 1, placement: e.target.value, fallbackRunner: undefined }
                  : { ...policy, placement: e.target.value },
              )
            }
          >
            <option value="prefer-local">Try selected runners in order; then GitHub-hosted</option>
            <option value="local-only">Selected runners only</option>
            <option value="cloud-only">GitHub-hosted only</option>
          </select>
        </Field>
        {policy.placement !== 'cloud-only' && (
          <>
            <Field
              label="Preferred runner"
              hint="The Linux pool uses the Default runner group with only self-hosted and Linux labels, or select an explicit machine by OS, architecture and custom label. The existing queue timeout applies."
            >
              <select
                required
                value={targetKey(policy)}
                onChange={(e) => {
                  const t = targets.find((t) => targetKey(t) === e.target.value)!
                  setPolicy({
                    ...policy,
                    platform: t.platform,
                    machineLabel: t.machineLabel,
                    ...(policy.version < 3 &&
                    policy.fallbackRunner?.machineLabel.toLowerCase() === t.machineLabel.toLowerCase()
                      ? { version: 1, fallbackRunner: undefined }
                      : {}),
                  })
                }}
              >
                {!targets.some((t) => targetKey(t) === targetKey(policy)) && (
                  <option value={targetKey(policy)}>
                    Current target: {policy.machineLabel} ({policy.platform}; unavailable)
                  </option>
                )}
                {targets.map((t) => (
                  <option key={targetKey(t)} value={targetKey(t)}>
                    {t.name} · {t.platform} · {t.managed ? 'Sluicework' : 'External'} · {t.machineLabel} ·{' '}
                    {t.online ? (t.busy ? 'Busy' : 'Online') : 'Offline'}
                  </option>
                ))}
              </select>
            </Field>
            <p>
              {policy.platform === 'linux' ? <>Required labels: <code>self-hosted, Linux</code>. Any available architecture in the Default group can run this build.</> : <>Required label: <code>{policy.machineLabel}</code>.</>} External hosts keep their own resource and service
              controls.
            </p>
            {initial.integration.version >= 2 && (
              <Field
                label={policy.version === 3 ? 'Second runner' : 'Second runner (optional)'}
                hint="Try this runner after the preferred runner's queue window expires. GitHub-hosted capacity follows both when fallback is enabled."
              >
                <select
                  value={policy.fallbackRunner ? targetKey(policy.fallbackRunner) : ''}
                  onChange={(e) => {
                    const t = targets.find((t) => targetKey(t) === e.target.value)
                    setPolicy({
                      ...policy,
                      version: policy.version === 3 ? 3 : t ? 2 : 1,
                      fallbackRunner: t ? { platform: t.platform, machineLabel: t.machineLabel } : undefined,
                    })
                  }}
                >
                  {policy.version < 3 && <option value="">No second runner</option>}
                  {policy.fallbackRunner &&
                    !targets.some((t) => targetKey(t) === targetKey(policy.fallbackRunner!)) && (
                      <option value={targetKey(policy.fallbackRunner)}>
                        Current target: {policy.fallbackRunner.machineLabel} ({policy.fallbackRunner.platform};
                        unavailable)
                      </option>
                    )}
                  {targets
                    .filter((t) => t.platform !== 'linux' && t.machineLabel.toLowerCase() !== policy.machineLabel.toLowerCase())
                    .map((t) => (
                      <option key={targetKey(t)} value={targetKey(t)}>
                        {t.name} · {t.platform} · {t.machineLabel} ·{' '}
                        {t.online ? (t.busy ? 'Busy' : 'Online') : 'Offline'}
                      </option>
                    ))}
                </select>
              </Field>
            )}
            <p>
              Order:{' '}
              <strong>
                {policy.platform === 'linux' ? 'Any self-hosted Linux runner' : policy.machineLabel}
                {policy.fallbackRunner ? ` → ${policy.fallbackRunner.machineLabel}` : ''}
                {policy.placement === 'prefer-local' ? ' → GitHub-hosted capacity' : ''}
              </strong>
            </p>
            {initial.integration.version >= 3 && (
              <Field
                label="Repository priority"
                hint="Higher-priority queued builds run first. Running builds finish normally. Deadlines run in the dispatcher, without a hosted runner waiting."
              >
                <select
                  value={policy.version === 3 ? policy.priority : 'legacy'}
                  onChange={(e) => {
                    const priority = e.target.value as 'high' | 'low'
                    setPolicy({
                      ...policy,
                      version: 3,
                      priority,
                      queueMinutes: priority === 'high' ? 15 : 60,
                      fallbackQueueMinutes: policy.fallbackQueueMinutes || 2,
                    })
                  }}
                >
                  {policy.version < 3 && (
                    <option value="legacy" disabled>
                      Legacy placement — choose a queue class
                    </option>
                  )}
                  <option value="high">High — preferred runner for up to 15 minutes</option>
                  <option value="low">Low — preferred runner for up to 60 minutes</option>
                </select>
              </Field>
            )}
            {policy.version === 3 ? (
              <Field
                label="Second runner queue window (minutes)"
                hint="The second runner is tried only when online and idle. An unassigned attempt can wait this long before hosted fallback."
              >
                <input
                  type="number"
                  min={1}
                  max={5}
                  required
                  value={policy.fallbackQueueMinutes || 2}
                  onChange={(e) => setPolicy({ ...policy, fallbackQueueMinutes: Number(e.target.value) })}
                />
              </Field>
            ) : (
              <Field
                label="Queue timeout (minutes)"
                hint="Applies separately to each selected runner. Only an unassigned, confirmed-cancelled build can fall back. Assigned or failed builds are never duplicated."
              >
                <input
                  type="number"
                  min={1}
                  max={15}
                  required
                  value={policy.queueMinutes}
                  onChange={(e) => setPolicy({ ...policy, queueMinutes: Number(e.target.value) })}
                />
              </Field>
            )}
          </>
        )}
        <p>
          Existing workflow rules still decide which PRs may use self-hosted capacity and which jobs deploy. Logs and
          job history remain on GitHub.
        </p>
      </Form>
    </section>
  )
}

function Labels({
  runner,
  repository,
  close,
  saved,
}: {
  repository: string
  runner: Runner
  close: () => void
  saved: () => void
}) {
  return (
    <Dialog title={'Labels for ' + runner.name} close={close}>
      <Form
        close={close}
        label="Save runner labels"
        submit={async (d) => {
          await api(runnerPath(runner, repository, '/labels'), 'PUT', {
            labels: String(d.get('labels') || '')
              .split(',')
              .map((s) => s.trim())
              .filter(Boolean),
          })
          saved()
        }}
      >
        <p>
          GitHub owns the default OS and architecture labels. Custom labels
          control future workflow matching. Keep labels used by existing
          workflows.
        </p>
        {runner.scope === 'organization' && (
          <p className="callout">
            This organization runner may serve other repositories; label changes
            affect their matching too.
          </p>
        )}
        <Field
          label="Custom runner labels"
          hint="Comma-separated letters, digits, dots, underscores and hyphens. Add a unique label such as linux-runner-01; Sluicework labels are reserved."
        >
          <input
            name="labels"
            defaultValue={runner.labels
              .filter((l) => l.type === 'custom')
              .map((l) => l.name)
              .join(', ')}
          />
        </Field>
      </Form>
    </Dialog>
  )
}
function Remove({
  runner,
  repository,
  close,
  saved,
}: {
  repository: string
  runner: Runner
  close: () => void
  saved: () => void
}) {
  return (
    <Dialog title={'Remove ' + runner.name} close={close}>
      <Form
        close={close}
        label="Remove GitHub registration"
        danger
        submit={async (d) => {
          await api(runnerPath(runner, repository), 'DELETE', {
            confirmName: String(d.get('name')),
          })
          saved()
        }}
      >
        <p>
          Removes this registration from GitHub. Stop or uninstall its runner
          service on the host first and wait for Offline. Assigned work must
          finish first. Update placement and any workflows targeting this runner
          before removing it.
        </p>
        {runner.scope === 'organization' && (
          <p className="callout">
            This removes the organization runner for every repository using it.
          </p>
        )}
        <Field label="Confirm runner name" hint={'Type ' + runner.name}>
          <input name="name" required autoComplete="off" />
        </Field>
      </Form>
    </Dialog>
  )
}
function Registration({
  organizationEnabled,
  repository,
  close,
}: {
  repository: string
  organizationEnabled: boolean
  close: () => void
}) {
  const [setup, setSetup] = useState<Setup>()
  const [show, setShow] = useState(false)
  const [message, setMessage] = useState('')
  const quote = (s: string) => "'" + s.replaceAll("'", "'\\''") + "'"
  const commands = setup
    ? `(\nset -eu\nmkdir actions-runner && cd actions-runner\ncurl --fail --location --output ${quote(setup.application.filename)} ${quote(setup.application.download_url)}\nprintf '%s\\n' ${quote(setup.application.sha256_checksum + '  ' + setup.application.filename)} | shasum -a 256 -c -\ntar xzf ${quote(setup.application.filename)}\n./config.sh --url ${quote(setup.url)} --name ${quote(setup.name)} --labels ${quote(setup.label)}\n./run.sh\n)`
    : ''
  return (
    <Dialog title="Register an external GitHub runner" close={close}>
      {!setup ? (
        <Form
          close={close}
          label="Prepare GitHub registration"
          submit={async (d) =>
            setSetup(
              await api(
                '/github/runners/registration' + repositoryQuery(repository),
                'POST',
                Object.fromEntries(d),
              ),
            )
          }
        >
          <p>
            Uses GitHub’s official runner. No Sluicework worker or admin is
            installed on that host.
          </p>
          <Field label="Registration scope">
            <select name="scope">
              <option value="repository">This repository</option>
              {organizationEnabled && (
                <option value="organization">
                  Organization (runner-group access applies)
                </option>
              )}
            </select>
          </Field>
          <Field label="Runner name">
            <input
              name="name"
              placeholder="linux-runner-01"
              pattern={'[A-Za-z0-9][A-Za-z0-9_.\\-]{0,99}'}
              required
            />
          </Field>
          <Field label="Unique routing label">
            <input
              name="label"
              placeholder="linux-runner-01"
              pattern={'[A-Za-z0-9][A-Za-z0-9_.\\-]{0,99}'}
              required
            />
          </Field>
          <Field label="Operating system">
            <select name="os">
              <option value="linux">Linux</option>
              <option value="osx">macOS</option>
            </select>
          </Field>
          <Field label="Architecture">
            <select name="arch">
              <option value="x64">x86-64</option>
              <option value="arm64">ARM64</option>
            </select>
          </Field>
        </Form>
      ) : (
        <div className="form-body">
          <p>
            Registration target: <b>{setup.url}</b>. Token expires{' '}
            {new Date(setup.expires_at).toLocaleString()}. It is shown only in
            this setup session.
          </p>
          <Field
            label="GitHub registration token"
            hint="Paste at config.sh’s token prompt. The commands below do not include the token."
          >
            <input
              readOnly
              type={show ? 'text' : 'password'}
              value={setup.token}
            />
          </Field>
          <div className="actions">
            <button onClick={() => setShow(!show)}>
              {show ? 'Hide token' : 'Show token'}
            </button>
            <button
              onClick={async () => {
                try {
                  await navigator.clipboard.writeText(setup.token)
                  setMessage('Token copied.')
                } catch {
                  setShow(true)
                  setMessage('Select and copy the token above.')
                }
              }}
            >
              <Copy size={14} /> Copy token
            </button>
          </div>
          <p>
            Run these commands as the intended non-root runner account, in a new
            directory on the chosen host. Install GitHub’s documented OS
            prerequisites first. Follow each successful command before
            proceeding.
          </p>
          <pre className="runner-example">{commands}</pre>
          <p>
            For a background service, stop the foreground runner and use
            GitHub’s <code>svc.sh</code> setup on that host. Organization runner
            groups must allow the intended repository and workflow. Return here
            and refresh after registration.
          </p>
          <p role="status">{message}</p>
          <a
            target="_blank"
            rel="noreferrer"
            href="https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/add-runners"
          >
            GitHub runner setup instructions <ExternalLink size={12} />
          </a>
        </div>
      )}
    </Dialog>
  )
}

export function ActionsRunners() {
  const [repository, setRepository] = useState('')
  const [repositories, setRepositories] = useState<string[]>([])
  const request = useRef(0)
  useEffect(() => {
    void api<{primaryRepository: string; repositories: string[]}>('/github/repositories').then((data) => {
      setRepositories(data.repositories)
      setRepository(data.primaryRepository)
    }).catch((e) => setError(e.message))
  }, [])
  const [inventory, setInventory] = useState<Inventory>()
  const [routing, setRouting] = useState<Routing>()
  const [error, setError] = useState('')
  const [routingError, setRoutingError] = useState('')
  const [loading, setLoading] = useState(false)
  const [notice, setNotice] = useState('')
  const [checkedAt, setCheckedAt] = useState('')
  const [modal, setModal] = useState<
    { type: 'labels' | 'remove'; runner: Runner } | { type: 'registration' }
  >()
  const load = async () => {
    if (!repository) return
    const sequence = ++request.current
    setLoading(true)
    const [runners, placement] = await Promise.allSettled([
      api<Inventory>('/github/runners' + repositoryQuery(repository)),
      api<Routing>('/github/routing' + repositoryQuery(repository)),
    ])
    if (sequence !== request.current) return
    if (runners.status === 'fulfilled') {
      setInventory(runners.value)
      setCheckedAt(new Date().toLocaleString())
      setError('')
    } else {
      setError(runners.reason.message)
      setInventory(undefined)
    }
    if (placement.status === 'fulfilled') {
      setRouting(placement.value)
      setRoutingError('')
    } else {
      setRoutingError(placement.reason.message)
      setRouting(undefined)
    }
    setLoading(false)
  }
  useEffect(() => {
    void load()
    return () => { request.current++ }
  }, [repository])
  const saved = () => {
    setModal(undefined)
    setNotice('GitHub runner updated.')
    void load()
  }
  return (
    <>
      <div className="heading actions-runners-heading">
        <div>
          <h1>Actions runners</h1>
          <p>
            Manage GitHub runner registrations and repository build placement.
          </p>
        </div>
        <div className="actions">
          <button disabled={loading} onClick={load}>
            <RefreshCw size={15} /> Refresh
          </button>
          {inventory && (
            <button
              className="primary"
              onClick={() => setModal({ type: 'registration' })}
            >
              <Plus size={15} /> Add external runner
            </button>
          )}
        </div>
      </div>
      <p>
        GitHub workflow files define jobs and permissions. Sluicework can manage
        compatible workflows’ published placement policy and GitHub runner
        labels. External runners retain their own host/service/resource
        controls.
      </p>
      <SpotRunners />
      {repositories.length > 1 && (
        <Field label="Repository">
          <select value={repository} onChange={(e) => {
            setRepository(e.target.value)
            setInventory(undefined)
            setRouting(undefined)
            setModal(undefined)
            setNotice('')
          }}>
            {repositories.map((repo) => <option key={repo} value={repo}>{repo}</option>)}
          </select>
        </Field>
      )}
      {notice && (
        <p role="status" className="callout">
          {notice}
        </p>
      )}
      {error && (
        <ErrorMessage>
          {error} <a href="#/settings">Open GitHub connection settings</a>
        </ErrorMessage>
      )}
      {inventory && (
        <>
          <p>
            <b>{inventory.repository}</b> · Last refreshed {checkedAt} ·{' '}
            <a
              href={`https://github.com/${inventory.repository}/settings/actions/runners`}
              target="_blank"
              rel="noreferrer"
            >
              GitHub runners <ExternalLink size={12} />
            </a>
            {' · '}
            <a
              href={`https://github.com/${inventory.repository}/actions`}
              target="_blank"
              rel="noreferrer"
            >
              Workflow runs <ExternalLink size={12} />
            </a>
          </p>
          {!inventory.organizationEnabled && (
            <p className="callout">
              For organization runners such as Linux runner, enable organization
              runner management in{' '}
              <a href="#/settings">Settings → GitHub App connection</a>, grant
              organization Self-hosted runners: read/write, and approve the
              installation.
            </p>
          )}
          {inventory.organizationIssue && (
            <ErrorMessage>{inventory.organizationIssue}</ErrorMessage>
          )}
          <div className="machine-grid">
            {inventory.runners.map((r) => (
              <article
                className="panel machine actions-runner"
                key={r.scope + r.id}
              >
                <div className="machine-main">
                  <div className="heading">
                    <h2>{r.name}</h2>
                    <span
                      className={
                        'badge ' +
                        (r.status === 'online' ? 'online' : 'offline')
                      }
                    >
                      {r.busy
                        ? 'Busy'
                        : r.status === 'online'
                          ? 'Online'
                          : 'Offline'}
                    </span>
                  </div>
                  <p>
                    {r.os} ·{' '}
                    {r.scope === 'organization'
                      ? 'Organization runner'
                      : 'Repository runner'}{' '}
                    · {r.managed ? 'Sluicework managed' : 'External'}
                  </p>
                  {r.group && (
                    <p>
                      Group: {r.group.name}
                      {r.group.inherited ? ' (inherited)' : ''}
                      {r.group.restricted_to_workflows
                        ? ' · workflow restrictions apply'
                        : ''}
                    </p>
                  )}
                  {!r.available && (
                    <p className="callout">
                      Runner group does not allow this repository. Update its
                      access in GitHub before selecting it.
                    </p>
                  )}
                  <div className="tag-list">
                    {r.labels.map((l) => (
                      <code key={l.name}>
                        {l.name}
                        {l.type === 'read-only' ? ' · GitHub default' : ''}
                      </code>
                    ))}
                  </div>
                  <WorkflowExample runner={r} targets={inventory.targets} />
                  {r.managed ? (
                    <a href="#/machines">Manage Sluicework machine →</a>
                  ) : r.group?.inherited ? (
                    <p>
                      Manage this inherited runner in its owning organization or
                      enterprise.
                    </p>
                  ) : (
                    <div className="actions">
                      <button
                        onClick={() => setModal({ type: 'labels', runner: r })}
                      >
                        Edit labels
                      </button>
                      <button
                        className="danger"
                        disabled={r.busy || r.status !== 'offline'}
                        onClick={() => setModal({ type: 'remove', runner: r })}
                      >
                        Remove registration
                      </button>
                    </div>
                  )}
                  {!r.managed && r.status !== 'offline' && (
                    <p className="muted">
                      Stop the runner service on its host and wait for Offline
                      before removing its registration.
                    </p>
                  )}
                </div>
              </article>
            ))}
          </div>
          {inventory.runners.length === 0 && (
            <p>
              No registered runners found. A paused Sluicework workstation may
              have no GitHub listener until capacity is offered.
            </p>
          )}
        </>
      )}
      {routing && inventory && routing.repository === inventory.repository ? (
        <Placement
          key={routing.revision}
          routing={routing}
          inventory={inventory}
          saved={(r) => {
            setRouting(r)
            setNotice(
              'Placement saved to GitHub. Future supported runs will use it.',
            )
          }}
        />
      ) : (
        (routingError ||
          (routing &&
            inventory &&
            routing.repository !== inventory.repository)) && (
          <section className="panel setup-panel">
            <h2>Repository build placement</h2>
            <ErrorMessage>
              {routingError ||
                'GitHub connection changed. Refresh before editing placement.'}
            </ErrorMessage>
            <p>
              Runner management works independently. Placement needs a version 1
              workflow integration and repository Contents: read / Variables:
              write permissions. Existing workflows keep their current policy.
            </p>
          </section>
        )
      )}
      {modal?.type === 'registration' && (
        <Registration
          repository={repository}
          organizationEnabled={inventory?.organizationEnabled || false}
          close={() => setModal(undefined)}
        />
      )}
      {modal?.type === 'labels' && (
        <Labels
          repository={repository}
          runner={modal.runner}
          close={() => setModal(undefined)}
          saved={saved}
        />
      )}
      {modal?.type === 'remove' && (
        <Remove
          repository={repository}
          runner={modal.runner}
          close={() => setModal(undefined)}
          saved={saved}
        />
      )}
    </>
  )
}
