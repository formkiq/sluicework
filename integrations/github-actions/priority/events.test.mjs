import test from 'node:test'
import assert from 'node:assert/strict'
import { resolveCompletion } from './events.mjs'
const S = 'a'.repeat(40),
  repository = 'o/r',
  policy = {
    defaultBranch: 'main',
    dispatcher: { appId: 55, backendId: 'test' },
    coordinators: { 'ci.yml': { kind: 'site', check: 'build', deploy: false } },
  }
function apiFor(parent) {
  return async (method, path) => {
    assert.equal(method, 'GET')
    if (path === '/actions/workflows/sluicework-build.yml') return { id: 9 }
    if (path === '/actions/runs/1') return parent
    if (path === '/actions/workflows/ci.yml') return { id: 4 }
    if (path.startsWith('/commits/')) return { total_count: 0, check_runs: [] }
    throw new Error('Unexpected endpoint ' + path)
  }
}
const parent = {
  id: 1,
  run_attempt: 1,
  workflow_id: 4,
  path: '.github/workflows/ci.yml',
  head_sha: S,
  event: 'push',
  head_branch: 'main',
  status: 'completed',
  conclusion: 'success',
  repository: { full_name: repository },
  head_repository: { full_name: repository },
}
test('hosted bootstrap checks and unrelated child runs cannot claim a required check', async () => {
  assert.equal(await resolveCompletion(apiFor(parent), policy, repository, parent), null)
  assert.equal(
    await resolveCompletion(apiFor(parent), policy, repository, {
      id: 2,
      run_attempt: 1,
      workflow_id: 9,
      event: 'workflow_call',
    }),
    null,
  )
})
test('a superseded parent attempt is ignored before result publication', async () => {
  assert.equal(await resolveCompletion(apiFor({ ...parent, run_attempt: 2 }), policy, repository, parent), null)
})
test('fork PR completion cannot enter the privileged callback', async () => {
  const fork = { ...parent, event: 'pull_request', head_repository: { full_name: 'outside/r' } }
  assert.equal(await resolveCompletion(apiFor(fork), policy, repository, fork), null)
})
test('wrong workflow identity and repository refuse completion', async () => {
  for (const bad of [
    { ...parent, workflow_id: 88 },
    { ...parent, repository: { full_name: 'evil/r' } },
  ])
    await assert.rejects(resolveCompletion(apiFor(bad), policy, repository, bad))
})
