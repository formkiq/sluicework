// A hosted coordinator dispatches one build at a time. No Sluicework endpoint,
// App key, or personal token is needed by this workflow.
import { randomUUID } from 'node:crypto'
import { appendFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'

export function options(env) {
  const policy = env.PLACEMENT || 'local-only'
  if (!['prefer-local', 'local-only', 'cloud-only'].includes(policy)) throw new Error('Invalid placement policy')
  const target = env.LOCAL_PLATFORM || 'macos-arm64'
  if (!['macos-arm64', 'linux-arm64'].includes(target)) throw new Error('Choose macos-arm64 or linux-arm64')
  const label = env.MACHINE_LABEL || ''
  if (policy !== 'cloud-only' && !/^sluicework-[a-zA-Z0-9_.-]{1,100}$/.test(label)) throw new Error('Choose an explicit Sluicework machine label')
  const minutes = Number(env.LOCAL_QUEUE_MINUTES || '2')
  if (!Number.isInteger(minutes) || minutes < 1 || minutes > 15) throw new Error('Local queue timeout must be 1–15 minutes')
  if (!/^[a-f0-9]{40}$/.test(env.GITHUB_SHA || '')) throw new Error('An immutable source SHA is required')
  return { policy, target, label, queueMs: minutes * 60_000, sha: env.GITHUB_SHA }
}

// Treat any evidence of assignment/execution conservatively. A canceled queued
// job can have timestamps, so timestamps alone are not proof of execution.
export function mayHaveStarted(jobs) {
  return jobs.some(j => j.runner_id > 0 || j.runner_name || (j.steps || []).some(s => s.started_at || s.status === 'in_progress') || j.status === 'in_progress' || (j.status === 'completed' && j.conclusion !== 'cancelled' && j.conclusion !== 'skipped'))
}

export async function route({ api, config, ref, now = Date.now, sleep = ms => new Promise(r => setTimeout(r, ms)), id = randomUUID(), log = console.log, deadlineMs = 65 * 60_000 }) {
  const deadline = now() + deadlineMs
  let active
  const check = () => { if (now() >= deadline) throw new Error('Build coordinator deadline exceeded') }
  async function dispatch(host) {
    const request = `${id}-${host}`
    // Dispatch is intentionally never retried: an uncertain POST must not
    // create duplicate builds. Correlation is unique across concurrent runs.
    await api('POST', '/actions/workflows/sluicework-build.yml/dispatches', {
      ref, inputs: { request_id: request, source_sha: config.sha, host, local_platform: config.target, machine_label: config.label },
    })
    const discovery = now() + 120_000
    while (now() < discovery) {
      check()
      const runs = await api('GET', '/actions/workflows/sluicework-build.yml/runs?event=workflow_dispatch&per_page=100')
      const matches = runs.workflow_runs.filter(r => r.display_title === `Sluicework build ${request}`)
      if (matches.length > 1) throw new Error('Ambiguous build dispatch; refusing fallback')
      if (matches.length === 1) {
        const run = matches[0]
        active = run.id
        if (run.head_sha !== config.sha) throw new Error('Workflow ref moved; refusing to build a different revision')
        log(`Build: ${run.html_url}`)
        return run
      }
      await sleep(5000)
    }
    throw new Error('Dispatch could not be confirmed; refusing fallback')
  }
  async function jobs(run) {
    const result = await api('GET', `/actions/runs/${run.id}/jobs?per_page=100`)
    if (result.total_count > 100) throw new Error('Unexpected child workflow size')
    return result.jobs
  }
  async function finish(run) {
    while (true) {
      check()
      const current = await api('GET', `/actions/runs/${run.id}`)
      if (current.status === 'completed') {
        active = undefined
        if (current.conclusion !== 'success') throw new Error(`Build ${run.id} finished with ${current.conclusion}; build failures are never rerouted`)
        return current
      }
      await sleep(5000)
    }
  }
  try {
    if (config.policy === 'cloud-only') return await finish(await dispatch('cloud'))
    const local = await dispatch('local')
    const until = now() + config.queueMs
    while (now() < until) {
      check()
      const current = await api('GET', `/actions/runs/${local.id}`)
      if (current.status === 'completed' || mayHaveStarted(await jobs(local))) return await finish(local)
      await sleep(5000)
    }
    // Recheck immediately before cancellation. Once a job starts, always wait
    // for that attempt, even if its execution exceeds the queue timeout.
    if (mayHaveStarted(await jobs(local))) return await finish(local)
    await api('POST', `/actions/runs/${local.id}/cancel`)
    const cancelDeadline = now() + 120_000
    while (true) {
      check()
      const current = await api('GET', `/actions/runs/${local.id}`)
      if (current.status === 'completed') {
        active = undefined
        const finalJobs = await jobs(local)
        // Cancellation races assignment. Never launch cloud work if there is
        // any evidence the local job was assigned or executed during the race.
        if (current.conclusion !== 'cancelled' || finalJobs.length === 0 || mayHaveStarted(finalJobs)) throw new Error('Local cancellation raced assignment; refusing duplicate execution')
        break
      }
      if (now() >= cancelDeadline) throw new Error('Local cancellation not confirmed; refusing fallback')
      await sleep(5000)
    }
    if (config.policy === 'local-only') throw new Error('No workstation accepted the build before the queue timeout; cloud fallback is disabled')
    log('No local build started; using GitHub-hosted ARM64 capacity')
    return await finish(await dispatch('cloud'))
  } finally {
    // On API errors/deadline, cancel the known child. Failure to confirm a
    // dispatch never permits a second build.
    if (active) { try { await api('POST', `/actions/runs/${active}/cancel`) } catch { log(`Child ${active} may need manual cancellation`) } }
  }
}

export function githubAPI({ token, repository, fetcher = fetch }) {
  if (!token || !/^[\w.-]+\/[\w.-]+$/.test(repository || '')) throw new Error('GitHub token and repository are required')
  return async (method, path, body) => {
    const response = await fetcher(`https://api.github.com/repos/${repository}${path}`, {
      method, signal: AbortSignal.timeout(30_000), redirect: 'error',
      headers: { authorization: `Bearer ${token}`, accept: 'application/vnd.github+json', 'content-type': 'application/json', 'x-github-api-version': '2022-11-28' },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!response.ok) throw new Error(`GitHub ${method} ${path.split('?')[0]} returned ${response.status}`)
    return response.status === 204 ? undefined : response.json()
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const config = options(process.env)
    const result = await route({ config, ref: process.env.GITHUB_REF_NAME, id: `${process.env.GITHUB_RUN_ID}-${process.env.GITHUB_RUN_ATTEMPT}-${randomUUID()}`, api: githubAPI({ token: process.env.GH_TOKEN, repository: process.env.GITHUB_REPOSITORY }) })
    if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, `ARM64 build passed: ${result.html_url}\n\nArtifacts are attached to that build. No deployment was performed.\n`)
  } catch (error) { console.error(error.message); process.exitCode = 1 }
}
