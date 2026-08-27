//go:build integration

// M-W2 — the shadow-visibility seam `shadow_types` on the arm_ranks naht
// (design/05 §4.2/§7), driven through the REAL request path against a PG18
// testcontainer.
//
// The seam adds shadow type names to the p_types_visible list of ctx_rrf and
// ctx_rrf_arms — and to NOTHING else. Every probe below is about one of the
// seven fail-closed gates or about the blast radius the wave promises to keep:
// the post-fusion stages never see a shadow type, and a shadow block leaves the
// server as a UUID plus numbers inside arm_ranks, never as a source.
//
//	go test -tags=integration ./internal/handler/ -run TestMW2 -count=1 -v
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// mw2ShadowType carries retrieval.shadow_measurable = true; mw2PlainType is
	// excluded WITHOUT the flag — the pair that separates G5 from "any excluded
	// type", which is the inversion changelog F-1 caught.
	mw2ShadowType = "mw2-shadow"
	mw2PlainType  = "mw2-plain"

	mw2ShadowBlockID  = "019fa500-0000-7000-9000-0000000002a1"
	mw2ForeignBlockID = "019fa500-0000-7000-9000-0000000002a2"
	mw2ForeignScope   = "mw2foreign"
	mw2ShadowTitle    = "shadow catalog entry"
)

// mw2TypeConfig is the fully spelled-out registry row of §4.2: every field a
// deliberate setting, none a side effect of another.
const mw2TypeConfig = `{"v":1,
  "retrieval":{"policy":"excluded","shadow_measurable":true,"untrusted":true},
  "guard":{"check":false,"candidate":false},
  "dream":{"linkable":false},
  "digest":{"include":false},
  "overview":{"include":false}}`

// mw2PlainConfig is the same visibility class WITHOUT the measurability flag.
const mw2PlainConfig = `{"v":1,"retrieval":{"policy":"excluded"}}`

// mw2Setup extends the B-W2 fixture by two shadow-typed blocks (one in the
// caller's scope, one in a foreign scope) and the two registry rows.
func mw2Setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := bw2Setup(t)
	ctx := context.Background()

	// The shadow block gets a vector COLLINEAR with the fake embed server's
	// query vector, so its cosine is ~1 and it ranks FIRST — ahead of every
	// knowledge block.
	//
	// That is not decoration, it is what makes the sources probe mean anything.
	// The first version of this fixture used bw2Embedding(12) on the assumption
	// "higher k = stronger"; the opposite is true (the base-0.1 vector loses
	// cosine as more components are raised), the block landed at rank 6, and
	// the limit-5 truncation removed it BEFORE the shadow filter ever saw it.
	// The probe passed while proving nothing — caught by negative probe 3, see
	// the wave report.
	for _, b := range []struct{ id, scope string }{
		{mw2ShadowBlockID, bw2Scope},
		{mw2ForeignBlockID, mw2ForeignScope},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
			 VALUES ($1::uuid, 'knowledge', $2, 'shadow catalog body', $3, $4, $5)`,
			b.id, mw2ShadowTitle, b.scope, pgvec.NewVector(mw2TopEmbedding()), mw2ShadowType,
		); err != nil {
			t.Fatalf("insert shadow block %s: %v", b.id, err)
		}
	}

	for _, r := range []struct{ name, cfg string }{
		{mw2ShadowType, mw2TypeConfig},
		{mw2PlainType, mw2PlainConfig},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
			 VALUES ($1, '_global', false, false, $2::jsonb)`, r.name, r.cfg,
		); err != nil {
			t.Fatalf("insert registry row %s: %v", r.name, err)
		}
	}
	return pool
}

// mw2TopEmbedding is collinear with the fake embed server's answer vector
// (bw2NewBackend: component i = (i%2)*2), so its cosine similarity is 1 and a
// block carrying it outranks the whole bw2 fixture.
func mw2TopEmbedding() []float32 {
	e := make([]float32, 1024)
	for i := range e {
		if i%2 == 1 {
			e[i] = 1
		}
	}
	return e
}

