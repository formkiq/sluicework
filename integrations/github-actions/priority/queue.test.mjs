import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { queueOptions, queueReady, enqueue, resolveQueuedCompletion, authenticateQueuedRequest } from './queue.mjs'
const S = 'a'.repeat(40),
  BASE = 'b'.repeat(40),
  ID = '00000000-0000-4000-8000-000000000001'
const binding = { appId: 55, backendId: 'test', heartbeatSha: BASE }
const manifest = { version: 3, dispatcher: binding, defaultBranch: 'main' }
const policy = {
  defaultBranch: 'main',
  dispatcher: binding,
  coordinators: { 'ci.yml': { kind: 'site', check: 'build', deploy: true } },
  artifactPattern: 'sluicework-site-{os}-{arch}',
}
const routing = {
  version: 3,
  placement: 'prefer-local',
  platform: 'linux-x64',
  machineLabel: 'linux-runner-01',
  queueMinutes: 60,
  priority: 'low',
  fallbackQueueMinutes: 2,
  fallbackRunner: { platform: 'macos-arm64', machineLabel: 'mac-1' },
}
const health = {
  app: { id: 55 },
  head_sha: BASE,
  external_id: 'sluicework-dispatcher:test',
  output: { text: JSON.stringify({ version: 3, backendId: 'test', ready: true, at: new Date().toISOString() }) },
}
function readyAPI(method, path, body) {
  if (path.startsWith('/contents/')) return { content: Buffer.from(JSON.stringify(manifest)).toString('base64') }
  if (path.includes('/check-runs?')) return { total_count: 1, check_runs: [health] }
  if (method === 'POST' && path === '/check-runs') return { id: 8, ...body }
  throw new Error(`Unexpected API: ${method} ${path}`)
}
test('high and low profiles have fixed deadlines and never enter a hosted wait loop', () => {
  assert.equal(queueOptions({ GITHUB_SHA: S }, routing).priority, 'low')
  assert.equal(queueOptions({ GITHUB_SHA: S }, { ...routing, priority: 'high', queueMinutes: 15 }).priority, 'high')
  // Keep old policies readable while the application and workflows are upgraded.
  assert.equal(queueOptions({ GITHUB_SHA: S }, { ...routing, priority: 'high', queueMinutes: 5 }).priority, 'high')
  for (const bad of [
    { queueMinutes: 5 },
    { priority: 'high', queueMinutes: 10 },
    { priority: 'urgent' },
    { fallbackQueueMinutes: 60 },
    { unexpected: true },
    { fallbackRunner: { platform: 'macos-arm64', machineLabel: 'self-hosted' } },
  ])
    assert.throws(() => queueOptions({ GITHUB_SHA: S }, { ...routing, ...bad }))
  assert.throws(() => queueOptions({ GITHUB_SHA: S, LOCAL_QUEUE_MINUTES: '60' }, routing))
  assert.equal(queueOptions({}, { version: 2 }), null)
})
test('readiness requires the configured App-owned, fresh heartbeat and installed binding', async () => {
  await queueReady(readyAPI, manifest)
  for (const altered of [
    { app: { id: 99 } },
    { head_sha: S },
    { output: { text: JSON.stringify({ version: 3, backendId: 'test', ready: true, at: '2000-01-01' }) } },
  ]) {
    await assert.rejects(
      queueReady(
        (m, p, b) =>
          p.includes('/check-runs?') ? { total_count: 1, check_runs: [{ ...health, ...altered }] } : readyAPI(m, p, b),
        manifest,
      ),
    )
  }
  await assert.rejects(
    queueReady(
      (m, p, b) =>
        p.startsWith('/contents/')
          ? {
              content: Buffer.from(
                JSON.stringify({ ...manifest, dispatcher: { ...binding, backendId: 'other' } }),
              ).toString('base64'),
            }
          : readyAPI(m, p, b),
      manifest,
    ),
  )
})
test('enqueue uploads only identity and exits with a pending required check; no dispatch or polling', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'sw-queue-')),
    calls = []
  try {
    const env = {
      GITHUB_RUN_ID: '1',
      GITHUB_RUN_ATTEMPT: '1',
      GITHUB_REPOSITORY: 'o/r',
      GITHUB_WORKFLOW_REF: 'o/r/.github/workflows/ci.yml@refs/heads/main',
      GITHUB_SHA: S,
      RUNNER_TEMP: dir,
      GITHUB_OUTPUT: join(dir, 'outputs'),
    }
    const request = await enqueue({
      api: async (m, p, b) => {
        calls.push([m, p, b])
        return readyAPI(m, p, b)
      },
      manifest,
      policy,
      env,
      event: {},
      config: queueOptions(env, routing),
      ref: 'main',
      id: ID,
    })
    assert.equal(request.checkId, 8)
    assert.equal(request.id, ID)
    assert.deepEqual(JSON.parse(readFileSync(join(dir, 'sluicework-request.json'))), request)
    assert.match(readFileSync(env.GITHUB_OUTPUT, 'utf8'), /detached=true\nqueued=true/)
    assert.equal(calls.filter((c) => c[0] === 'POST').length, 1)
    assert.equal(calls.at(-1)[2].status, 'in_progress')
    assert.ok(!('priority' in request)) // class comes from protected server policy
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
function fixture() {
  const parent = {
    id: 1,
    run_attempt: 1,
    head_sha: S,
    head_branch: 'main',
    status: 'completed',
    conclusion: 'success',
    event: 'push',
    repository: { full_name: 'o/r' },
    head_repository: { full_name: 'o/r' },
  }
  const check = {
    id: 8,
    name: 'build',
    app: { slug: 'github-actions' },
    head_sha: S,
    external_id: `sluicework-v3:1:1:${ID}`,
    status: 'in_progress',
  }
  const request = {
    id: ID,
    repository: 'o/r',
    parentId: 1,
    parentAttempt: 1,
    parentWorkflow: 'ci.yml',
    kind: 'site',
    checkId: 8,
    checkSha: S,
    sourceSha: S,
    workflowSha: S,
    ref: 'main',
    artifactId: 44,
    placement: 'prefer-local',
    state: 'completed',
    attempt: { runId: 2, requestId: `1-1-${ID}-local`, target: { platform: 'linux-x64' } },
  }
  const selected = {
    app: { id: 55 },
    head_sha: S,
    external_id: `sluicework-queue-v3:1:1:${ID}`,
    output: { text: JSON.stringify({ version: 3, backendId: 'test', request }) },
  }
  const artifact = {
    id: 44,
    name: 'sluicework-request-1',
    expired: false,
    workflow_run: { id: 1, head_sha: S },
    digest: 'sha256:' + 'd'.repeat(64),
    size_in_bytes: 400,
  }
  const child = {
    id: 2,
    workflow_id: 9,
    event: 'workflow_dispatch',
    run_attempt: 1,
    repository: { full_name: 'o/r' },
    head_repository: { full_name: 'o/r' },
    head_sha: S,
    head_branch: 'main',
    display_title: `Sluicework build 1-1-${ID}-local @ ${S}`,
    status: 'completed',
    conclusion: 'success',
  }
  const childArtifacts = []
  const api = async (m, p) => {
    if (p.includes('check_name=Sluicework%20queue')) return { total_count: 1, check_runs: [selected] }
    if (p === '/actions/runs/1/artifacts?per_page=100') return { total_count: 1, artifacts: [artifact] }
    if (p === '/actions/runs/2') return child
    if (p === '/actions/runs/2/artifacts?per_page=100') return { total_count: childArtifacts.length, artifacts: childArtifacts }
    throw new Error('Unexpected API ' + p)
  }
  const resolve = () =>
    resolveQueuedCompletion(
      api,
      policy,
      'o/r',
      { id: 2, run_attempt: 1 },
      {
        parent,
        rule: policy.coordinators['ci.yml'],
        file: 'ci.yml',
        worker: { id: 9 },
        fromChild: true,
        retry: false,
        checks: [check],
      },
    )
  return { parent, check, request, selected, artifact, child, childArtifacts, resolve }
}
test('completion trusts the App-selected attempt plus the immutable parent request', async () => {
  const f = fixture(),
    r = await f.resolve()
  assert.equal(r.workerSuccess, true)
  assert.equal(r.shouldDeploy, true)
  assert.equal(r.receipt.child.artifactName, 'sluicework-site-Linux-X64')
  authenticateQueuedRequest(r, JSON.stringify(r.queueRequest))
  assert.throws(() => authenticateQueuedRequest(r, JSON.stringify({ ...r.queueRequest, sourceSha: BASE })))
})
test('an Actions-owned fake selection cannot authorize a result', async () => {
  const f = fixture()
  f.selected.app.id = 15368
  assert.equal(await f.resolve(), null)
})
test('wrong parent, source, workflow, artifact or worker attempt fail closed', async () => {
  for (const alter of [
    (f) => (f.artifact.workflow_run.head_sha = BASE),
    (f) => (f.artifact.id = 99),
    (f) => (f.child.head_sha = BASE),
    (f) => (f.child.run_attempt = 2),
    (f) => (f.child.display_title += ' forged'),
    (f) => (f.child.repository.full_name = 'evil/r'),
  ]) {
    const f = fixture()
    alter(f)
    await assert.rejects(f.resolve())
  }
})
test('unselected old attempts and duplicate completion do nothing', async () => {
  const f = fixture()
  f.child.id = 3
  await assert.rejects(f.resolve())
  const g = fixture()
  g.check.status = 'completed'
  assert.equal(await g.resolve(), null)
})
test('a selected failed worker reports failure without deployment or a retry', async () => {
  const f = fixture()
  f.child.conclusion = 'failure'
  const r = await f.resolve()
  assert.equal(r.workerSuccess, false)
  assert.equal(r.shouldDeploy, false)
})

test('a failed parent upload fails its required check without waiting for a selection', async () => {
  const f = fixture()
  f.parent.conclusion = 'failure'
  const result = await f.resolve()
  assert.equal(result.workerSuccess, false)
  assert.equal(result.shouldDeploy, false)
  assert.equal(result.parentArtifact, undefined)
})

test('existing v3 completion accepts a source-bound Spot attempt without a consumer upgrade', async () => {
  const f = fixture()
  f.request.policy = { spot: { profile: 'standard', queueMinutes: 5, startupMinutes: 10, runtimeMinutes: 90 } }
  f.request.spot = { allocation: { id: ID, state: 'ready', label: 'sluicework-spot-test' } }
  f.request.attempt.tier = 2
  f.request.attempt.target = { host: 'local', platform: 'linux-arm64', label: 'sluicework-spot-test' }
  f.selected.output.text = JSON.stringify({ version: 3, backendId: 'test', request: f.request })
  const r = await f.resolve()
  assert.equal(r.workerSuccess, true)
  assert.equal(r.receipt.child.artifactName, 'sluicework-site-Linux-ARM64')
})

test('generic primary Linux pool needs neither a custom label nor architecture but preserves explicit fallback', () => {
  const pool = { ...routing, platform: 'linux', machineLabel: 'self-hosted' }
  assert.equal(queueOptions({ GITHUB_SHA: S }, pool).priority, routing.priority)
  for (const changed of [{ machineLabel: 'linux-runner-01' }, { platform: 'macos-arm64' }, { fallbackRunner: { platform: 'linux', machineLabel: 'self-hosted' } }])
    assert.throws(() => queueOptions({ GITHUB_SHA: S }, { ...pool, ...changed }))
})

test('Linux pool completion derives the actual artifact architecture from its authenticated worker', async () => {
  for (const arch of ['ARM64', 'X64']) {
    const f = fixture()
    f.request.attempt.target = { host: 'local', platform: 'linux', label: 'self-hosted' }
    f.selected.output.text = JSON.stringify({ version: 3, backendId: 'test', request: f.request })
    f.childArtifacts.push({ id: 88, name: `sluicework-site-Linux-${arch}`, expired: false, workflow_run: { id: f.child.id, head_sha: f.child.head_sha } })
    assert.equal((await f.resolve()).receipt.child.artifactName, `sluicework-site-Linux-${arch}`)
    f.childArtifacts.push({ ...f.childArtifacts[0], id: 89, name: `sluicework-site-Linux-${arch === 'ARM64' ? 'X64' : 'ARM64'}` })
    await assert.rejects(f.resolve(), /ambiguous/)
    f.childArtifacts.pop(); f.childArtifacts[0].workflow_run.id = 99
    await assert.rejects(f.resolve(), /differs/)
    f.childArtifacts.length = 0
    await assert.rejects(f.resolve(), /Missing/)
  }
})
