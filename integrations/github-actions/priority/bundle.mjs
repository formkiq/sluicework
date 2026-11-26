// Small portable artifact contract for repositories without Rust Lambda bundles.
import { createHash } from 'node:crypto'
import { readFileSync, readdirSync, lstatSync, mkdirSync, copyFileSync, writeFileSync } from 'node:fs'
import { resolve, relative, join } from 'node:path'
import { pathToFileURL } from 'node:url'
const hash = (file) => createHash('sha256').update(readFileSync(file)).digest('hex')
function requireThat(v, m) {
  if (!v) throw new Error(m)
}
function files(root) {
  requireThat(
    lstatSync(root).isDirectory() && !lstatSync(root).isSymbolicLink(),
    'Bundle roots must be real directories',
  )
  const found = []
  const visit = (dir) => {
    for (const entry of readdirSync(dir)) {
      const file = join(dir, entry),
        st = lstatSync(file)
      requireThat(!st.isSymbolicLink(), 'Bundle symlinks are forbidden')
      if (st.isDirectory()) visit(file)
      else {
        requireThat(st.isFile(), 'Only regular bundle files are allowed')
        found.push(relative(root, file).split('\\').join('/'))
      }
    }
  }
  visit(root)
  return found.sort()
}
export function bundle(mode, root, config, identity) {
  requireThat(
    /^[a-f0-9]{40}$/.test(identity.sourceSha || '') &&
      /^[1-9][0-9]*$/.test(identity.runId || '') &&
      /^[\w.-]+\/[\w.-]+$/.test(identity.repository || ''),
    'Invalid build identity',
  )
  const allowed = (p) =>
    typeof p === 'string' &&
    !p.startsWith('/') &&
    !p.split('/').some((s) => s === '..' || s === '.' || !s) &&
    config.roots.some((r) => p.startsWith(r + '/'))
  requireThat(
    Array.isArray(config.roots) && config.roots.every((r) => /^[\w.-]+$/.test(r)) && config.roots.length > 0,
    'Invalid bundle roots',
  )
  if (mode === 'create') {
    mkdirSync(root, { recursive: true })
    requireThat(files(root).length === 0, 'Bundle output must start empty')
    const selected = config.roots.flatMap((r) => files(r).map((p) => r + '/' + p)).sort()
    requireThat(selected.length > 0 && selected.length < 20000, 'Unexpected build output count')
    const hashes = {}
    for (const path of selected) {
      requireThat(allowed(path), 'Invalid output path')
      const dest = join(root, path)
      mkdirSync(resolve(dest, '..'), { recursive: true })
      copyFileSync(path, dest)
      hashes[path] = hash(dest)
    }
    writeFileSync(join(root, 'manifest.json'), JSON.stringify({ version: 1, ...identity, files: hashes }))
  } else {
    requireThat(mode === 'verify', 'Unknown bundle operation')
    const manifest = JSON.parse(readFileSync(join(root, 'manifest.json'), 'utf8'))
    requireThat(
      manifest.version === 1 &&
        manifest.sourceSha === identity.sourceSha &&
        manifest.runId === identity.runId &&
        manifest.repository === identity.repository,
      'Artifact belongs to another build',
    )
    const listed = Object.keys(manifest.files || {}).sort(),
      actual = files(root).filter((p) => p !== 'manifest.json')
    requireThat(
      listed.length > 0 && listed.length < 20000 && JSON.stringify(listed) === JSON.stringify(actual),
      'Bundle contains missing or unlisted files',
    )
    for (const path of listed)
      requireThat(allowed(path) && manifest.files[path] === hash(join(root, path)), 'Bundle path or hash differs')
    for (const required of config.required || [])
      requireThat(listed.includes(required), 'Required build output is missing')
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    bundle(
      process.argv[2],
      process.argv[3] || '.sluicework-bundle',
      JSON.parse(readFileSync('.github/sluicework-bundle.json', 'utf8')),
      {
        repository: process.env.GITHUB_REPOSITORY,
        sourceSha: process.env.SOURCE_SHA,
        runId: process.env.BUILD_RUN_ID || process.env.GITHUB_RUN_ID,
      },
    )
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}
