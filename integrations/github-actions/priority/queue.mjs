// Vendored from Sluicework. No GitHub App key or inbound workstation URL in CI.
import { randomUUID } from 'node:crypto'
import { appendFileSync, writeFileSync } from 'node:fs'
import { isDeepStrictEqual } from 'node:util'
const sha = (v) => typeof v === 'string' && /^[a-f0-9]{40}$/.test(v)
const positive = (v) => Number.isSafeInteger(v) && v > 0
const uuid = (v) =>
  typeof v === 'string' && /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/.test(v)
const branch = (v) => typeof v === 'string' && /^[A-Za-z0-9_./-]+$/.test(v) && !v.includes('..') && !v.startsWith('/')
function requireThat(value, message) {
  if (!value) throw new Error(message)
}
export function queueOptions(env, defaults) {
  const p = env.SLUICEWORK_ROUTING ? JSON.parse(env.SLUICEWORK_ROUTING) : defaults
  if (p?.version !== 3) return null
  requireThat(
    Object.keys(p).sort().join(',') ===
      'fallbackQueueMinutes,fallbackRunner,machineLabel,placement,platform,priority,queueMinutes,version' &&
      ['high', 'low'].includes(p.priority) &&
      (p.priority === 'high' ? [5, 15].includes(p.queueMinutes) : p.queueMinutes === 60) &&
      Number.isInteger(p.fallbackQueueMinutes) &&
      p.fallbackQueueMinutes >= 1 &&
      p.fallbackQueueMinutes <= 5 &&
      ['prefer-local', 'local-only', 'cloud-only'].includes(p.placement),
    'Invalid priority policy',
  )
  const valid = (t) =>
    t &&
    ['linux-x64', 'linux-arm64', 'macos-arm64', 'macos-x64'].includes(t.platform) &&
    /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}$/.test(t.machineLabel || '') &&
    !['self-hosted', 'linux', 'macos', 'windows', 'x64', 'arm64', 'arm'].includes(t.machineLabel.toLowerCase())
  requireThat(
    (valid(p) || (p.platform === 'linux' && p.machineLabel === 'self-hosted')) &&
      valid(p.fallbackRunner) &&
      Object.keys(p.fallbackRunner).sort().join(',') === 'machineLabel,platform' &&
      p.machineLabel.toLowerCase() !== p.fallbackRunner.machineLabel.toLowerCase(),
    'Invalid queue targets',
  )
  requireThat(
    !env.LOCAL_PLATFORM && !env.MACHINE_LABEL && !env.LOCAL_QUEUE_MINUTES,
    'Manage priority queue targets and deadlines in Sluicework admin; clear manual runner overrides',
  )
  const placement = !env.PLACEMENT || env.PLACEMENT === 'configured' ? p.placement : env.PLACEMENT
  requireThat(
    ['prefer-local', 'local-only', 'cloud-only'].includes(placement) && sha(env.SOURCE_SHA || env.GITHUB_SHA),
    'Invalid queued build source or placement',
  )
  return {
    queueVersion: 3,
    policy: placement,
    sha: env.SOURCE_SHA || env.GITHUB_SHA,
    kind: env.BUILD_KIND || 'site',
    priority: p.priority,
  }
}
export async function queueReady(api, manifest, now = Date.now()) {
  const b = manifest.dispatcher
  requireThat(
    manifest.version === 3 && positive(b?.appId) && /^[\w.-]{1,100}$/.test(b.backendId || '') && sha(b.heartbeatSha),
    'Missing trusted dispatcher binding',
  )
  const deployedFile = await api(
    'GET',
    `/contents/.github/sluicework.json?ref=${encodeURIComponent(manifest.defaultBranch || 'main')}`,
  )
  const deployed = JSON.parse(Buffer.from(deployedFile.content, 'base64').toString('utf8'))
  requireThat(
    deployed.version === 3 && isDeepStrictEqual(deployed.dispatcher, b),
    'Priority integration is not installed on the default branch yet',
  )
  const checks = await allChecks(api, b.heartbeatSha, 'Sluicework dispatcher')
  const matches = checks.filter(
    (c) =>
      c.app?.id === b.appId &&
      c.external_id === `sluicework-dispatcher:${b.backendId}` &&
      c.head_sha === b.heartbeatSha,
  )
  requireThat(matches.length === 1, 'Dispatcher heartbeat is missing or ambiguous')
  const health = JSON.parse(matches[0].output?.text || '{}')
  const age = now - Date.parse(health.at)
  requireThat(
    health.version === 3 &&
      health.backendId === b.backendId &&
      health.ready === true &&
      age >= -30_000 &&
      age <= 6 * 60_000,
    'Priority dispatcher is offline; retry after it is healthy',
  )
}
export async function enqueue({ api, manifest, policy, env, event, config, ref, id = randomUUID() }) {
  await queueReady(api, { ...manifest, defaultBranch: policy.defaultBranch })
  const fileName = (env.GITHUB_WORKFLOW_REF || '').split('@')[0].split('/').at(-1)
  const rule = policy.coordinators[fileName]
  requireThat(
    rule && rule.kind === config.kind && uuid(id) && branch(ref) && sha(config.workflowSha || config.sha),
    'Unknown queued coordinator',
  )
  const request = {
    version: 3,
    backendId: manifest.dispatcher.backendId,
    id,
    parentId: Number(env.GITHUB_RUN_ID),
    parentAttempt: Number(env.GITHUB_RUN_ATTEMPT),
    checkId: 0,
    checkSha: event.pull_request?.head?.sha || env.GITHUB_SHA,
    sourceSha: config.sha,
    workflowSha: config.workflowSha || config.sha,
    ref,
    kind: rule.kind,
    placement: config.policy,
    prNumber: event.pull_request?.number || 0,
    baseSha: event.pull_request?.base?.sha || '',
  }
  requireThat(
    positive(request.parentId) && positive(request.parentAttempt) && sha(request.checkSha),
    'Invalid queue parent identity',
  )
  const check = await api('POST', '/check-runs', {
    name: rule.check,
    head_sha: request.checkSha,
    status: 'in_progress',
    external_id: `sluicework-v3:${request.parentId}:${request.parentAttempt}:${id}`,
    output: {
      title: 'Build queued',
      summary: `${config.priority} priority; shared dispatcher selects the runner. No hosted runner waits for capacity.`,
    },
  })
  requireThat(positive(check.id), 'Missing queued build check')
  request.checkId = check.id
  const path = `${env.RUNNER_TEMP}/sluicework-request.json`
  writeFileSync(path, JSON.stringify(request), { mode: 0o600 })
  if (env.GITHUB_OUTPUT) appendFileSync(env.GITHUB_OUTPUT, `detached=true\nqueued=true\nhandoff_path=${path}\n`)
  if (env.GITHUB_STEP_SUMMARY)
    appendFileSync(
      env.GITHUB_STEP_SUMMARY,
      `Build queued (${config.priority} priority). The **${rule.check}** check remains pending through worker completion and verification.\n`,
    )
  return request
}
async function allChecks(api, commit, name) {
  const checks = []
  for (let page = 1; page <= 10; page++) {
    const list = await api(
      'GET',
      `/commits/${commit}/check-runs?check_name=${encodeURIComponent(name)}&filter=all&per_page=100&page=${page}`,
    )
    checks.push(...list.check_runs)
    if (checks.length >= list.total_count) return checks
  }
  throw new Error('Too many checks to authenticate')
}
export async function resolveQueuedCompletion(
  api,
  policy,
  repository,
  trigger,
  { parent, rule, file, worker, fromChild, retry, checks },
) {
  const binding = policy.dispatcher
  if (!binding) return null
  const prefix = `sluicework-v3:${parent.id}:${parent.run_attempt}:`
  const candidates = checks.filter(
    (c) =>
      c.app?.slug === 'github-actions' &&
      c.name === rule.check &&
      c.head_sha === parent.head_sha &&
      c.external_id?.startsWith(prefix),
  )
  if (!candidates.length) return null
  requireThat(candidates.length === 1, 'Ambiguous queued required check')
  const check = candidates[0],
    id = check.external_id.slice(prefix.length)
  requireThat(uuid(id), 'Invalid queued correlation')
  if (
    (check.status === 'completed' && !retry) ||
    checks.some(
      (c) =>
        c.app?.slug === 'github-actions' &&
        c.name === rule.check &&
        /^sluicework-v[13]:/.test(c.external_id || '') &&
        c.id > check.id,
    )
  )
    return null
  if (trigger.queueRequestId && trigger.queueRequestId !== id) return null
  if (parent.status === 'completed' && parent.conclusion !== 'success') {
    // A failed upload cannot be accepted by the dispatcher. Failing its own
    // required check needs no worker authorization and can never deploy.
    return {
      check,
      parent,
      rule,
      workerSuccess: false,
      shouldDeploy: false,
      receipt: {
        version: 1,
        repository,
        parentId: parent.id,
        parentAttempt: parent.run_attempt,
        parentWorkflow: file,
        kind: rule.kind,
        checkSha: parent.head_sha,
        sourceSha: parent.head_sha,
        workflowSha: parent.head_sha,
        ref: parent.head_branch,
        prNumber: 0,
        baseSha: '',
        child: null,
      },
    }
  }
  const receipts = await allChecks(api, parent.head_sha, 'Sluicework queue')
  const selected = receipts.filter(
    (c) =>
      c.app?.id === binding.appId &&
      c.head_sha === parent.head_sha &&
      c.external_id === `sluicework-queue-v3:${parent.id}:${parent.run_attempt}:${id}`,
  )
  if (!selected.length) return null // dispatcher has not published this transition yet
  requireThat(selected.length === 1 && (selected[0].output?.text || '').length < 60000, 'Invalid dispatcher receipt')
  const published = JSON.parse(selected[0].output.text),
    r = published.request
  requireThat(
    published.version === 3 &&
      published.backendId === binding.backendId &&
      r?.id === id &&
      r.repository === repository &&
      r.parentId === parent.id &&
      r.parentAttempt === parent.run_attempt &&
      r.parentWorkflow === file &&
      r.kind === rule.kind &&
      r.checkId === check.id &&
      r.checkSha === parent.head_sha &&
      positive(r.artifactId) &&
      sha(r.sourceSha) &&
      sha(r.workflowSha) &&
      branch(r.ref),
    'Dispatcher receipt differs from parent',
  )
  if (!['completed', 'failed'].includes(r.state)) return null
  const list = await api('GET', `/actions/runs/${parent.id}/artifacts?per_page=100`)
  const matches = list.artifacts.filter((a) => a.name === `sluicework-request-${parent.run_attempt}`)
  requireThat(list.total_count <= 100 && matches.length === 1, 'Missing authenticated queue request')
  const parentArtifact = matches[0]
  requireThat(
    parentArtifact.id === r.artifactId &&
      parentArtifact.expired === false &&
      parentArtifact.workflow_run?.id === parent.id &&
      parentArtifact.workflow_run?.head_sha === parent.head_sha &&
      /^sha256:[a-f0-9]{64}$/.test(parentArtifact.digest || '') &&
      parentArtifact.size_in_bytes > 0 &&
      parentArtifact.size_in_bytes <= 32768,
    'Invalid immutable queue request',
  )
  // The original upload is downloaded and compared again by claim/deploy jobs.
  const receipt = {
    version: 1,
    repository,
    parentId: parent.id,
    parentAttempt: parent.run_attempt,
    parentWorkflow: file,
    kind: r.kind,
    checkSha: r.checkSha,
    sourceSha: r.sourceSha,
    workflowSha: r.workflowSha,
    ref: r.ref,
    prNumber: 0,
    baseSha: '',
    child: null,
  }
  // Request PR metadata is proven by the App, then rechecked against the API.
  let prNumber = 0,
    baseSha = ''
  if (parent.event === 'pull_request') {
    requireThat(parent.pull_requests?.length === 1, 'Missing queued PR binding')
    prNumber = parent.pull_requests[0].number
    const pr = await api('GET', `/pulls/${prNumber}`)
    requireThat(
      pr.head?.repo?.full_name === repository &&
        pr.base?.repo?.full_name === repository &&
        pr.base.ref === policy.defaultBranch &&
        pr.user?.type === 'User' &&
        parent.actor?.type === 'User' &&
        ['OWNER', 'MEMBER', 'COLLABORATOR'].includes(pr.author_association),
      'Untrusted queued PR',
    )
    if (pr.head.sha !== r.checkSha) return null
    requireThat(r.workflowSha === pr.head.sha && r.ref === pr.head.ref, 'Queued PR workflow differs')
    const commit = await api('GET', `/git/commits/${r.sourceSha}`)
    requireThat(
      commit.parents?.some((p) => p.sha === r.checkSha) && commit.parents.length === 2,
      'Queued source is not a PR merge',
    )
    baseSha = commit.parents.find((p) => p.sha !== r.checkSha)?.sha || ''
    requireThat(sha(baseSha), 'Missing queued PR base')
  } else
    requireThat(
      ['push', 'workflow_dispatch'].includes(parent.event) &&
        r.sourceSha === parent.head_sha &&
        r.workflowSha === r.sourceSha &&
        r.ref === parent.head_branch &&
        (parent.event !== 'push' || r.ref === policy.defaultBranch),
      'Invalid queued source',
    )
  Object.assign(receipt, { prNumber, baseSha })
  const queueRequest = {
    version: 3,
    backendId: binding.backendId,
    id,
    parentId: r.parentId,
    parentAttempt: r.parentAttempt,
    checkId: r.checkId,
    checkSha: r.checkSha,
    sourceSha: r.sourceSha,
    workflowSha: r.workflowSha,
    ref: r.ref,
    kind: r.kind,
    placement: r.placement,
    prNumber,
    baseSha,
  }
  if (parent.status !== 'completed') return null
  if (r.state === 'failed' || !r.attempt?.runId)
    return { check, receipt, parent, parentArtifact, queueRequest, rule, workerSuccess: false, shouldDeploy: false }
  const a = r.attempt
  requireThat(
    positive(a.runId) &&
      new RegExp(`^${parent.id}-${parent.run_attempt}-${id}-(?:fallback[1-9][0-9]*-)?(?:local|cloud)$`).test(
        a.requestId,
      ),
    'Invalid selected queue attempt',
  )
  const child = await api('GET', `/actions/runs/${a.runId}`)
  const title = `Sluicework build ${a.requestId} @ ${r.sourceSha}`
  requireThat(
    child.id === a.runId &&
      child.workflow_id === worker.id &&
      child.event === 'workflow_dispatch' &&
      child.run_attempt === 1 &&
      child.repository?.full_name === repository &&
      child.head_repository?.full_name === repository &&
      child.head_sha === r.workflowSha &&
      child.head_branch === r.ref &&
      child.display_title === title,
    'Worker differs from App-selected attempt',
  )
  if (fromChild && trigger.id !== child.id) return null
  if (child.status !== 'completed') return null
  const pool = a.target.platform === 'linux' && a.target.label === 'self-hosted'
  const os = pool || a.target.platform.startsWith('linux-') ? 'Linux' : 'macOS',
    arch = a.target.platform.endsWith('-arm64') ? 'ARM64' : 'X64'
  const pattern = policy.artifactPattern || 'sluicework-arm64-{os}-{arch}'
  let artifactName = pattern.replace('{kind}', r.kind).replace('{os}', os).replace('{arch}', arch)
  if (pool && child.conclusion === 'success') {
    const uploaded = await api('GET', `/actions/runs/${child.id}/artifacts?per_page=100`)
    const names = ['X64', 'ARM64'].map((value) => pattern.replace('{kind}', r.kind).replace('{os}', 'Linux').replace('{arch}', value))
    const artifacts = uploaded.artifacts?.filter((value) => names.includes(value.name)) || []
    requireThat(Number.isSafeInteger(uploaded.total_count) && uploaded.total_count <= 100 && artifacts.length === 1, 'Missing or ambiguous Linux pool artifact')
    const artifact = artifacts[0]
    requireThat(positive(artifact.id) && artifact.expired === false && artifact.workflow_run?.id === child.id && artifact.workflow_run?.head_sha === child.head_sha, 'Linux pool artifact differs from the selected worker')
    artifactName = artifact.name
  }
  receipt.child = {
    id: child.id,
    attempt: 1,
    title,
    artifactName,
  }
  const workerSuccess = parent.conclusion === 'success' && child.conclusion === 'success'
  return {
    check,
    receipt,
    parent,
    child,
    parentArtifact,
    queueRequest,
    rule,
    workerSuccess,
    shouldDeploy:
      workerSuccess && rule.deploy === true && parent.event !== 'pull_request' && r.ref === policy.defaultBranch,
  }
}
export function authenticateQueuedRequest(result, text) {
  requireThat(
    typeof text === 'string' && text.length < 16384 && isDeepStrictEqual(JSON.parse(text), result.queueRequest),
    'Queue request differs from the authenticated parent upload',
  )
}
