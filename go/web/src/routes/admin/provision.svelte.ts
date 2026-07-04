// Project-provisioning wizard model (design 04 §7-U12, wave U12) — the
// resumable checkpoint + the three ordered write steps over the REAL compound
// model (workflow-seams.md §7.1/§7.2). Mirrors the TenantsModel shape: a plain
// $state class with an injectable api so vitest covers the flow ordering without
// a DOM. The wizard is a SEQUENCER over the existing manage-actions — it adds NO
// new backend compound; the CLI `ctx project init` (project-provision) is the
// other frontend of the same path (design 04 §7-U12 line 444).
//
// The three ordered steps (new-tenant flow):
//   1. tenant-create  — the ATOMIC compound (K10): tenant row + limit seeding +
//      initial `<slug>:main` scope + owner-key, all in ONE Tx. A "tenant without
//      a scope" is UNPRODUCIBLE, so after step 1 the register shows a consistent
//      tenant (the alt-flow skips this — it targets an EXISTING tenant).
//   2. scope-create   — the repo scope (bare name; the server prefixes
//      `<slug>:<name>` from the target tenant — S1, prefix injection impossible).
//   3. api-key-create — the K12 agent-key template: home_scope=repo scope,
//      allowed_scopes=[], write_scopes=[] (home-only read+write; NO shared).
//
// HYGIENE (design 05 §6): the owner-key + agent-key PLAINTEXT are returned to the
// wizard for reveal-and-discard and are NEVER held on this model — the model
// carries only the non-secret checkpoint (tenant id/slug + the built repo scope +
// the stage), exactly so a close→reopen RESUMES without ever re-showing a key.

import { toApiError } from '../../lib/api'
import { createTenant, type TenantSpec } from '../../lib/api/tenants'
import { createScope } from '../../lib/api/scopes'
import { createApiKey } from '../../lib/api/keys'
import type { ApiKeyCreateResult, ScopeCreateResult, TenantCreateResult } from '../../lib/api/types'

/** Wizard entry mode: a brand-new tenant (3 steps) or a repo in an EXISTING one (2 steps). */
export type WizardMode = 'new' | 'existing'

/**
 * The persistent checkpoint. 'entry' = mode chooser; 'tenant' = step 1 form;
 * 'scope' = step 2 form; 'key' = step 3 form; 'done' = summary. Only 'scope' and
 * 'key' are RESUMABLE mid-flow states (a tenant/scope already exists server-side).
 */
export type WizardStage = 'entry' | 'tenant' | 'scope' | 'key' | 'done'

/** Injectable seam: the three ordered manage-action writes (vitest covers ordering). */
export interface ProvisionApi {
  createTenant: typeof createTenant
  createScope: typeof createScope
  createApiKey: typeof createApiKey
}

export class ProvisionModel {
  mode = $state<WizardMode | null>(null)
  stage = $state<WizardStage>('entry')
  /** The target tenant (created in step 1, or chosen in the alt-flow). */
  tenantId = $state<string | null>(null)
  tenantSlug = $state<string | null>(null)
  /** The FULL server-built repo scope (`<slug>:<name>`), set after step 2. */
  repoScope = $state<string | null>(null)
  busy = $state(false)
  /** Last step failure — surfaced as the inline banner; the input is kept (U10 draft). */
  error = $state<string | null>(null)

  #api: ProvisionApi

  constructor(
    api: ProvisionApi = { createTenant, createScope, createApiKey },
  ) {
    this.#api = api
  }

  /** A mid-flow checkpoint exists (tenant and/or repo scope already provisioned)
   *  — the AdminPage shows a Resume affordance for it. */
  get resumable(): boolean {
    return this.stage === 'scope' || this.stage === 'key'
  }

  /** Fresh wizard from the entry chooser (a completed/abandoned run resets here). */
  reset(): void {
    this.mode = null
    this.stage = 'entry'
    this.tenantId = null
    this.tenantSlug = null
    this.repoScope = null
    this.busy = false
    this.error = null
  }

  /** Entry → new-tenant flow (step 1 first). */
  chooseNew(): void {
    this.mode = 'new'
    this.stage = 'tenant'
    this.error = null
  }

  /** Entry → existing-tenant flow: skip step 1, target the chosen tenant (§9.7, 2 calls). */
  chooseExisting(tenantId: string, tenantSlug: string): void {
    this.mode = 'existing'
    this.tenantId = tenantId
    this.tenantSlug = tenantSlug
    this.stage = 'scope'
    this.error = null
  }

  /**
   * Step 1 — the atomic tenant-create compound. Records the created tenant and
   * ADVANCES to 'scope' on success (the tenant now exists consistently, so even a
   * close during the owner-key reveal resumes at step 2). Returns the reveal-once
   * result (owner-key plaintext) for the wizard to reveal-and-discard; throws
   * ApiError (409 slug-exists / 400) for the form to render with the input kept.
   */
  async createTenantStep(spec: TenantSpec): Promise<TenantCreateResult> {
    if (this.busy) throw new Error('busy')
    this.busy = true
    this.error = null
    try {
      const res = await this.#api.createTenant(spec)
      this.tenantId = res.tenant.id
      this.tenantSlug = res.tenant.slug
      this.stage = 'scope'
      return res
    } catch (err) {
      this.error = toApiError(err).message
      throw err
    } finally {
      this.busy = false
    }
  }

  /**
   * Step 2 — the repo scope. The client sends only the bare `name` (+ the target
   * tenant_id, server-admin override); the server builds `<slug>:<name>` and
   * echoes it (S1). Records the FULL scope + advances to 'key' on success. A 409
   * (scope exists) throws with the stage UNCHANGED, so the wizard keeps the input
   * (U10 draft) and stays on step 2.
   */
  async createScopeStep(name: string): Promise<ScopeCreateResult> {
    if (this.busy) throw new Error('busy')
    this.busy = true
    this.error = null
    try {
      const res = await this.#api.createScope({ name, tenant_id: this.tenantId ?? undefined })
      this.repoScope = res.scope
      this.stage = 'key'
      return res
    } catch (err) {
      this.error = toApiError(err).message
      throw err
    } finally {
      this.busy = false
    }
  }

  /**
   * Step 3 — the K12 agent key: home_scope=the repo scope, allowed_scopes=[] (no
   * shared read), write_scopes=[] (home-only write). Returns the reveal-once
   * plaintext for reveal-and-discard; advances to 'done'.
   */
  async mintAgentKeyStep(label: string): Promise<ApiKeyCreateResult> {
    if (this.busy) throw new Error('busy')
    if (this.repoScope === null) throw new Error('no repo scope to bind the agent key to')
    this.busy = true
    this.error = null
    try {
      const res = await this.#api.createApiKey({
        label,
        home_scope: this.repoScope,
        allowed_scopes: [], // K12: no shared read — home-only
        write_scopes: [], // K12: home-only write
        tenant_id: this.tenantId ?? undefined,
      })
      this.stage = 'done'
      return res
    } catch (err) {
      this.error = toApiError(err).message
      throw err
    } finally {
      this.busy = false
    }
  }
}
