// Project-picker policy (design 04 §4.1.5, wave U05). Pure helpers so the 0/1/N
// behaviour is asserted without a DOM. Source is B11 GET /api/project (the
// ReadScopes-intersected project register), NOT the raw whoami.read_scopes list
// (which carries home/shared and non-project scopes, types.ts:9-10).

import type { ProjectRow } from '../../lib/api/types'

/** Picker render mode from the project count (§4.1.5). */
export type PickerMode = 'empty' | 'single' | 'multi'

export function pickerMode(projects: ProjectRow[]): PickerMode {
  if (projects.length === 0) return 'empty'
  if (projects.length === 1) return 'single'
  return 'multi'
}

/**
 * The scope to activate given the loaded projects and the URL scope. URL is the
 * single source of truth (§4.1.5): an explicit, still-valid `?scope=` wins; a
 * lone project auto-selects (writes its scope back); otherwise nothing is
 * selected (the N-case waits for a user pick). A URL scope that is NOT among the
 * visible projects resolves to null — a stale/foreign deep link never silently
 * targets the wrong project (it falls back to the picker prompt / auto-select).
 */
export function resolveScope(projects: ProjectRow[], urlScope: string | null): string | null {
  if (urlScope !== null && projects.some((p) => p.scope === urlScope)) return urlScope
  if (urlScope === null && projects.length === 1) return projects[0].scope
  return null
}

/** The project whose scope matches, or null. The list API is keyed by project
 * id, so the resolved scope must be mapped back to its id before any fetch. */
export function projectForScope(projects: ProjectRow[], scope: string | null): ProjectRow | null {
  if (scope === null) return null
  return projects.find((p) => p.scope === scope) ?? null
}
