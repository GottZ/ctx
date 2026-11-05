// Shared reader for the live-tier seed state (design 06 §4.7, wave PV10).
// seed.ts (globalSetup) writes this JSON; the @live specs read it. NOT a
// *.spec.ts file, so the live config's testMatch never treats it as a test.
//
// The file holds reveal-once owner keys and tenant B's reveal-once ctxm_ model
// key (wave W05-18) — it lives ONLY under the gitignored
// e2e/live/.results/ of the ephemeral run and the keys are valid solely
// against the job-local instance that dies with the compose stack (§3.6
// invariant 3). It is never committed and never uploaded outside the
// retention-3 live artifact set.

import { readFileSync } from 'node:fs'

export interface TenantSeed {
  slug: string
  /** tenant UUID (the tenant_id of model-consumer-create). */
  id: string
  scope: string
  ownerKey: string
  sentinel: string
}

/** Model consumers seeded through model-consumer-create (wave W05-18). */
export interface ConsumerSeeds {
  /** tenant A's consumer — B's /gateway must never list its label. */
  a: { label: string }
  /**
   * tenant B's consumer: its label (the positive control on B's /gateway) and
   * its ctxm_ key — under CTX_E2E_MODELKEY_SWAP=1 B's owner key instead.
   */
  b: { label: string; modelKey: string }
  /** rows B's /gateway list holds: B's own seeded consumers (a leak is not counted). */
  bRows: number
}

export interface SeedState {
  baseURL: string
  runId: string
  bootstrapLabel: string
  bootstrapKey: string
  tenants: { a: TenantSeed; b: TenantSeed }
  roundtripTitle: string
  issueTitle: string | null
  consumers: ConsumerSeeds
  writes: number
}

export function readState(): SeedState {
  const file = process.env.CTX_E2E_STATE_FILE
  if (!file) throw new Error('CTX_E2E_STATE_FILE unset — run via run-live.sh')
  return JSON.parse(readFileSync(file, 'utf8')) as SeedState
}
