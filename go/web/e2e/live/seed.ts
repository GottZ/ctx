// e2e live-tier seed (design 06 §3.6 / §4.7, wave PV10). Builds the test corpus
// EXCLUSIVELY through production write paths (tenant-create → store →
// issue-create) against the throwaway ctxd — so the seed itself IS an
// integration test of those paths and no hand-written fixture shape can drift
// from the server. Runs as the live Playwright globalSetup; also runnable
// standalone (`bun seed.ts`) for the PV10 negative gates.
//
// FAIL-CLOSED TARGET GATE (§3.6, three layers, ALL before the first write):
//   1. Env-gate: CTX_E2E_LIVE=1 AND the baseURL host is localhost/127.0.0.1 or
//      a compose service name (ctx-e2e/ctx). The admin key is read ONLY from
//      CTX_E2E_ADMIN_KEY — NEVER from ~/.config/ctx/config.
//   2. Instance handshake: GET /api/whoami must authenticate as exactly the
//      per-run key AND carry the label e2e-bootstrap-<run-id>. A production or
//      foreign instance does not know the per-run random key ⇒ 401 ⇒ zero
//      writes. The bootstrap key IS the instance marker (PV10a).
//   3. Key-validity invariant (documented, enforced by construction): every key
//      minted here is valid ONLY against this job-local instance, which dies
//      with the compose stack (README §retention).
//
// On ANY gate failure the seed logs `SEED-REFUSAL writes=<n> reason=...` and
// aborts — n is 0 because the handshake is a READ that precedes every write.

import { writeFileSync, mkdirSync } from 'node:fs'
import { dirname } from 'node:path'

const RUN_ID = process.env.CTX_E2E_RUN_ID ?? 'unknown'
const EXPECTED_LABEL = `e2e-bootstrap-${RUN_ID}`

// Write accounting: every mutating request increments this. The refusal log
// prints it so the target-gate negative probe can assert `writes=0`.
let writeCount = 0

function refuse(reason: string): never {
  // stderr marker consumed by the PV10 target-gate negative probe.
  console.error(`SEED-REFUSAL writes=${writeCount} reason=${reason}`)
  throw new Error(`seed target-gate refusal: ${reason}`)
}

function allowedHost(host: string): boolean {
  return host === 'localhost' || host === '127.0.0.1' || host === 'ctx-e2e' || host === 'ctx'
}

interface SeedConfig {
  baseURL: string
  adminKey: string
  stateFile: string
  leakInject: boolean
}

function readConfig(): SeedConfig {
  // Layer 1 — env-gate.
  if (process.env.CTX_E2E_LIVE !== '1') {
    refuse('CTX_E2E_LIVE is not 1 (this is the live tier; refuse to write to any non-e2e target)')
  }
  const baseURL = process.env.CTX_E2E_BASE_URL ?? ''
  if (!baseURL) refuse('CTX_E2E_BASE_URL is unset')
  let host: string
  try {
    host = new URL(baseURL).hostname
  } catch {
    return refuse(`CTX_E2E_BASE_URL is not a valid URL: ${baseURL}`)
  }
  if (!allowedHost(host)) {
    refuse(`baseURL host ${host} is not a local/compose target — refusing (production-safety)`)
  }
  // The admin key comes ONLY from the per-run env — never from a dev config on
  // disk. An empty key would 401 the handshake anyway, but reject early.
  const adminKey = process.env.CTX_E2E_ADMIN_KEY ?? ''
  if (!adminKey) refuse('CTX_E2E_ADMIN_KEY is unset (seed NEVER reads ~/.config/ctx/config)')

  const stateFile = process.env.CTX_E2E_STATE_FILE ?? ''
  if (!stateFile) refuse('CTX_E2E_STATE_FILE is unset')

  return { baseURL, adminKey, stateFile, leakInject: process.env.CTX_E2E_LEAK_INJECT === '1' }
}

async function whoami(baseURL: string, key: string): Promise<Record<string, unknown>> {
  const res = await fetch(`${baseURL}/api/whoami`, {
    headers: { 'X-Context-Key': key },
  })
  if (!res.ok) throw new Error(`whoami ${res.status}`)
  return (await res.json()) as Record<string, unknown>
}

// post is the ONLY mutating primitive — it increments writeCount, so a refusal
// that fires before any post() leaves writeCount at 0 (the negative-probe
// invariant). Throws on transport error, non-2xx, or {success:false}.
async function post(baseURL: string, key: string, path: string, body: unknown): Promise<Record<string, unknown>> {
  writeCount++
  const res = await fetch(`${baseURL}${path}`, {
    method: 'POST',
    headers: { 'X-Context-Key': key, 'content-type': 'application/json' },
    body: JSON.stringify(body),
  })
  const json = (await res.json().catch(() => ({}))) as Record<string, unknown>
  if (!res.ok || json.success === false) {
    throw new Error(`POST ${path} → ${res.status} ${JSON.stringify(json)}`)
  }
  return json
}

interface TenantSeed {
  slug: string
  scope: string
  ownerKey: string
  sentinel: string
}

