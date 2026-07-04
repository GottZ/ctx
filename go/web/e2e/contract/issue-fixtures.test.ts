// U03 fixture-namespace pins (vitest, e2e/contract include). Proves the three
// mechanical properties the browser mocks rely on WITHOUT booting Playwright:
//  (a) the served body IS the on-disk freeze JSON (import, not hand-copy);
//  (b) an un-mocked path INSIDE the ISSUES_BASE/TYPES_BASE namespace hard-fails
//      LOUD (599 {success:false}) — the closed N3 benign catch-all cannot re-open;
//  (c) the namespace matcher is derived from ISSUES_BASE/TYPES_BASE (a path off
//      the base is not swallowed), and neither module re-types the prefix literal.
// meta.spec.ts asserts the same at the browser layer through the real fixtures.

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { issueFreeze, workflowMock } from '../issue-fixtures'
import { ISSUES_BASE } from '../../src/lib/api/issues'
import { TYPES_BASE } from '../../src/lib/api/types-registry'
import issueListJson from '../../src/lib/api/__fixtures__/issue-list.json' with { type: 'json' }
import boardJson from '../../src/lib/api/__fixtures__/board.json' with { type: 'json' }
import typeListJson from '../../src/lib/api/__fixtures__/type-list.json' with { type: 'json' }

describe('U03 fixture namespace (design 04 §7-U03)', () => {
  // (a) no hand-copy: the fixture module serves the SAME object the freeze file
  // parses to (vite caches JSON imports → reference identity).
  it('serves the freeze JSON itself, not a hand-copy', () => {
    expect(issueFreeze.issueList).toBe(issueListJson)
    expect(workflowMock('GET', `${ISSUES_BASE}/p1/issues`)?.json).toBe(issueListJson)
    expect(workflowMock('GET', `${ISSUES_BASE}/p1/board`)?.json).toBe(boardJson)
    expect(workflowMock('GET', TYPES_BASE)?.json).toBe(typeListJson)
  })

  // (b) namespace hard-fail: an un-mocked path INSIDE the namespace is a LOUD 599,
  // never a benign {success:true} (the N3 trap stays closed).
  it('hard-fails an un-mocked path INSIDE the namespace (loud 599, never {success:true})', () => {
    const miss = workflowMock('GET', `${ISSUES_BASE}/p1/bogus`)
    expect(miss).not.toBeNull()
    expect(miss?.status).toBe(599)
    const body = miss?.json as Record<string, unknown>
    expect(body.success).toBe(false)
    expect(body.__unmocked).toBe(true)
    expect(String(body.error)).toContain('unmocked workflow endpoint')

    // A wrong METHOD on a real path is also a namespace miss, not a silent pass.
    const wrongMethod = workflowMock('DELETE', `${ISSUES_BASE}/p1/issues/b1`)
    expect(wrongMethod?.status).toBe(599)
    expect((wrongMethod?.json as Record<string, unknown>).success).toBe(false)
  })

  // (c) coupling: the boundary is ISSUES_BASE/TYPES_BASE-derived. A path off the
  // base returns null (falls through to the generic handler), so the namespace
  // can never run past the endpoint form.
  it('returns null for paths OUTSIDE the namespace (matcher follows the base)', () => {
    expect(workflowMock('GET', '/api/status')).toBeNull()
    expect(workflowMock('GET', '/api/search')).toBeNull()
    // A prefix that merely resembles the base is NOT captured.
    expect(workflowMock('GET', '/api/projectile/x')).toBeNull()
  })

  // (c) the prefix literal lives in exactly ONE place per module (the exported
  // const) — the fixture matcher imports it, it does not re-type '/api/project'.
  it('the fixture matcher imports the base, never re-types the prefix', () => {
    const src = readFileSync(new URL('../issue-fixtures.ts', import.meta.url), 'utf8')
    // The only allowed occurrences are the two import lines; the dispatch logic
    // uses ISSUES_BASE / TYPES_BASE. So no bare '/api/project' string literal.
    expect(src.includes("'/api/project'")).toBe(false)
    expect(src.includes("'/api/types'")).toBe(false)
  })

  it('routes each mocked workflow endpoint to its freeze body', () => {
    expect(workflowMock('GET', ISSUES_BASE)?.status).toBe(200) // project list
    expect(workflowMock('GET', `${ISSUES_BASE}/p1`)?.status).toBe(200) // project detail
    expect(workflowMock('GET', `${ISSUES_BASE}/p1/issues/b1`)?.status).toBe(200)
    expect(workflowMock('POST', `${ISSUES_BASE}/p1/issues`)?.status).toBe(200)
    expect(workflowMock('PATCH', `${ISSUES_BASE}/p1/issues/b1`)?.status).toBe(200)
    expect(workflowMock('GET', `${ISSUES_BASE}/p1/issues/b1/comments`)?.status).toBe(200)
    expect(workflowMock('POST', `${ISSUES_BASE}/p1/issues/b1/comments`)?.status).toBe(200)
    expect(workflowMock('GET', `${ISSUES_BASE}/p1/sync`)?.status).toBe(200)
    expect(workflowMock('POST', `${ISSUES_BASE}/p1/sync`)?.status).toBe(200)
    expect(workflowMock('GET', `${TYPES_BASE}/issue`)?.status).toBe(200)
  })
})
