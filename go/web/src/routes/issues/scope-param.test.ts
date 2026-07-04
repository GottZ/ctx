// Pins parseScopeParam (design 04 §4.1.5, wave U04): the ?scope= deep-link
// parse both /issues and /board read on mount. Absent/blank → null, present →
// the decoded value, other filters ignored, malformed query tolerated.

import { describe, expect, it } from 'vitest'
import { parseScopeParam } from './scope-param'

describe('parseScopeParam', () => {
  it('returns null when no scope param is present', () => {
    expect(parseScopeParam('')).toBeNull()
    expect(parseScopeParam('?q=open&status=todo')).toBeNull()
  })

  it('reads the scope value (with or without the leading ?)', () => {
    expect(parseScopeParam('?scope=acme:research')).toBe('acme:research')
    expect(parseScopeParam('scope=acme:research')).toBe('acme:research')
  })

  it('treats a blank or whitespace-only scope as absent', () => {
    expect(parseScopeParam('?scope=')).toBeNull()
    expect(parseScopeParam('?scope=%20%20')).toBeNull()
  })

  it('decodes the value and ignores the other filter params', () => {
    expect(parseScopeParam('?status=open&scope=team%2Falpha&q=bug')).toBe('team/alpha')
  })

  it('does not throw on a malformed query', () => {
    expect(parseScopeParam('?=&&scope')).toBeNull()
  })
})