// mw2Backends is the pool topology of these probes: embed + translate +
// synthesis, all on ONE locality so a probe can vary it.
func mw2Backends(host, locality string) []backends.Backend {
	return []backends.Backend{
		{ID: "e", Name: "mw2-embed", Host: host, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Locality: locality, Roles: []string{backends.RoleEmbed}},
		{ID: "t", Name: "mw2-chat", Host: host, Protocol: backends.ProtocolOllama,
			Model: "test-chat", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Locality: locality, Roles: []string{backends.RoleTranslate, backends.RoleSynthesis}},
	}
}

// mw2Pool is the LAN pool every probe uses unless it is about locality.
func mw2Pool(host string) *backends.Pool {
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest(mw2Backends(host, backends.LocalityLAN))
	return bpool
}

// mw2Handler boots a registry against the fixture DB — the shadow types live in
// context_block_types, so a compiled-in builtin set would not know them.
func mw2Handler(t *testing.T, pool *pgxpool.Pool, b *bw2Backend, cfg *config.Config) *QueryHandler {
	t.Helper()
	return mw2HandlerWithPool(t, pool, cfg, mw2Pool(b.srv.URL))
}

func mw2HandlerWithPool(t *testing.T, pool *pgxpool.Pool, cfg *config.Config, bpool *backends.Pool) *QueryHandler {
	t.Helper()
	reg := blocktype.NewRegistry()
	if err := reg.Reload(context.Background(), pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	if _, ok := reg.Snapshot().Resolve(mw2ShadowType); !ok {
		t.Fatalf("registry does not know %q after reload — the shadow fixture never loaded", mw2ShadowType)
	}
	return NewQueryHandler(pool, config.NewStore(cfg), bpool, nil, reg, snapshotTestAdmitter(t))
}

// mw2Body builds a request body from fragments, so every probe differs from the
// happy path in exactly the one field it is about.
func mw2Body(extra string) string {
	return `{"query":"` + bw2GoldenQuery + `","synthesize":false,"arm_ranks":true` + extra + `}`
}

// mw2ShadowRequest is the fully legal shadow measurement.
func mw2ShadowRequest() string {
	return mw2Body(`,"shadow_types":["` + mw2ShadowType + `"]`)
}

// mw2IDs reads an id list out of either a string array (fusion_order) or an
// array of objects carrying an "id" (rows, sources).
func mw2IDs(t *testing.T, v any) []string {
	t.Helper()
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		switch x := e.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			id, _ := x["id"].(string)
			out = append(out, id)
		default:
			t.Fatalf("unexpected list element %T", e)
		}
	}
	return out
}

