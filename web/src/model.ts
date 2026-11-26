export type Page = 'Overview' | 'Machines' | 'Jobs' | 'Runs' | 'AI connections' | 'Settings' | 'Actions runners'
export interface Session {
  username: string
  role: 'admin' | 'operator'
}
export interface OpenWebUIStatus {
  configured: boolean
  enabled: boolean
  url?: string
  ollamaBaseUrl?: string
  healthy: boolean
  error?: string
}
export interface Runner {
  control?: { id: string; operation: 'enable' | 'pause'; issuedAt: string; enabledUntil: string }
  settings?: WorkstationSettings
  workstation?: boolean
  name: string
  scope: string
  labels: string[]
  service: string
}
export interface Machine {
  workstation?: {
    settingsVersion: number
    controlVersion?: number
    appliedControl?: string
    local: WorkstationSettings
    effective: WorkstationSettings
    enabledUntil?: string
  }
  id: string
  name: string
  version: string
  os?: 'darwin' | 'linux'
  arch?: 'arm64' | 'amd64'
  roles: string[]
  runnerStatus?: string
  runnerReason?: string
  labels: string[]
  slots: number
  roleLimits: Record<string, number>
  runner?: Runner
  lastHeartbeat: string
  online: boolean
  draining: boolean
  revoked: boolean
  running: number
}
export interface WorkstationSettings {
  buildThreads: number
  maxLoadPerCpu: number
  minAvailableMemoryMb: number
  minIdleSeconds: number
  requireAcPower: boolean
  enableMinutes: number
}
export interface Input {
  name: string
  required: boolean
  paths?: string[]
}
export interface Job {
  id: string
  revision: number
  name: string
  type: 'script' | 'ai'
  role: string
  labels: string[]
  os?: 'darwin' | 'linux'
  arch?: 'arm64' | 'amd64'
  inputs: Input[]
  command?: string
  args?: string[]
  workDir?: string
  timeoutSeconds: number
  prompt?: string
  connection?: string
  model?: string
  allowCloud: boolean
  outputFormat?: string
  outputLimit: number
  maxTokens?: number
  discardOutput: boolean
  safeToRepeat: boolean
  maxAutoRetries?: number
}
export interface Attempt {
  id: string
  number: number
  state: string
  machineId?: string
  queuedAt: string
  startedAt?: string
  endedAt?: string
  leaseUntil?: string
  cancelRequested: boolean
  outcomeUnknown: boolean
  output?: string
  outputTruncated: boolean
  logsTruncated: boolean
  error?: string
}
export interface Run {
  id: string
  jobId: string
  job: Job
  inputs: Record<string, string>
  trigger: string
  scheduleId?: string
  createdAt: string
  state: string
  attempts: Attempt[]
}
export interface Log {
  id: number
  attemptId: string
  at: string
  text: string
  truncated?: boolean
}
export interface Occurrence {
  scheduledAt: string
  localSlot: string
  status: string
  runId?: string
  details?: string
}
export interface Schedule {
  id: string
  name: string
  jobId: string
  cron: string
  timezone: string
  inputs: Record<string, string>
  enabled: boolean
  allowOverlap: boolean
  nextRun?: string
  latest?: Occurrence
}
export interface Connection {
  owner?: string
  id: string
  name: string
  kind: string
  endpoint: string
  models: string[]
  disabled: boolean
  health: string
  last_checked?: string
  last_error?: string
  contextLength?: number
  keepAliveSeconds?: number
  thinking?: boolean
}
export interface Summary {
  queued: number
  running: number
  failed24h: number
  unhealthyConnections: number
  hourly: { start: string; runs: number; failures: number }[]
  recentFailures: {
    id: string
    jobName: string
    jobType: string
    machineId: string
    state: string
    createdAt: string
  }[]
}
export interface Cluster {
  overview: Summary
  machines: Machine[]
  jobs: Job[]
  runs: Run[]
  schedules: Schedule[]
  connections: Connection[]
  github: GitHubActivity
}
export interface GitHubBuild {
  repository: string
  machineId: string
  runnerId: number
  runId: number
  jobId: number
  attempt: number
  workflow: string
  name: string
  branch: string
  sha: string
  status: string
  conclusion?: string
  startedAt?: string
  completedAt?: string
  step?: string
  url: string
}
export interface GitHubActivity {
  repositories?: string[]
  configured: boolean
  repository?: string
  syncedAt?: string
  issue?: string
  builds: GitHubBuild[]
}
export const emptyCluster: Cluster = {
  overview: {
    queued: 0,
    running: 0,
    failed24h: 0,
    unhealthyConnections: 0,
    hourly: [],
    recentFailures: [],
  },
  machines: [],
  jobs: [],
  runs: [],
  schedules: [],
  connections: [],
  github: { configured: false, builds: [] },
}
export const stateNames: Record<string, string> = {
  queued: 'Queued',
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
  timed_out: 'Timed out',
  cancelled: 'Cancelled',
  interrupted: 'Interrupted',
  cancellation_requested: 'Cancellation requested',
  skipped_overlap: 'Skipped · overlap',
}
export const githubBuildState = (b: GitHubBuild) =>
  b.status !== 'completed' ? (b.status === 'in_progress' ? 'running' : b.status) :
    ({ success: 'succeeded', failure: 'failed', cancelled: 'cancelled', timed_out: 'timed_out',
      skipped: 'skipped', neutral: 'neutral', action_required: 'action_required', stale: 'stale' }[b.conclusion || ''] || 'unknown')
