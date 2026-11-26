import { readFileSync } from 'node:fs'
import { githubAPI } from './github-api.mjs'
import { queueOptions, enqueue } from './queue.mjs'
try {
  const env = process.env,
    event = JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8'))
  const manifest = JSON.parse(readFileSync('.github/sluicework.json', 'utf8')),
    policy = JSON.parse(readFileSync('.github/sluicework-events.json', 'utf8'))
  const pr = event.pull_request
  const trusted =
    env.GITHUB_EVENT_NAME === 'push'
      ? env.GITHUB_REF === `refs/heads/${policy.defaultBranch}`
      : env.GITHUB_EVENT_NAME === 'workflow_dispatch'
        ? event.sender?.type === 'User'
        : env.GITHUB_EVENT_NAME === 'pull_request' &&
          pr?.head?.repo?.full_name === env.GITHUB_REPOSITORY &&
          pr?.base?.repo?.full_name === env.GITHUB_REPOSITORY &&
          pr.base.ref === policy.defaultBranch &&
          pr.user?.type === 'User' &&
          event.sender?.type === 'User' &&
          ['OWNER', 'MEMBER', 'COLLABORATOR'].includes(pr.author_association)
  if (!trusted) throw new Error('Untrusted queue event')
  const config = queueOptions(env, manifest.defaults)
  if (!config) throw new Error('Publish a version 3 queue policy before enabling the dispatcher')
  const api = githubAPI({ token: env.GH_TOKEN, repository: env.GITHUB_REPOSITORY })
  const ref = pr?.head?.ref || env.GITHUB_REF_NAME
  const current = await api('GET', `/git/ref/heads/${ref.split('/').map(encodeURIComponent).join('/')}`)
  config.workflowSha = pr?.head?.sha || env.GITHUB_SHA
  if (current.object?.sha !== config.workflowSha) throw new Error('Workflow branch moved; use its current revision')
  await enqueue({ api, manifest, policy, env, event, config, ref })
} catch (error) {
  console.error(error.message)
  process.exitCode = 1
}
