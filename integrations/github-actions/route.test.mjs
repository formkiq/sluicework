import test from 'node:test'
import assert from 'node:assert/strict'
import { options, mayHaveStarted, route } from './route.mjs'

const sha = 'a'.repeat(40)
const config = { policy: 'prefer-local', target: 'macos-arm64', label: 'sluicework-pilot', queueMs: 10, sha }

function fixture({ started = false, race = false, failure = false, unconfirmed = false } = {}) {
  let clock = 0
  let cancelled = false
  const calls = []
  const builds = []
  const api = async (method, path, body) => {
    calls.push({ method, path, body })
    if (path.endsWith('/dispatches')) { builds.push(body.inputs.host); return }
    if (path.includes('/workflows/') && method === 'GET') return { workflow_runs: builds.map((host, i) => ({ id: i + 1, display_title: `Sluicework build test-${host}`, head_sha: sha, html_url: `https://example.test/${i + 1}` })) }
    if (path.endsWith('/cancel')) { cancelled = true; return }
    if (path.includes('/jobs')) return { total_count: 1, jobs: [{ runner_id: started || (cancelled && race) ? 123 : 0, status: started ? 'in_progress' : cancelled ? 'completed' : 'queued', conclusion: cancelled ? 'cancelled' : null, steps: [] }] }
    if (path.endsWith('/2')) return { id: 2, status: 'completed', conclusion: 'success' }
    if (path.endsWith('/1')) {
      if (cancelled) return { status: unconfirmed ? 'queued' : 'completed', conclusion: 'cancelled' }
      if (started && clock > 15) return { status: 'completed', conclusion: failure ? 'failure' : 'success' }
      return { status: 'queued' }
    }
    throw new Error(`Unexpected request ${path}`)
  }
  return { api, builds, calls, now: () => clock, sleep: async () => { clock += 10 }, log: () => {}, id: 'test', ref: 'main' }
}

test('cloud fallback waits for a confirmed unstarted cancellation', async () => {
  const f = fixture()
  const result = await route({ ...f, config })
  assert.equal(result.id, 2)
  assert.deepEqual(f.builds, ['local', 'cloud'])
  const cancel = f.calls.findIndex(c => c.path === '/actions/runs/1/cancel')
  const cloud = f.calls.findIndex(c => c.body?.inputs?.host === 'cloud')
  assert.ok(cancel >= 0 && cloud > cancel)
  assert.ok(f.calls.slice(cancel + 1, cloud).some(c => c.path.includes('/jobs')))
})
test('local-only never dispatches a cloud build', async () => {
  const f = fixture()
  await assert.rejects(route({ ...f, config: { ...config, policy: 'local-only' } }), /cloud fallback is disabled/)
  assert.deepEqual(f.builds, ['local'])
})
test('an assigned build can exceed the queue deadline and is never duplicated', async () => {
  const f = fixture({ started: true })
  await route({ ...f, config })
  assert.deepEqual(f.builds, ['local'])
  assert.equal(f.calls.some(c => c.path.endsWith('/cancel')), false)
})
test('build failures are reported without retrying on another host', async () => {
  const f = fixture({ started: true, failure: true })
  await assert.rejects(route({ ...f, config }), /failure; build failures are never rerouted/)
  assert.deepEqual(f.builds, ['local'])
})
test('a cancellation/assignment race refuses fallback', async () => {
  const f = fixture({ race: true })
  await assert.rejects(route({ ...f, config }), /raced assignment/)
  assert.deepEqual(f.builds, ['local'])
})
test('unconfirmed cancellation refuses fallback', async () => {
  const f = fixture({ unconfirmed: true })
  await assert.rejects(route({ ...f, config, deadlineMs: 100 }), /deadline exceeded/)
  assert.deepEqual(f.builds, ['local'])
})
test('cloud-only bypasses local placement', async () => {
  const f = fixture()
  // Cloud is the first dispatch in this case.
  const original = f.api
  f.api = (method, path, body) => path === '/actions/runs/1' ? { status: 'completed', conclusion: 'success' } : original(method, path, body)
  await route({ ...f, config: { ...config, policy: 'cloud-only' } })
  assert.deepEqual(f.builds, ['cloud'])
})
test('templates require explicit platforms, labels and immutable revisions', () => {
  const env = { GITHUB_SHA: sha, MACHINE_LABEL: 'sluicework-mac' }
  assert.equal(options(env).policy, 'local-only')
  for (const change of [{ LOCAL_PLATFORM: 'random' }, { MACHINE_LABEL: 'self-hosted' }, { PLACEMENT: 'automatic' }, { LOCAL_QUEUE_MINUTES: '0' }, { GITHUB_SHA: 'main' }]) assert.throws(() => options({ ...env, ...change }))
  assert.equal(mayHaveStarted([{ status: 'completed', conclusion: 'cancelled', started_at: 'timestamp', steps: [] }]), false)
  assert.equal(mayHaveStarted([{ steps: [{ started_at: 'timestamp' }] }]), true)
})