// mw2RepeatedList builds a JSON array naming one type n times — the shape of
// review finding #7: every entry passes G4 and G5, and only a cap stops it.
func mw2RepeatedList(name string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = `"` + name + `"`
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func mw2Contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func mw2Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Probes (a)-(e), (g): the fail-closed gate table (§4.2 G1-G5, G7)
// ---------------------------------------------------------------------------

// TestMW2GateTable pins six of the seven gates by their answer code.
//
// RED before M-W2: json.Decoder in HandleQuery does not know the field, so it
// is dropped silently and EVERY refusal row below answers 200 — a request
// carrying shadow_types runs today as an ordinary measurement query (measured,
// see the wave report).
func TestMW2GateTable(t *testing.T) {
	pool := mw2Setup(t)
	h := mw2Handler(t, pool, bw2NewBackend(t), bw2Config())

	cases := []struct {
		name  string
		admin bool
		body  string
		want  int
	}{
		{"(a) G1 non-admin", false, mw2ShadowRequest(), http.StatusForbidden},
		// G1 must stand on its OWN, not behind the arm_ranks admin gate: this
		// body carries no arm_ranks, so today it is an ordinary 200 and only a
		// shadow gate of its own can refuse it. G1 before G2 is also the reason
		// the answer is 403 and not the 400 G2 would give.
		{"(a) G1 non-admin without arm_ranks", false,
			`{"query":"` + bw2GoldenQuery + `","synthesize":false,"shadow_types":["` + mw2ShadowType + `"]}`,
			http.StatusForbidden},
		{"(b) G2 without arm_ranks", true,
			`{"query":"` + bw2GoldenQuery + `","synthesize":false,"shadow_types":["` + mw2ShadowType + `"]}`,
			http.StatusBadRequest},
		{"(c) G3 with synthesize:true", true,
			`{"query":"` + bw2GoldenQuery + `","synthesize":true,"arm_ranks":true,"shadow_types":["` + mw2ShadowType + `"]}`,
			http.StatusBadRequest},
		{"(c) G3 with synthesize omitted", true,
			`{"query":"` + bw2GoldenQuery + `","arm_ranks":true,"shadow_types":["` + mw2ShadowType + `"]}`,
			http.StatusBadRequest},
		{"(d) G4 unknown type name", true, mw2Body(`,"shadow_types":["nosuchtype"]`), http.StatusBadRequest},
		{"(d) G4 one unknown among known", true,
			mw2Body(`,"shadow_types":["` + mw2ShadowType + `","nosuchtype"]`), http.StatusBadRequest},
		{"(e) G5 deny-list checkpoint, as admin", true,
			mw2Body(`,"shadow_types":["checkpoint"]`), http.StatusBadRequest},
		{"(e) G5 deny-list system-meta, as admin", true,
			mw2Body(`,"shadow_types":["system-meta"]`), http.StatusBadRequest},
		{"(e) G5 deny-list beside a legal type", true,
			mw2Body(`,"shadow_types":["` + mw2ShadowType + `","checkpoint"]`), http.StatusBadRequest},
		{"G5 excluded but not measurable", true,
			mw2Body(`,"shadow_types":["` + mw2PlainType + `"]`), http.StatusBadRequest},
		{"G5 a VISIBLE builtin is not measurable either", true,
			mw2Body(`,"shadow_types":["knowledge"]`), http.StatusBadRequest},
		{"(g) G7 with include_content", true,
			mw2Body(`,"shadow_types":["` + mw2ShadowType + `"],"include_content":true`), http.StatusBadRequest},
		// Review finding #7: a list longer than the registry cannot name that
		// many DISTINCT registered types, so past the cap it is a resource claim
		// and not a measurement.
		{"a list longer than the registry", true,
			mw2Body(`,"shadow_types":` + mw2RepeatedList(mw2ShadowType, 500)), http.StatusBadRequest},
		// Duplicates below the cap stay legal — caller sloppiness, not an attack —
		// but they must not reach p_types_visible twice (pinned separately).
		{"duplicates are accepted", true,
			mw2Body(`,"shadow_types":["` + mw2ShadowType + `","` + mw2ShadowType + `"]`), http.StatusOK},
		// Precedence: a non-admin never learns from the status code whether the
		// rest of the body would have been well-formed.
		{"(a) G1 beats every later gate", false,
			`{"query":"` + bw2GoldenQuery + `","synthesize":true,"shadow_types":["checkpoint"],"include_content":true}`,
			http.StatusForbidden},
		// The field absent is the production path, unchanged.
		{"no shadow_types is the ordinary measurement", true, mw2Body(``), http.StatusOK},
		{"an EMPTY list is not a shadow request", true, mw2Body(`,"shadow_types":[]`), http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := bw2AdminKeyID
			if !tc.admin {
				key = bw2UserKeyID
			}
			rec := bw2Do(t, h, key, tc.admin, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Review finding #3: the F-1 core — no flag can override the deny-list
// ---------------------------------------------------------------------------

// TestMW2DenyListBeatsAFlippedFlag pins the scenario changelog F-1 exists for,
// and it is the ONLY test that can: everywhere else `checkpoint` is refused by
// the flag half (it carries no retrieval.shadow_measurable), so the deny-list
// could be deleted as "redundant" and every other probe would stay green — with
// the 5 955-block pile one registry write away from being measurable.
//
// The registry write is not hypothetical: a `_global` row overrides a builtin
// (blocktype/registry.go, merged[p.Name] = p), and a tenant overlay can do the
// same for its own scope.
//
// The test carries BOTH halves so neither can be removed unnoticed:
//
//	Fall A  checkpoint/system-meta WITH the flag flipped on ⇒ 400
//	        — only the deny-list can answer this one.
//	Fall B  an excluded type WITHOUT the flag ⇒ 400
//	        — only the flag half can answer this one (the deny-list never
//	          heard of mw2-plain).
//
// RED (deny-list gutted, flag flipped): 200 with a full pipeline response.
// RED (flag half gutted): Fall B answers 200.
func TestMW2DenyListBeatsAFlippedFlag(t *testing.T) {
	pool := mw2Setup(t)
	ctx := context.Background()

	// Flip retrieval.shadow_measurable on the two protected builtins, exactly
	// the way an operator (or a bug) would: an UPDATE on the _global registry
	// row, leaving every other field of the policy untouched.
	for _, name := range []string{"checkpoint", "system-meta"} {
		tag, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = jsonb_set(config, '{retrieval,shadow_measurable}', 'true'::jsonb, true)
			  WHERE name = $1 AND scope = '_global'`, name)
		if err != nil {
			t.Fatalf("flip flag on %s: %v", name, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("flip flag on %s: %d rows affected, want 1", name, tag.RowsAffected())
		}
	}

	// Non-vacuity: the flip must actually have reached the resolved policy. If
	// it had not, the 400s below would prove nothing at all.
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	for _, name := range []string{"checkpoint", "system-meta"} {
		if !reg.Snapshot().IsShadowMeasurable(name) {
			t.Fatalf("fixture is vacuous: %q does not carry shadow_measurable after the flip", name)
		}
	}

	h := mw2Handler(t, pool, bw2NewBackend(t), bw2Config())

	for _, tc := range []struct{ name, body string }{
		// Fall A — deny-list half.
		{"checkpoint with the flag ON", mw2Body(`,"shadow_types":["checkpoint"]`)},
		{"system-meta with the flag ON", mw2Body(`,"shadow_types":["system-meta"]`)},
		{"checkpoint beside a legal shadow type", mw2Body(`,"shadow_types":["` + mw2ShadowType + `","checkpoint"]`)},
		// Fall B — flag half. The deny-list does not know this name, so only
		// the flag check can refuse it.
		{"an excluded type WITHOUT the flag", mw2Body(`,"shadow_types":["` + mw2PlainType + `"]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := bw2Do(t, h, bw2AdminKeyID, true, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}

	// And the legal type still works with the flipped builtins in the registry —
	// the deny-list refuses two names, it does not disable the seam.
	if rec := bw2Do(t, h, bw2AdminKeyID, true, mw2ShadowRequest()); rec.Code != http.StatusOK {
		t.Fatalf("the legal shadow type was refused too: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Probe (h): rows yes, sources no
// ---------------------------------------------------------------------------

// TestMW2ShadowRowsNeverBecomeSources is the happy path and the containment
// claim in one: the shadow block ranks into the arm rows and the fusion order
// (that IS the measurement), and it appears in nothing the surrounding response
// carries — no source id, no title, no category, no content.
//
// RED before M-W2: `arm_ranks.rows does not carry the shadow block` — without
// the seam the type is excluded from p_types_visible and never enters an arm.
func TestMW2ShadowRowsNeverBecomeSources(t *testing.T) {
	pool := mw2Setup(t)
	h := mw2Handler(t, pool, bw2NewBackend(t), bw2Config())

	rec := bw2Do(t, h, bw2AdminKeyID, true, mw2ShadowRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	body := bw2DecodeBody(t, rec)
	block := bw2Block(t, rec)

	rows := mw2IDs(t, block["rows"])
	if !mw2Contains(rows, mw2ShadowBlockID) {
		t.Fatalf("arm_ranks.rows does not carry the shadow block: %v", rows)
	}
	if order := mw2IDs(t, block["fusion_order"]); !mw2Contains(order, mw2ShadowBlockID) {
		t.Errorf("fusion_order does not carry the shadow block: %v", order)
	}

	sources, _ := body["sources"].([]any)
	if len(sources) == 0 {
		t.Fatal("sources is empty — the probe cannot tell filtering from an empty result")
	}
	for i, s := range sources {
		src, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("source %d is %T", i, s)
		}
		if src["id"] == mw2ShadowBlockID {
			t.Errorf("source %d IS the shadow block: %v", i, src)
		}
	}
	// Belt and braces over the whole wire body: the fixture title exists only
	// on shadow blocks, and the arm block projects no titles at all — so any
	// occurrence anywhere in the body is a leak.
	if strings.Contains(rec.Body.String(), mw2ShadowTitle) {
		t.Errorf("response body carries the shadow title: %s", rec.Body.String())
	}

	// Control: the same request WITHOUT the field must not see the block at all.
	rec2 := bw2Do(t, h, bw2AdminKeyID, true, mw2Body(``))
	if rows := mw2IDs(t, bw2Block(t, rec2)["rows"]); mw2Contains(rows, mw2ShadowBlockID) {
		t.Errorf("a plain measurement request sees the shadow block: %v", rows)
	}
}

// ---------------------------------------------------------------------------
// Review finding #1: the embed backfill is a SECOND chain on the same request
// ---------------------------------------------------------------------------

// TestMW2SkipsEmbedBackfill closes the hole the adversarial review found in the
// chain-locality obligation.
//
// shadowChainLocality checks Chain(role, querySens, ar.HomeScope). Step 3d runs
// backfillPending on the SAME request, which resolves Chain(RoleEmbed,
// floor.Apply(blockSensitivity, blockScope), blockScope) — both keys differ from
// the ones the gate checked, and both differences can WIDEN the chain: a lower
// required sensitivity lets Trust.Allows admit backends the gate never saw, and
// a foreign block scope changes the VisibleTo set entirely. Over that chain goes
// title + content of a block (query.go, embedText).
//
// The fix is to skip the backfill on the shadow path rather than to re-check it
// there: a write in the middle of a read-only measurement contradicts the B-W2
// determinism doctrine anyway, and an embedding written during a measurement
// changes the corpus the measurement is measuring.
//
// RED before the fix: the block gets an embedding from the shadow request
// (`the shadow request embedded a pending block`), and the fake backend counts
// two embed calls instead of one.
func TestMW2SkipsEmbedBackfill(t *testing.T) {
	pool := mw2Setup(t)
	b := bw2NewBackend(t)
	h := mw2Handler(t, pool, b, bw2Config())

	const pendingID = "019fa500-0000-7000-9000-0000000003b1"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, type_name)
		 VALUES ($1::uuid, 'knowledge', 'pending block', 'pending body', $2, 'knowledge')`,
		pendingID, bw2Scope,
	); err != nil {
		t.Fatalf("insert pending block: %v", err)
	}

	// 1) The shadow request must not touch it.
	if rec := bw2Do(t, h, bw2AdminKeyID, true, mw2ShadowRequest()); rec.Code != http.StatusOK {
		t.Fatalf("shadow status %d, body %s", rec.Code, rec.Body.String())
	}
	if mw2HasEmbedding(t, pool, pendingID) {
		t.Error("the shadow request embedded a pending block — a second embed chain ran unchecked")
	}
	if got := b.embedHits.Load(); got != 1 {
		t.Errorf("shadow request made %d embed-wire calls, want 1 (query embed only)", got)
	}

	// 2) Non-regression: the SAME request without the field still backfills.
	//    Same block, so this cannot pass by measuring a different fixture.
	if rec := bw2Do(t, h, bw2AdminKeyID, true, mw2Body(``)); rec.Code != http.StatusOK {
		t.Fatalf("plain status %d, body %s", rec.Code, rec.Body.String())
	}
	if !mw2HasEmbedding(t, pool, pendingID) {
		t.Error("an ordinary measurement request no longer backfills — the skip is too wide")
	}
}

func mw2HasEmbedding(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(context.Background(),
		`SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1::uuid`, id).Scan(&present); err != nil {
		t.Fatalf("read embedding state: %v", err)
	}
	return present
}

// ---------------------------------------------------------------------------
// Probe (k): scope
// ---------------------------------------------------------------------------

// TestMW2ShadowRespectsScope pins that the seam widens the TYPE allowlist and
// nothing else: an identical shadow block in a foreign scope stays invisible.
func TestMW2ShadowRespectsScope(t *testing.T) {
	pool := mw2Setup(t)
	h := mw2Handler(t, pool, bw2NewBackend(t), bw2Config())

	rec := bw2Do(t, h, bw2AdminKeyID, true, mw2ShadowRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	rows := mw2IDs(t, bw2Block(t, rec)["rows"])
	if mw2Contains(rows, mw2ForeignBlockID) {
		t.Errorf("a shadow block from a foreign scope reached arm_ranks.rows: %v", rows)
	}
	if !mw2Contains(rows, mw2ShadowBlockID) {
		t.Fatalf("the in-scope shadow block is missing — the probe would pass vacuously: %v", rows)
	}
}

// ---------------------------------------------------------------------------
// Probe (f): the forced rerank shutdown (G6)
// ---------------------------------------------------------------------------

// mw2RerankConfig arms the reranker. With no backend carrying the rerank role
// the dispatch takes the LLM-as-judge branch — a chat-wire call over the
// synthesis chain that builds its prompt from BLOCK CONTENT (rrf/rerank.go),
// which is exactly the egress a shadow request must not pay.
func mw2RerankConfig() *config.Config {
	c := bw2Config()
	c.Rerank = config.RerankConfig{Enabled: true, MaxDocs: 50, BlendWeight: 1.0}
	return c
}

// TestMW2ForcesRerankOff pins G6: the handler switches the reranker off for a
// shadow request instead of trusting rerank.enabled — a hot, tenant-overridable
// value that a security invariant may not hang on.
//
// The instrument is twofold: the chat backend's hit counter (synchronous, so no
// polling race) and the context_llm_log row the judge writes (the design's own
// wording: "no query-rerank-judge line").
//
// RED before M-W2: the control run below is the only behaviour that exists — a
// shadow request is an ordinary request and pays the judge call.
func TestMW2ForcesRerankOff(t *testing.T) {
	pool := mw2Setup(t)

	// Control first, on its OWN backend, so the counters cannot be confused:
	// with the reranker armed and no shadow_types the judge MUST run. Without
	// it the probe below could pass because the instrument is broken.
	ctrlBackend := bw2NewBackend(t)
	ctrl := mw2Handler(t, pool, ctrlBackend, mw2RerankConfig())
	if rec := bw2Do(t, ctrl, bw2AdminKeyID, true, mw2Body(``)); rec.Code != http.StatusOK {
		t.Fatalf("control status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := ctrlBackend.chatHits.Load(); got == 0 {
		t.Fatal("control: the rerank judge never ran — the probe below would be vacuous")
	}
	if n := mw2WaitJudgeRows(t, pool, 1); n == 0 {
		t.Fatal("control: no query-rerank-judge row in context_llm_log — instrument broken")
	}
	before := mw2JudgeRows(t, pool)

	shadowBackend := bw2NewBackend(t)
	sh := mw2HandlerWithPool(t, pool, mw2RerankConfig(), mw2Pool(shadowBackend.srv.URL))
	if rec := bw2Do(t, sh, bw2AdminKeyID, true, mw2ShadowRequest()); rec.Code != http.StatusOK {
		t.Fatalf("shadow status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := shadowBackend.chatHits.Load(); got != 0 {
		t.Errorf("shadow request made %d chat-wire calls, want 0 (rerank was not forced off)", got)
	}
	// The llmlog insert is asynchronous: give it the window the control row
	// needed, then assert the count did NOT move.
	time.Sleep(500 * time.Millisecond)
	if after := mw2JudgeRows(t, pool); after != before {
		t.Errorf("query-rerank-judge rows went %d → %d for a shadow request", before, after)
	}
}

func mw2JudgeRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_llm_log WHERE pipeline = 'query-rerank-judge'`).Scan(&n); err != nil {
		t.Fatalf("count llm log: %v", err)
	}
	return n
}

// mw2WaitJudgeRows polls for the asynchronous llmlog insert.
func mw2WaitJudgeRows(t *testing.T, pool *pgxpool.Pool, want int) int {
	t.Helper()
	for i := 0; i < 40; i++ {
		if n := mw2JudgeRows(t, pool); n >= want {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	return mw2JudgeRows(t, pool)
}

// ---------------------------------------------------------------------------
// The chain-locality obligation that follows from G6
// ---------------------------------------------------------------------------

// TestMW2RefusesNonLANChain pins the obligation of §4.2: every role a shadow
// request can still resolve a chain for must be locality=lan. The live pool
// carries openrouter with locality=external, trust=no-credentials, enabled — so
// "no LLM runs anyway" is not a property of the request, it is a property of
// the configuration, and this gate makes it the former.
//
// The refusal is fail-fast (4xx), never 5xx: the sweep driver treats 5xx as
// retryable and would silently exclude the case after its retries.
func TestMW2RefusesNonLANChain(t *testing.T) {
	pool := mw2Setup(t)
	b := bw2NewBackend(t)

	for _, tc := range []struct {
		locality string
		want     int
	}{
		{backends.LocalityLAN, http.StatusOK},
		{backends.LocalityExternal, http.StatusBadRequest},
		{backends.LocalityLocal, http.StatusBadRequest},
		{"", http.StatusBadRequest},
	} {
		t.Run("locality="+tc.locality, func(t *testing.T) {
			bpool := backends.NewPool(nil, nil)
			bpool.SeedSnapshotForTest(mw2Backends(b.srv.URL, tc.locality))
			h := mw2HandlerWithPool(t, pool, bw2Config(), bpool)
			rec := bw2Do(t, h, bw2AdminKeyID, true, mw2ShadowRequest())
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK && strings.Contains(rec.Body.String(), "mw2-embed") {
				t.Errorf("the refusal names a backend — topology disclosure: %s", rec.Body.String())
			}
		})
	}

	// A request WITHOUT shadow_types is not gated on locality at all — the
	// obligation belongs to the shadow path, not to the production path.
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest(mw2Backends(b.srv.URL, backends.LocalityExternal))
	h := mw2HandlerWithPool(t, pool, bw2Config(), bpool)
	if rec := bw2Do(t, h, bw2AdminKeyID, true, mw2Body(``)); rec.Code != http.StatusOK {
		t.Fatalf("a plain measurement was refused on locality: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Probe (i): the response without the field is byte-identical
// ---------------------------------------------------------------------------

// mw2PreWaveMeasurementSHA is the sha256 of the FIRST arm_ranks measurement
// response over this fixture, captured against the UNCHANGED tree (M-W1 tip
// ad0a6244) with the two registry rows carrying mw2PlainConfig — a state in
// which the shadow type is simply not registered, which is visibility-identical
// to "registered but excluded". The seam must not move that body by one byte.
//
// Reproduce: run the test, read the digest it logs on mismatch.
const mw2PreWaveMeasurementSHA = "3e4e8e4f9aca1bc29d0f020402e9899029556f300361908b2427fe285ae74e8b"

// TestMW2ByteIdenticalWithoutField is non-regression gate (i). The request is
// the FIRST one this test makes, so the embed cache is cold exactly as it was
// when the anchor was taken (embed_cache_hit is part of the body).
func TestMW2ByteIdenticalWithoutField(t *testing.T) {
	pool := mw2Setup(t)
	h := mw2Handler(t, pool, bw2NewBackend(t), bw2Config())

	rec := bw2Do(t, h, bw2AdminKeyID, true, mw2Body(``))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := mw2Digest(rec.Body.String()); got != mw2PreWaveMeasurementSHA {
		t.Errorf("measurement response sha256 = %s, want %s (pre-wave anchor)\nbody: %s",
			got, mw2PreWaveMeasurementSHA, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Probe (j), allocation half: the measurement slice is a copy
// ---------------------------------------------------------------------------

// TestMW2MeasureSliceIsACopy pins the ALLOCATION half of §4.2's two-slice rule:
// the measurement slice is a clone plus the shadow names, so nothing holding
// the visible slice can observe the widening — not even through spare capacity
// in the backing array, which is the failure mode a plain append hits only
// SOMETIMES and is therefore the one that survives a review.
//
// The wiring half (which variable reaches which consumer) is pinned statically
// in query_shadow_wiring_test.go; a local slice cannot be observed at runtime.
//
// RED before M-W2: `undefined: measureVisibleTypesFor`.
func TestMW2MeasureSliceIsACopy(t *testing.T) {
	// Spare capacity on purpose: with cap > len a naive append writes INTO the
	// caller's array and the mutation is invisible to len().
	visible := make([]string, 2, 8)
	visible[0], visible[1] = "knowledge", "reference"
	spy := visible[:cap(visible)]

	got := measureVisibleTypesFor(visible, []string{mw2ShadowType})
	if len(got) != 3 || got[2] != mw2ShadowType {
		t.Fatalf("measure slice = %v, want the two visible types plus the shadow type", got)
	}
	if len(visible) != 2 || visible[0] != "knowledge" || visible[1] != "reference" {
		t.Errorf("the visible slice was modified: %v", visible)
	}
	if spy[2] != "" {
		t.Errorf("the widening was written into the visible slice's backing array: %q", spy[2])
	}

	// Without shadow types it must be the SAME slice, not a copy: that is what
	// makes the no-flag path allocation-identical to production.
	same := measureVisibleTypesFor(visible, nil)
	if len(same) != len(visible) || &same[0] != &visible[0] {
		t.Error("without shadow types the measure slice is not the visible slice itself")
	}
}

// TestMW2MeasureSliceDeduplicates is review finding #7 on the slice side: a
// repeated name must reach p_types_visible ONCE. `= ANY(array)` does not care,
// but the array is bound into two statements per request and its length is
// caller-controlled — so the request list is normalised before it becomes SQL.
// A name already in the visible slice is dropped too: the widening is a set
// union, not a concatenation.
//
// RED before the fix: `measure slice = [knowledge reference mw2-shadow
// mw2-shadow knowledge]`.
func TestMW2MeasureSliceDeduplicates(t *testing.T) {
	visible := []string{"knowledge", "reference"}
	got := measureVisibleTypesFor(visible, []string{mw2ShadowType, mw2ShadowType, "knowledge"})

	want := []string{"knowledge", "reference", mw2ShadowType}
	if len(got) != len(want) {
		t.Fatalf("measure slice = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("measure slice = %v, want %v", got, want)
		}
	}
}