export const githubActivityStale = (g: GitHubActivity) =>
  !g.syncedAt || !Number.isFinite(Date.parse(g.syncedAt)) || Date.now() - Date.parse(g.syncedAt) > 90_000
export const kindNames: Record<string, string> = {
  ollama: 'Ollama / Herd',
  openai: 'OpenAI API key',
  chatgpt: 'ChatGPT plan',
  anthropic: 'Anthropic',
  gemini: 'Google Gemini',
}
export const attemptOf = (r: Run) => r.attempts?.at(-1)
export const runStatus = (r: Run) =>
  r.state === 'running' && attemptOf(r)?.cancelRequested ? 'cancellation_requested' : r.state
export const machineStatus = (m: Machine) =>
  m.revoked ? 'revoked' : !m.online ? 'offline' : m.draining ? 'draining' : 'online'
export const platformName = (p: { os?: string; arch?: string }) =>
  [p.os === 'darwin' ? 'macOS' : p.os === 'linux' ? 'Linux' : 'Either OS',
    p.arch === 'arm64' ? 'ARM64' : p.arch === 'amd64' ? 'x86-64' : 'Either architecture'].join(' · ')
export const matchesJob = (m: Machine, j: Job) =>
  !m.revoked && (!j.os || j.os === m.os) && (!j.arch || j.arch === m.arch) &&
  m.roles.includes(j.role) && (j.labels || []).every((label) => m.labels.includes(label))
export const runnerLabels = (m: Machine) =>
  ['self-hosted', m.os === 'darwin' ? 'macOS' : m.os === 'linux' ? 'Linux' : '',
    m.arch === 'arm64' ? 'ARM64' : m.arch === 'amd64' ? 'X64' : ''].filter(Boolean)
export const split = (s: string) =>
  s
    .split(',')
    .map((x) => x.trim())
    .filter(Boolean)
export function when(date?: string | null, timeZone?: string) {
  if (!date) return '—'
  const instant = new Date(date)
  try {
    return instant.toLocaleString(undefined, {
      month: 'short',
      day: 'numeric',
      hour: 'numeric',
      minute: '2-digit',
      timeZone,
    })
  } catch {
    // A server can have more recent IANA zone data than the browser.
    return instant.toISOString()
  }
}
export const lines = (s: string) => (s ? s.split(/\r?\n/) : [])
export const terminal = (state: string) => !['running', 'queued'].includes(state)
