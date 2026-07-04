// Shared workflow-UI fixture data + namespace dispatcher for the e2e mocks
// (design/04 §7-U03). This module has NO @playwright/test import on purpose, so
// it is unit-testable in vitest (e2e/contract/issue-fixtures.test.ts) AND
// imported by e2e/fixtures.ts into the page.route layer.
//
// The response bodies ARE the contract-freeze JSONs in
// src/lib/api/__fixtures__/*.json — imported, NEVER hand-copied. The Go golden
// TestContractFreezeGolden pins those same files to the live W6/W7/W11/W4/types
// handler structs, so a Go-side wire drift turns the golden red before it can
// silently pass these browser mocks (closes e2e-playwright.md Finding 8).
//
// EVERY matched path is derived from ISSUES_BASE / TYPES_BASE (the single path
// prefixes) — the namespace matcher can never drift from the final endpoint form
// (U03 coupling gate). Un-mocked paths INSIDE the namespace get this module's OWN
// loud hard-fail (599), never the closed benign {success:true} catch-all (N3).

import { ISSUES_BASE } from '../src/lib/api/issues'
import { TYPES_BASE } from '../src/lib/api/types-registry'
import board from '../src/lib/api/__fixtures__/board.json' with { type: 'json' }
import commentCreate from '../src/lib/api/__fixtures__/comment-create.json' with { type: 'json' }
import issueComments from '../src/lib/api/__fixtures__/issue-comments.json' with { type: 'json' }
import issueDetail from '../src/lib/api/__fixtures__/issue-detail.json' with { type: 'json' }
import issueList from '../src/lib/api/__fixtures__/issue-list.json' with { type: 'json' }
import issueMutate from '../src/lib/api/__fixtures__/issue-mutate.json' with { type: 'json' }
import projectList from '../src/lib/api/__fixtures__/project-list.json' with { type: 'json' }
import syncStatus from '../src/lib/api/__fixtures__/sync-status.json' with { type: 'json' }
import typeList from '../src/lib/api/__fixtures__/type-list.json' with { type: 'json' }

/** The imported freeze JSONs, exposed so the vitest probe can prove the served
 * body is the SAME object as the on-disk fixture (import, not hand-copy). */
export const issueFreeze = {
  board,
  commentCreate,
  issueComments,
  issueDetail,
  issueList,
  issueMutate,
  projectList,
  syncStatus,
  typeList,
} as const

/** One mock result: an HTTP status + the JSON body page.route fulfills with. */
export interface MockResult {
  status: number
  json: unknown
}

/**
 * A distinct, LOUD hard-fail for an un-mocked path INSIDE the workflow namespace.
 * Kept separate from the global 599 catch-all so a namespace miss is diagnosable
 * as such — and it must NEVER degrade to a benign {success:true} (the closed N3
 * trap, e2e-playwright.md N3). 599 is non-2xx ⇒ apiFetch throws (api.ts) loudly.
 */
function namespaceHardFail(method: string, path: string): MockResult {
  return {
    status: 599,
    json: { __unmocked: true, success: false, error: `unmocked workflow endpoint: ${method} ${path}` },
  }
}

/**
 * Dispatch a workflow-namespace request to its freeze-JSON body.
 *  - returns `null` when `path` is OUTSIDE the namespace (the caller falls
 *    through to the generic /api/** handler);
 *  - returns a 200 + freeze-JSON body for a mocked endpoint;
 *  - returns the namespace hard-fail for an un-mocked path INSIDE the namespace.
 */
export function workflowMock(method: string, path: string): MockResult | null {
  const inTypes = path === TYPES_BASE || path.startsWith(`${TYPES_BASE}/`)
  const inIssues = path === ISSUES_BASE || path.startsWith(`${ISSUES_BASE}/`)
  if (!inTypes && !inIssues) return null

  // --- /api/types[/{name}] ---
  if (inTypes) {
    if (path === TYPES_BASE && method === 'GET') return { status: 200, json: typeList }
    if (path.startsWith(`${TYPES_BASE}/`) && method === 'GET') {
      const first = (typeList as { types: unknown[] }).types[0]
      return { status: 200, json: { success: true, type: first } }
    }
    return namespaceHardFail(method, path)
  }

  // --- /api/project family --- strip the base, split the remainder.
  const rest = path === ISSUES_BASE ? '' : path.slice(`${ISSUES_BASE}/`.length)
  const seg = rest === '' ? [] : rest.split('/')

  // GET /api/project  (register list / picker source)
  if (seg.length === 0) {
    if (method === 'GET') return { status: 200, json: projectList }
    return namespaceHardFail(method, path)
  }
  // GET /api/project/{id}  (single project)
  if (seg.length === 1) {
    if (method === 'GET') {
      const first = (projectList as { projects: unknown[] }).projects[0]
      return { status: 200, json: { success: true, project: first } }
    }
    return namespaceHardFail(method, path)
  }

  const sub = seg[1]
  // /api/project/{id}/board
  if (sub === 'board' && seg.length === 2 && method === 'GET') return { status: 200, json: board }
  // /api/project/{id}/sync
  if (sub === 'sync' && seg.length === 2) {
    if (method === 'GET') return { status: 200, json: syncStatus }
    if (method === 'POST') return { status: 200, json: { success: true, run: (syncStatus as { run: unknown }).run } }
    return namespaceHardFail(method, path)
  }
  // /api/project/{id}/issues*
  if (sub === 'issues') {
    if (seg.length === 2) {
      if (method === 'GET') return { status: 200, json: issueList }
      if (method === 'POST') return { status: 200, json: issueMutate }
      return namespaceHardFail(method, path)
    }
    if (seg.length === 3) {
      if (method === 'GET') return { status: 200, json: issueDetail }
      if (method === 'PATCH') return { status: 200, json: issueMutate }
      return namespaceHardFail(method, path)
    }
    if (seg.length === 4 && seg[3] === 'comments') {
      if (method === 'GET') return { status: 200, json: issueComments }
      if (method === 'POST') return { status: 200, json: commentCreate }
      return namespaceHardFail(method, path)
    }
  }
  return namespaceHardFail(method, path)
}