async function createTenant(baseURL: string, adminKey: string, slug: string, display: string): Promise<TenantSeed> {
  const res = await post(baseURL, adminKey, '/api/manage', {
    action: 'tenant-create',
    data: { slug, display_name: display },
  })
  const scope = res.scope as string
  const ownerKey = res.owner_key as string
  if (!scope || !ownerKey) throw new Error(`tenant-create ${slug} returned no scope/owner_key`)
  // High-entropy sentinel (NOT a dictionary word — avoids collision with UI
  // copy, design 06 §5.6b). Stored as its OWN block in the tenant's scope.
  const sentinel = `${slug.toUpperCase()}-SENTINEL-${crypto.randomUUID()}`
  return { slug, scope, ownerKey, sentinel }
}

async function storeBlock(baseURL: string, key: string, scope: string, title: string, content: string): Promise<void> {
  await post(baseURL, key, '/api/store', {
    category: 'reference',
    title,
    content,
    scope,
  })
}

export async function runSeed(): Promise<void> {
  const cfg = readConfig()

  // Layer 2 — instance handshake (a READ; NO write yet, writeCount stays 0).
  let who: Record<string, unknown>
  try {
    who = await whoami(cfg.baseURL, cfg.adminKey)
  } catch (err) {
    refuse(`whoami handshake failed (${(err as Error).message}) — target does not know the per-run key`)
  }
  if (who.success !== true || who.admin !== true) {
    refuse(`handshake key is not a valid server-admin (admin=${who.admin})`)
  }
  if (who.label !== EXPECTED_LABEL) {
    refuse(`handshake label ${JSON.stringify(who.label)} != expected ${EXPECTED_LABEL} — wrong instance`)
  }
  console.log(`seed: target-gate PASSED — server-admin ${EXPECTED_LABEL} on ${cfg.baseURL}`)

  // ---- Seeds (production write paths only) ----
  const a = await createTenant(cfg.baseURL, cfg.adminKey, 'e2e-a', 'E2E Tenant A')
  await storeBlock(cfg.baseURL, a.ownerKey, a.scope, a.sentinel, `Tenant A sentinel block. ${a.sentinel}`)

  const b = await createTenant(cfg.baseURL, cfg.adminKey, 'e2e-b', 'E2E Tenant B')
  await storeBlock(cfg.baseURL, b.ownerKey, b.scope, b.sentinel, `Tenant B sentinel block. ${b.sentinel}`)

  // Store-roundtrip corpus in tenant B (login → search → detail spec).
  const roundtripTitle = `E2E roundtrip block ${crypto.randomUUID()}`
  await storeBlock(cfg.baseURL, b.ownerKey, b.scope, roundtripTitle, 'Roundtrip content stored via the live /api/store handler.')

  // Achse-02 write path: an issue in tenant B (the workflow surfaces are live).
  let issueTitle: string | null = null
  try {
    issueTitle = `E2E live issue ${crypto.randomUUID()}`
    await post(cfg.baseURL, b.ownerKey, '/api/manage', {
      action: 'issue-create',
      data: { scope: b.scope, title: issueTitle, content: 'Issue seeded through the live Achse-02 write path.' },
    })
  } catch (err) {
    // Non-fatal: the issue seed is a bonus write-path exercise; the isolation /
    // roundtrip / SSE proofs do not depend on it. Surface it, do not abort.
    console.warn(`seed: issue-create skipped (${(err as Error).message})`)
    issueTitle = null
  }

  // Leak-detector negative gate (design 06 §5.6b / PV10 gate c): when armed,
  // inject tenant A's sentinel INTO tenant B's scope. The isolation spec then
  // finds the A-sentinel under B's session ⇒ the isolation probe goes red,
  // proving the detector actually detects.
  if (cfg.leakInject) {
    await storeBlock(cfg.baseURL, b.ownerKey, b.scope, `LEAK-INJECT ${a.sentinel}`, `Injected A sentinel: ${a.sentinel}`)
    console.warn('seed: LEAK INJECTED — A sentinel written into tenant B (negative gate c)')
  }

  const state = {
    baseURL: cfg.baseURL,
    runId: RUN_ID,
    bootstrapLabel: EXPECTED_LABEL,
    bootstrapKey: cfg.adminKey,
    tenants: { a, b },
    roundtripTitle,
    issueTitle,
    writes: writeCount,
  }
  mkdirSync(dirname(cfg.stateFile), { recursive: true })
  writeFileSync(cfg.stateFile, JSON.stringify(state, null, 2))
  console.log(`seed: DONE — ${writeCount} writes, state → ${cfg.stateFile}`)
}

// Playwright globalSetup entry: a throw here aborts the whole run before any
// spec (the fail-closed guarantee at the run level).
export default async function globalSetup(): Promise<void> {
  await runSeed()
}

// Standalone entry for the PV10 target-gate negative probe (`bun seed.ts`).
if (import.meta.main) {
  runSeed().catch((err) => {
    console.error(String(err))
    process.exit(1)
  })
}
