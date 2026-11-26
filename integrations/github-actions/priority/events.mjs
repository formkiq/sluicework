// Default-branch callback adapter for build-only repository integrations.
import { readFileSync } from 'node:fs'
import { resolveQueuedCompletion, authenticateQueuedRequest } from './queue.mjs'
const positive = (v) => Number.isSafeInteger(v) && v > 0
function requireThat(v, m) {
  if (!v) throw new Error(m)
}
export function loadPolicy() {
  return JSON.parse(readFileSync('.github/sluicework-events.json', 'utf8'))
}
export async function resolveCompletion(api, policy, repository, trigger, { retry = false } = {}) {
  requireThat(positive(trigger?.id) && positive(trigger.run_attempt), 'Invalid completion trigger')
  const worker = await api('GET', '/actions/workflows/sluicework-build.yml')
  let parentId = trigger.id,
    attempt = trigger.run_attempt,
    fromChild = false
  if (trigger.workflow_id === worker.id) {
    if (trigger.event !== 'workflow_dispatch') return null
    const match =
      /^Sluicework build ([1-9][0-9]*)-([1-9][0-9]*)-[a-f0-9-]{36}-(?:fallback[1-9][0-9]*-)?(?:local|cloud) @ ([a-f0-9]{40})$/.exec(
        trigger.display_title || '',
      )
    if (!match) return null
    parentId = Number(match[1])
    attempt = Number(match[2])
    fromChild = true
  }
  const parent = await api('GET', `/actions/runs/${parentId}`)
  if (parent.run_attempt !== attempt) return null
  const entry = Object.entries(policy.coordinators).find(([file]) => parent.path === `.github/workflows/${file}`)
  if (!entry) return null
  const [file, rule] = entry,
    workflow = await api('GET', `/actions/workflows/${file}`)
  if (parent.event === 'pull_request' && parent.head_repository?.full_name !== repository) return null
  requireThat(
    parent.workflow_id === workflow.id &&
      parent.repository?.full_name === repository &&
      parent.head_repository?.full_name === repository &&
      /^[a-f0-9]{40}$/.test(parent.head_sha || ''),
    'Untrusted queue coordinator',
  )
  if (!fromChild && trigger.workflow_id !== workflow.id) return null
  const checks = []
  for (let page = 1; page <= 10; page++) {
    const list = await api(
      'GET',
      `/commits/${parent.head_sha}/check-runs?check_name=${encodeURIComponent(rule.check)}&filter=all&per_page=100&page=${page}`,
    )
    checks.push(...list.check_runs)
    if (checks.length >= list.total_count)
      return resolveQueuedCompletion(api, policy, repository, trigger, {
        parent,
        rule,
        file,
        worker,
        fromChild,
        retry,
        checks,
      })
  }
  throw new Error('Too many required checks to authenticate')
}
export async function reportCompletion(api, result, success) {
  await api('PATCH', `/check-runs/${result.check.id}`, {
    status: 'completed',
    conclusion: success ? 'success' : 'failure',
    output: {
      title: 'Sluicework build',
      summary: success
        ? 'Selected worker and immutable artifact verified.'
        : 'Build or artifact verification failed. Inspect completion logs.',
    },
  })
}
export function authenticateHandoff(result, text) {
  authenticateQueuedRequest(result, text)
}
export async function deploymentCurrent() {
  return false
} // these integrations are build-only
