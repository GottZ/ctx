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

The stack runs with `CTX_GATEWAY_ENABLED=true` (only here; the production
default stays off): a tenant-admin reaches `/gateway` only while the model API
is on, and since F-W8 ctxd refuses that boot unless the model group registers
its ingress protection — so every live run also proves the gateway-on boot.

## Specs

| spec | proves |
|---|---|
| `login.spec.ts` | real key → shell; wrong key → error band, no shell |
| `store-roundtrip.spec.ts` | store → search → detail over the real handlers |
| `tenant-isolation.spec.ts` | B finds its own sentinel block, never A's |
| `sse-transport.spec.ts` | real `/api/events` frames + client reconnect |
| `model-consumers.spec.ts` | B's `/gateway` lists B's consumer, never A's; a `ctxm_` model key at the login form and at `POST /auth/login` ⇒ 401, no session |

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
  tenant A's sentinel into tenant B (a block, and a model consumer carrying A's
  consumer label); `tenant-isolation.spec.ts` and the `/gateway` probe of
  `model-consumers.spec.ts` go RED, proving the isolation detectors detect.
- **(d) model-key swap** — `CTX_E2E_MODELKEY_SWAP=1 bun run e2e:live` hands
  B's knowledge owner key to the specs as B's model key; the login form and
  `POST /auth/login` then sign in and the two model-key probes of
  `model-consumers.spec.ts` go RED, proving the "model key is no web login"
  probe detects a credential that does sign in.

## Secrets in artifacts (§4.8)

`trace: 'retain-on-failure'` records `Authorization`/`X-Context-Key` headers,
and the tenant-create response carries the reveal-once `owner_key` in clear.
**Impact is null** because §3.6 invariant 3 holds: **every live-tier key is
valid ONLY against the job-local, end-of-run-destroyed instance.** Live traces
therefore get `retention-days: 3` in CI (mock reports get 14 — no real keys
flow there).

`model-consumers.spec.ts` goes further and leaves no clear key in its own
artifacts: B's reveal-once `ctxm_` key lives only in the seed state file. A
trace records every fill value and request body, and a failing locator
matcher embeds an aria snapshot carrying a password input's value
(`error-context.md`) — so that file runs with `trace: 'off'`, blanks every
input right after the login exchange, and blanks them again in `afterEach`.

> **Staging-redaction caveat.** The moment the live tier is pointed at a
> **long-lived staging instance** instead of a throwaway one, invariant 3
> breaks and a leaked key has real impact. Playwright ships NO default trace
> redaction — so a staging switch has **trace redaction as a hard
> precondition**, not an afterthought.

## Release-gate rule

**A version tag needs a GREEN live-tier run on the tagged commit** — the
extension of the repo's "CI is truth, not local" rule (MEMORY
`feedback_ci_is_truth_not_local`) to the live tier. The `web-live` job
(`.github/workflows/ci.yml`) runs on every push to `root`, so every merge
commit — every release candidate — carries its verdict; the `verify` job of
`release.yml` refuses a tag whose commit has no successful `Web Live Tier`
check run (see [GOVERNANCE.md](../../../../GOVERNANCE.md), Releases). The
nightly run is the daily drift signal, `workflow_dispatch` runs it on demand,
and a PR carrying the `e2e-live` label runs it on that PR (backend PRs that
change enforcement/shapes/SSE should set the label).

## Scope honesty (W21)

The SSE spec proves connect + real frames + client-driven reconnect. The
**server-restart** reconnect variant (§4.7) needs an orchestrated ctxd bounce
mid-test and is NOT automated here — a documented nightly/manual extension.
Issue live-seeds ride the Achse-02 write path (`issue-create`); it is a
non-fatal bonus write and does not gate the isolation/roundtrip/SSE proofs.
