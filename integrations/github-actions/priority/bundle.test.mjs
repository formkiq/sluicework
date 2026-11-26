import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, symlinkSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { bundle } from './bundle.mjs'
test('build bundle verifies source and exact files and rejects altered content, extras and symlinks', () => {
  const previous = process.cwd(),
    dir = mkdtempSync(join(tmpdir(), 'sw-bundle-'))
  try {
    process.chdir(dir)
    mkdirSync('dist')
    writeFileSync('dist/index.html', 'ok')
    const config = { roots: ['dist'], required: ['dist/index.html'] },
      identity = { sourceSha: 'a'.repeat(40), runId: '1', repository: 'o/r' }
    bundle('create', 'result', config, identity)
    bundle('verify', 'result', config, identity)
    assert.throws(() => bundle('verify', 'result', config, { ...identity, sourceSha: 'b'.repeat(40) }))
    writeFileSync('result/dist/index.html', 'changed')
    assert.throws(() => bundle('verify', 'result', config, identity))
    writeFileSync('result/dist/index.html', 'ok')
    writeFileSync('result/extra', 'bad')
    assert.throws(() => bundle('verify', 'result', config, identity))
    rmSync('result/extra')
    symlinkSync('/etc/passwd', 'result/link')
    assert.throws(() => bundle('verify', 'result', config, identity))
  } finally {
    process.chdir(previous)
    rmSync(dir, { recursive: true, force: true })
  }
})
