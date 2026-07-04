# Live tier (PV10) — real ctxd + Postgres, production write paths

The live tier proves **classes of statements the mock tier cannot** (design 06
§4.7): real server enforcement (tenant isolation), fixture-shape truth (W10),
and real SSE transport. It is deliberately small (≤ 15 `@live` specs) — it
proves *classes*, not surface; surface belongs to the mock tier.

## Run it

```bash
cd go/web
bun run e2e:live          # = bash e2e/live/run-live.sh
```

`run-live.sh` is ONE code path (local == CI):

1. Generates a **per-run** random bootstrap key, run id and DB password (env
   only — never committed).
2. `docker compose -p ctx-e2e-live` brings up a **throwaway** Postgres + the
   **real ctx image** (built from `go/Dockerfile`, so the embedded SPA is the
   release SPA — verify-lens = release-lens).
3. ctxd's PV10a fail-closed bootstrap mints the first server-admin key from the
   per-run key **because the DB starts empty**.
4. Waits for `/health`, then runs `seed.ts` (fail-closed target gate →
   production-path seeds) + the `@live` specs inside the pinned toolchain image
   (`e2e/toolchain.lock`, `--network host`).
5. Tears the stack down — the DB, and every key with it, dies.

## Fail-closed target gate (`seed.ts`, §3.6)

Three layers, ALL before the first write; any failure ⇒ `SEED-REFUSAL
writes=0` + abort:

1. **Env-gate** — `CTX_E2E_LIVE=1` AND baseURL host ∈ {localhost, 127.0.0.1,
   ctx-e2e, ctx}. The admin key comes ONLY from `CTX_E2E_ADMIN_KEY`, **never**
   `~/.config/ctx/config`.
2. **Instance handshake** — `GET /api/whoami` must authenticate as exactly the
   per-run key AND carry label `e2e-bootstrap-<run-id>`. A production/foreign
   instance does not know the per-run random key ⇒ 401 ⇒ zero writes. The
   bootstrap key IS the instance marker.
3. **Key-validity invariant** — every key here is valid ONLY against the
   job-local instance that dies with the stack (§below).

## Negative gates (proved RED-then-GREEN)

- **(a) W10, correct way round** — point the stack at a shape-mutated ctxd
  build (one-line whoami field rename, e.g. `tenant_slug`→`tenantSlug`): the
  live tier goes RED while the mock tier (fixtures+UI consistently stale) stays
  GREEN. A fixtures-only mutation can NOT produce mock-green/live-red (§5.2).
- **(b) target-gate** — run `seed.ts` against a non-e2e target (no run key):
  `SEED-REFUSAL writes=0`, abort before the first write.
- **(c) leak detector** — `CTX_E2E_LEAK_INJECT=1 bun run e2e:live` injects
  tenant A's sentinel into tenant B; `tenant-isolation.spec.ts` goes RED,
  proving the isolation detector detects.

## Secrets in artifacts (§4.8)

`trace: 'retain-on-failure'` records `Authorization`/`X-Context-Key` headers,
and the tenant-create response carries the reveal-once `owner_key` in clear.
**Impact is null** because §3.6 invariant 3 holds: **every live-tier key is
valid ONLY against the job-local, end-of-run-destroyed instance.** Live traces
therefore get `retention-days: 3` in CI (mock reports get 14 — no real keys
flow there).

> **Staging-redaction caveat.** The moment the live tier is pointed at a
> **long-lived staging instance** instead of a throwaway one, invariant 3
> breaks and a leaked key has real impact. Playwright ships NO default trace
> redaction — so a staging switch has **trace redaction as a hard
> precondition**, not an afterthought.

## Release-gate rule

**A version tag is pushed only after a GREEN nightly live-tier run** — the
extension of the repo's "CI is truth, not local" rule (MEMORY
`feedback_ci_is_truth_not_local`) to the live tier. The nightly `web-live` job
(`.github/workflows/ci.yml`) is the enforcement; `workflow_dispatch` runs it on
demand, and a PR carrying the `e2e-live` label runs it on that PR (backend PRs
that change enforcement/shapes/SSE should set the label).

## Scope honesty (W21)

The SSE spec proves connect + real frames + client-driven reconnect. The
**server-restart** reconnect variant (§4.7) needs an orchestrated ctxd bounce
mid-test and is NOT automated here — a documented nightly/manual extension.
Issue live-seeds ride the Achse-02 write path (`issue-create`); it is a
non-fatal bonus write and does not gate the isolation/roundtrip/SSE proofs.
