// Project-picker policy pins (design 04 §4.1.5, wave U05): 0/1/N modes,
// URL-wins scope resolution, and the scope→id mapping the list fetch needs.

import { describe, expect, it } from 'vitest'
import { pickerMode, projectForScope, resolveScope } from './picker'
import type { ProjectRow } from '../../lib/api/types'

function proj(scope: string, id = scope): ProjectRow {
  return {
    id,
    tenant_id: 't',
    scope,
    identity: `github:${scope}`,
    display_name: scope,
    forge: null,
    webhook_secret_ref: null,
    sync_status: 'idle',
    sync_enabled: true,
    push_enabled: false,
    last_sync_at: null,
    sync_cursor: null,
    created_at: '2026-07-01T00:00:00Z',
    metadata: {},
  }
}

describe('pickerMode', () => {
  it('maps the project count to empty/single/multi', () => {
    expect(pickerMode([])).toBe('empty')
    expect(pickerMode([proj('a:main')])).toBe('single')
    expect(pickerMode([proj('a:main'), proj('b:main')])).toBe('multi')
  })
})

describe('resolveScope', () => {
  it('auto-selects the lone project when the URL carries no scope', () => {
    expect(resolveScope([proj('a:main')], null)).toBe('a:main')
  })

  it('honours an explicit valid URL scope (URL is the source of truth)', () => {
    expect(resolveScope([proj('a:main'), proj('b:main')], 'b:main')).toBe('b:main')
  })

  it('drops a URL scope that is not among the visible projects (stale/foreign link)', () => {
    expect(resolveScope([proj('a:main')], 'ghost:main')).toBeNull()
  })

  it('selects nothing for N projects without a URL scope', () => {
    expect(resolveScope([proj('a:main'), proj('b:main')], null)).toBeNull()
  })

  it('selects nothing when there are no projects', () => {
    expect(resolveScope([], null)).toBeNull()
    expect(resolveScope([], 'a:main')).toBeNull()
  })
})

describe('projectForScope', () => {
  it('maps a scope back to its project (the list API is keyed by id)', () => {
    const projects = [proj('a:main', 'id-a'), proj('b:main', 'id-b')]
    expect(projectForScope(projects, 'b:main')?.id).toBe('id-b')
    expect(projectForScope(projects, 'ghost')).toBeNull()
    expect(projectForScope(projects, null)).toBeNull()
  })
})
