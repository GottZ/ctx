# Security

## Admin tier

(BREAKING, migration 052.) Keys carry an `is_admin` flag (default `false`, no key is auto-promoted). These `/api/manage` actions require an admin key — **BREAKING for previously-working non-admin keys**: `api-key-create`, `api-key-list`, `api-key-delete`, `mcp-client-create`, `mcp-client-list`, `mcp-client-delete`, and `dream-mode` when mutating (reading the current mode stays open).

**Rationale.** Before this gate, ANY valid key of any `home_scope` could mint keys for arbitrary scopes — read access to foreign tenants — and the settings/secrets API must not inherit that model. The per-tenant admin tier (owner/admin/member) that layers on top is documented in [multi-tenancy](multi-tenancy.md#admin-tiers).

**Admin bootstrap** (one-time, host access required). Promote by `id`, never by label — `label` has no UNIQUE constraint and an UPDATE by label would escalate every same-named key, including inactive ones:

```bash
# 1. Inspect candidates:
docker exec -e PGPASSWORD="$CONTEXT_DB_PASSWORD" n8n-db-1 \
  psql -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" \
  -c "SELECT id, label, active, home_scope, is_admin FROM context_api_keys;"
# 2. Promote EXACTLY one key by id:
docker exec -e PGPASSWORD="$CONTEXT_DB_PASSWORD" n8n-db-1 \
  psql -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" \
  -c "UPDATE context_api_keys SET is_admin = true WHERE id = '<uuid>';"
```

**Admin-key hygiene.** Since S3 (migration 099) the OAuth `/token` exchange hands out an **opaque, key-bound, revocable `ctxt_` token** (SHA-256 at rest, 1h TTL) instead of the api key itself — what circulates through claude.ai/Cloudflare and external connector storage is no longer the long-lived key. The raw api key **stays valid as a Bearer value** (deliberate E2 legacy path for existing connectors), so a key pasted directly into a connector still circulates; the hygiene rule therefore stands: create a **dedicated admin key that is never used as an MCP/OAuth credential**; the claude.ai MCP key stays non-admin. Test/eval script keys stay non-admin too (least privilege). Revoking a key (or deactivating its principal) kills every token minted from it instantly — token resolution runs through the same `ctx_auth_by_id` gates as key auth.

## Sealed secrets & break-glass

Provider credentials live **AES-256-GCM-sealed** in `context_secrets` (encrypted in Go — never via pgcrypto, the master key must not cross the SQL wire). The AAD binds each ciphertext to its `name`+`scope` row identity, so a ciphertext copied onto another row fails authentication. Writes go through the admin-gated, **write-only** `/api/secrets` (set/rotate/delete — values never appear in any response, list shows metadata + `referenced_by` only, no fingerprints); settings reference a secret by *name* (`secret_ref`), resolved to plaintext exclusively inside the in-memory snapshot. A rotation or revocation reloads the snapshot immediately — no settings write needed, the incident-response path is never silently inert. Deleting a secret that settings still reference is a `409` listing the keys.

Per-tenant secret resolution (the `tenant.allow_shared_secrets` opt-in, default strict isolation) is documented in [multi-tenancy](multi-tenancy.md#per-tenant-settings--secrets).

**Master key setup** (one-time):

```bash
# generate and append to .env:
echo "CTX_SECRETS_KEY=$(openssl rand -hex 32)" >> .env
```

**Mandatory: copy `CTX_SECRETS_KEY` into your password manager when you set it.** `backup.sh` archives only the pg_dumps — the ciphertexts are in every dump, the master key is in none (deliberate: the key stays spatially separated from the ciphertexts it opens, so disaster recovery needs both places). **Key loss = total loss of all sealed secrets, by design.** No recovery mechanism; re-enter the provider keys instead.

**Master-key rotation.** Generate a new key, move the old value to `CTX_SECRETS_KEY_PREV`, put the new one in `CTX_SECRETS_KEY`, restart ctx. The boot sweep re-seals every secret it can open with the previous key (`key_version` bump, log line per name, one transaction per row); it logs a completion line — `re-encrypt sweep complete` means remove `CTX_SECRETS_KEY_PREV` from `.env`, a `finished with failures` WARN means keep it set and investigate. Secrets that open with neither key are left untouched (WARN per name, no boot abort, no data loss). The *value* rotation of a single provider key is `PUT /api/secrets/{name}` (or `ctx secrets rotate`) — no restart, propagates immediately.

**Break-glass extraction** (host access; works even when the ctx container crash-loops — the decrypt mode reads ONLY env + stdin, no DB):

```bash
./break-glass.sh secret <name> [scope]     # prints the plaintext
./break-glass.sh reset-settings [key]      # factory-reset settings overrides (audited via DB trigger)
```

`openssl enc` cannot do AES-GCM, so extraction pipes the row through the ctxd binary itself: `psql -At … | docker run --rm -i -e CTX_SECRETS_KEY -e CTX_SECRETS_KEY_PREV n8n-ctx -secret-decrypt`. PostgreSQL's `encode(bytea,'base64')` is MIME (RFC 2045) and wraps every 76 chars — the script strips the wraps SQL-side, and the decrypt mode additionally reads stdin to EOF and strips CR/LF, so every realistic provider-key length survives the pipe.

## Trust × sensitivity gating

(Backend pool, migration 055.) A backend carries a **trust level** and an egress `locality`; every block carries a `sensitivity`. A backend with trust `T` may receive content of sensitivity `S` iff `rank(S) ≤ maxRank(T)` — `full-trust` ≥ credentials, `no-credentials` ≥ personal, `non-personal` ≥ internal, `public` = public only. Empty/unknown sensitivity counts as `credentials`; an empty chain is an error, **never a silent escalation across trust borders**.

**Block sensitivity.** Every block carries `sensitivity` (default `credentials` — unclassified content never leaves full-trust backends; normal operation is untouched while all backends are full-trust) plus `sensitivity_source` (`default`/`llm-audit`/`pattern`/`manual`; `manual` is untouchable for the audit wave) and `sensitivity_audited_at`. The query path batch-annotates **all** RRF candidates after graph expansion (a supersedes/graph straggler from beyond rank 50 still carries its level into the gate; a lookup miss acts as `credentials`), applies the scope floor `pool.scope_sensitivity_floor` (a JSON map `scope → minimum level`; it can only RAISE — blanket protection for friend-tenant scopes without block mutation), and gates each role with its real requirement: query-only roles (translate, temporal, query-embed) with the query sensitivity, rerank with `max(query, judged docs)`, synthesis with `max(query, final prompt set)`, inline backfill per block.

**Downgrade guard** (both directions of the same border). Lowering a block's sensitivity needs `confirm_sensitivity_downgrade:true` on `manage update` (audited to `metadata.sensitivity_audit`), exactly like raising a backend's trust needs `confirm_trust_elevation`; the settings defaults `pool.default_block_sensitivity`/`pool.default_query_sensitivity` are guard-marked the same way.

**Eject toggle** (AM-7: manage action `eject-mode`, alias `gaming-mode`; CLI command `ctx eject`, legacy alias `ctx gaming`). `ctx eject on` flips the GPU-host backends (default `herbert-chat` + `herbert-rerank`) out of EVERY chain so the GPU is free — `llama-cpu` and any external backend stay in as failover. Since U01-W5 the toggle IS the reserved `eject` disable-profile (092): a profile write, admin-gated (an ungated toggle would let any tenant key flip the system's egress topology) and persistent — it SURVIVES a restart (the dream-mode break path, where a restart drops the GPU lock, is the anti-pattern it avoids) and takes effect on the next chain without one. In-flight requests finish normally. `/health` never carries the eject flag (it would be an "admin sits at the GPU host" presence oracle).

The legacy `gaming.active`/`gaming.disabled_backends` settings keys were **retired** in U01-W5: nothing reads them anymore. Any such rows left in `context_settings` are inert (dropped as unknown keys with a WARN), and a psql/break-glass edit to a `gaming.active` row is **a no-op** — the truth is now the `eject` profile row (`context_disable_profiles`, `scope='_global'`, `name='eject'`). Toggle via `ctx eject on|off` (legacy alias `ctx gaming`), the disable-profiles card on the backends settings page (U01-W6: member chips + role impact shown before the click, role-blackout activations require an explicit confirm step), or flip `context_disable_profiles.active` directly. The status payload still exposes a `gaming.active` field for the frontend, but it is derived live from the eject profile's active state, not from any settings row.

**OpenRouter ZDR.** External backends of `provider_class: "openrouter"` always carry `provider.zdr:true` + `provider.data_collection:"deny"`, independent of trust level — see [api](api.md#backend-pool). Raising the backend to full-trust never silently drops the ZDR guarantee.

## Sensitivity classification

**LLM audit (G41).** `ctx blocks audit start` (manage action `blocks-audit-start`, **admin**) classifies every home-scope block still at `sensitivity_source='default'` out of the fail-closed credentials default: two SEPARATE yes/no questions per block over the `classify` role chain — one for schützenswerte credentials, one for personenbezogene Daten — answered as strict JSON booleans (deliberately NO confidence field: local-model self-reported confidence is uncalibrated). Verdict table: credentials-yes keeps `credentials` (the personal question is skipped), no+personal-yes → `personal`, no×2 → `internal`; `public` is **never** assigned by the audit (that stays manual). A parse failure is no verdict (the block keeps the credentials default and a 24h retry cooldown); a chain/backend failure **aborts the run** instead of cooling down blocks the model never judged. `manual` rows are untouchable by the SQL predicate itself. The `classify` role is **hard-local**: `backend-create`/`update` rejects `classify` on `locality='external'` with 422 (no metadata escape hatch — audit prompts carry unclassified block content by definition, full-trust ZDR included); the chain executor additionally drops external rows at call time. Gate a bulk run with `ctx blocks audit sample --n 30` (30 random pending blocks, no writes, reports would-be verdicts).

**Credentials pattern detector (G40).** A deterministic, LLM-free scanner (`internal/sensitivity`) that only ever RAISES content to `credentials` — never downgrades. It runs at two points automatically: on `POST /api/store` (a content hit forces `credentials` with `sensitivity_source='pattern'`, records the secret-free reason in `metadata.sensitivity_detector`) and on `POST /api/query` (a hit in the query text raises the operation's required sensitivity). Rule set (precision over recall — a false positive permanently blocks external failover for that block): AWS key ids, PEM private-key headers, JWTs, vendor token prefixes (`sk-`/`ghp_`/`xox…`/`AIza…`/`glpat-`), entropy- and placeholder-gated secret assignments, high-entropy base64 blobs (≥32 chars, >4.5 bits/char), long hex blobs (≥64). The bulk re-audit `ctx blocks classify start` (manage action `blocks-classify-start`, **admin**) keyset-walks every home-scope block not already `credentials` and not `manual`, raising hits — the deterministic **veto** against the G41 audit (a `pattern` row is outside the audit's pick set, so the LLM can never downgrade a pattern hit). Always `dry-run` first (`ctx blocks classify dry-run`) — it scans the real corpus WITHOUT writing and lists what would be raised, the empirical false-positive gate. Once the corpus is classified, `pool.default_block_sensitivity` can be lowered to `personal` via the guarded settings write.

## Prompt-injection hardening (promptguard)

**Doctrine:** foreign text enters an LLM prompt through exactly one function. `internal/promptguard` is that entry — pure functions, no DB access, no configuration: `NewNonce` (16-hex per prompt build), `Neutralize` (breaks control tokens — `<|`, double-newline turn markers, guard-tag delimiters — with U+034F so the readable text stays intact; idempotent, and deliberately narrow: `|>` is content), `ClampLine` (line breaks → U+23CE for line-based prompt positions), `Wrap` (nonce-carrying `<untrusted_block>` markers; every attribute position — name, value, tag, nonce — runs through a full-match allowlist clamp a caller cannot bypass), `Rule` (the nonce-bound system-prompt sentence that makes the block boundary verifiable instead of believed) and `Canonicalize` (zero-nonce substitution for deterministic prompt goldens). The guard is structural — it removes forgeable tokens and boundaries; it never claims model obedience. Call-site wiring lands wave by wave. Wired so far: `dream-keywords` and `dream-temporal` (fixed marker + Neutralize before the existing XML escape; the temporal cut is rune-safe now), `dream-recurrence` (ONE nonce binding both `Wrap`ed blocks and the `Rule` in the system prompt; block metadata rides a line-clamped header line above the marker, because a 36-char uuid and a spaced title cannot pass the marker-attribute clamp). Also wired: `dream-daily-synthesis` (the daily report prompt is line-based — every DB-sourced value runs `ClampLine` + `Neutralize`, so a newline in a title or an aggregate label can no longer forge an extra item line). Also wired: the chat tool return (`web-chat`: every foreign field of a tool result — title, category, tags, preview/content — is neutralised BEFORE `mustJSON`; JSON escaping alone only looked safe because `encoding/json` HTML-escapes `<` by default, an encoder default a routine `SetEscapeHTML(false)` would drop silently). Known open fifth path there: `runUpdate` serialises the staged-write card whose `Category`/`Title` come from the TARGET block — deliberately unguarded for now because the same struct feeds the SPA confirm card (Ops surface). Outstanding: `query-synthesize`, `query-rerank-judge`, `dream-eval` (eval-gated) and `sensitivity-audit`.
