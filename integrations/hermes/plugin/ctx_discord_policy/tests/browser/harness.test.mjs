// The harness's own guard: no test compares DOM nodes through node:assert (whose failure message would walk the
// React fiber graph and the jsdom window until the process runs out of memory), and the harness assert fails
// such comparisons quickly with a short message.
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'

import { assert, cleanup, describeNode, h, render } from './harness.mjs'

const here = import.meta.dirname

afterEach(cleanup)

test('test files take assert from the harness, never from node:assert', () => {
  const offenders = readdirSync(here).filter((f) => f.endsWith('.mjs') && f !== 'harness.mjs')
    .filter((f) => /from\s+['"](node:)?assert(\/strict)?['"]/.test(readFileSync(join(here, f), 'utf8')))
  assert.deepEqual(offenders, [])
})

async function renderedButtons(n) {
  const { container } = await render(h('div', null, Array.from({ length: n }, (_, i) =>
    h('button', { key: i, onClick() {} }, 'Remove #c' + i))))
  return container.querySelector('button')
}

test('a failing comparison of a rendered node fails fast with one short line', async () => {
  const el = await renderedButtons(4)       // node:assert alone would format ~150 KB here
  const started = Date.now()
  let error = null
  try { assert.equal(el, null, 'the chip is gone') } catch (e) { error = e }
  assert.ok(error, 'the comparison failed')
  assert.ok(Date.now() - started < 1000, 'within a second')
  assert.ok(error.message.length < 300, `short message (${error.message.length} chars)`)
  assert.equal(error.message, 'the chip is gone: expected <button> "Remove #c0" to be null')
})

test('identity comparisons of nodes still pass and fail like strict equality', async () => {
  const el = await renderedButtons(2)
  assert.equal(el, el)
  assert.notEqual(el, null)
  assert.throws(() => assert.notEqual(el, el), /not to be <button>/)
  assert.throws(() => assert.deepEqual([el], [el]), /deepEqual with DOM nodes is not supported/)
  assert.equal(describeNode(null), 'null')
})

test('nodes inside arrays and objects are guarded too, at any depth', async () => {
  const el = await renderedButtons(4)
  for (const run of [() => assert.equal([el], null, 'm'), () => assert.strictEqual({ a: { b: [el] } }, null),
    () => { assert.notEqual(null, [[el]]); assert.equal(null, [[el]]) }, () => assert.deepEqual({ a: { b: el } }, {}),
    () => assert.deepStrictEqual([[{ c: el }]], [])]) {
    const started = Date.now()
    let error = null
    try { run() } catch (e) { error = e }
    assert.ok(error, 'the comparison failed')
    assert.ok(Date.now() - started < 1000 && error.message.length < 300, error.message.slice(0, 120))
  }
})

test('a comparison without a message fails as an assertion with both values', () => {
  let error = null
  try { assert.equal(1, 2) } catch (e) { error = e }
  assert.equal(error && error.code, 'ERR_ASSERTION')
  assert.match(error.message, /1 !== 2/)
})
