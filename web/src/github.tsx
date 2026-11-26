import { ArrowUpRight, CircleAlert, Github } from 'lucide-react'
import type { Cluster, GitHubBuild, Machine } from './model'
import { githubBuildState, githubActivityStale, when } from './model'
import { Badge, Empty, Panel } from './ui'

function BuildLink({ build }: { build: GitHubBuild }) {
  return (
    <a className="github-build-link" href={build.url} target="_blank" rel="noreferrer">
      <Github size={16} />
      <span>
        <b>{build.workflow} · {build.name}</b>
        <small>{build.branch} · {build.sha.slice(0, 7)} · Attempt {build.attempt}</small>
      </span>
      <ArrowUpRight size={14} />
    </a>
  )
}

export function GitHubActivityPanel({ cluster: c, limit = 100 }: { cluster: Cluster; limit?: number }) {
  const g = c.github
  if (!g.configured) return null
  const stale = githubActivityStale(g)
  const unavailableHint = g.issue?.includes('Actions: read')
    ? 'Grant the configured GitHub App Actions: read access to synchronize build details.'
    : g.issue
      ? 'Open GitHub for build details while the connection recovers.'
      : 'Matching builds will appear after the next GitHub sync.'
  return (
    <Panel
      title="GitHub Actions builds"
      action={<span className="minor">{g.syncedAt ? `Last synced ${when(g.syncedAt)}` : 'Waiting for first sync'}</span>}
    >
      <div className="github-activity-note">
        <p>Builds executed on your Sluicework machines. Open a build for GitHub logs and artifacts.</p>
        {(g.issue || stale) && (
          <div className="provider-warning">
            <CircleAlert size={16} />
            {g.issue || 'Activity is not current. Showing last known build status.'}
          </div>
        )}
      </div>
      {!g.builds.length ? (
        <Empty title={g.issue ? 'Build activity unavailable' : 'No local GitHub builds recorded'}>
          {unavailableHint}
        </Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Build</th>
                <th>{stale || g.issue ? 'Last known status' : 'Status'}</th>
                <th>Machine</th>
                <th>Started</th>
                <th>{stale || g.issue ? 'Last known step / finished' : 'Current step / finished'}</th>
              </tr>
            </thead>
            <tbody>
              {g.builds.slice(0, limit).map(b => (
                <tr key={`${b.repository}:${b.jobId}`}>
                  <td><BuildLink build={b} /></td>
                  <td><Badge value={githubBuildState(b)} /></td>
                  <td>
                    {c.machines.find(m => m.id === b.machineId)?.name || b.machineId.slice(0, 12)}
                    <small className="github-local-host">Local · runner {b.runnerId}</small>
                  </td>
                  <td>{when(b.startedAt)}</td>
                  <td>{b.status === 'completed' ? when(b.completedAt) : b.step || 'Waiting for job details'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {g.builds.length > limit && <p className="github-activity-note minor">See Runs for recent GitHub build history.</p>}
    </Panel>
  )
}

export function MachineBuilds({ cluster: c, machine: m }: { cluster: Cluster; machine: Machine }) {
  const builds = c.github.builds.filter(b => b.machineId === m.id)
  const active = builds.filter(b => b.status !== 'completed')
  return (
    <div className="machine-github-builds">
      {m.online && m.runnerReason && <p className="minor">{m.runnerReason}</p>}
      {m.online && m.runnerStatus === 'draining' && (
        <p className="minor">New work is paused. Any assigned build continues to completion.</p>
      )}
      {active.length > 0 && (
        <p className="minor">
          {githubActivityStale(c.github) || c.github.issue ? 'Last known GitHub activity' : 'Current GitHub build'}
        </p>
      )}
      {(active.length ? active : builds.slice(0, 1)).map(b => (
        <div className="machine-github-build" key={b.jobId}>
          <BuildLink build={b} />
          <Badge value={githubBuildState(b)} />
          <small>{b.status === 'completed' ? `Finished ${when(b.completedAt)}` : b.step || 'Waiting for job details'}</small>
        </div>
      ))}
      {!builds.length && m.online && m.runnerStatus === 'building' && (
        <p className="minor">A build is running. Details are waiting for GitHub sync.</p>
      )}
    </div>
  )
}
