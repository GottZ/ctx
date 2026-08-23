//go:build integration

// Cap-hit booking pin (issue #26), DB-visible half: the unit tests in
// evaluate_test.go pin what evaluateRelationships RETURNS after two
// truncations at the output cap (ErrOutputCapHit, exactly two wire calls);
// this file pins what RunDreamCycle LEAVES BEHIND in Postgres when that
// sentinel reaches it.
//
// The distinction is the whole point of the wave. Every other eval failure
// takes SetDreamCooldownMinutes — the 5-minute transient park that does NOT
// advance dream_eval_count, because a GPU restart says nothing about the
// block. A prompt that overruns twice the cap says something about the block:
// it will overrun again unchanged, and the transient path has no attempt
// counter, so the cycle would re-burn one eval every five minutes forever.
// The cap-hit branch therefore books a COMPLETED-but-inert eval instead —
// SetDreamCooldown(inert=true), the same booking a "nothing relates" verdict
// gets. Three DB facts separate the two bookings, and this test asserts all
// three on one real cycle.
package dream_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	crSrcID  = "019d0000-0000-7000-9000-000000001a01"
	crCandID = "019d0000-0000-7000-9000-000000001a02"

	// Own scope so the pick, the RRF arm and the same-scope candidate filter
	// all see exactly these two blocks (context_blocks.scope is VARCHAR(20)).
	crScope = "capretry-it"

	// The ONE keyword the cycle searches with. Pre-seeded on the block so no
	// keyword LLM call is needed, and pre-seeded in context_embed_cache so no
	// embed WIRE call is needed either (see seedKeywordEmbed).
	crKeyword = "capretry"

	crBackendOrigin = "http://gpu-capretry-it:8091"
)

// crTruncated is a real-shaped dream-eval answer cut off mid-JSON: the
// object-map drift form with the second key started but never closed — the
// exact shape evaluate_test.go's truncatedObjectMap carries, re-stated here
// because that constant lives in package dream and this file is the external
// test package. Written against the REAL candidate id so the fixture stays
// honest; parseLinks never gets far enough to look at it.
const crTruncated = `{"` + crCandID + `":{"target_id":"` + crCandID + `","type":"topical","confidence":0.9}, "` + crCandID

// crEvalMarker is the first line of dream's link-evaluation system prompt
// (dreamSystemPrompt). The chatJSON seam is ONE process-wide hook for all five
// dream chat pipelines, so a cycle-level test has to tell them apart — the
// preempt pins do it by target-id in the USER prompt, this one by the SYSTEM
// prompt, because the discriminator has to hold for a call whose user prompt
// is not yet known. The three other prompts a cycle can reach open with
// "You are a temporal reference extractor" (dream-temporal), "You extract
// conceptual keywords" (dream-keywords) and "Classify whether two knowledge
// blocks form a recurring pattern" (dream-recurrence); none contains this
// substring.
const crEvalMarker = "source block and candidate blocks"

// crBenign is what every NON-eval dream call gets: a well-formed empty JSON
// object, which the temporal review (the one such call this path actually
// makes, step 1b) unmarshals into an empty TemporalReview — no findings, no
// warning. Each of those pipelines is non-fatal inside RunDreamCycle, so junk
// would not break the test; it would only put an unrelated parse failure in
// the log next to the one the assertion is about.
const crBenign = `{}`

// crEmbedDim is the context_blocks.embedding / context_embed_cache.embedding
// width (vector(1024)).
const crEmbedDim = 1024

// crVec is the single vector every fixture uses — block embeddings AND the
// cached keyword embedding. Identical vectors put the candidate at cosine
// similarity 1.0 in the RRF vector arm, so the candidate set does not depend
// on the fixture's prose.
func crVec() pgvec.Vector {
	v := make([]float32, crEmbedDim)
	for i := range v {
		v[i] = 0.02
	}
	return pgvec.NewVector(v)
}

// crBackend is one seeded backend row serving BOTH roles the cycle walks:
// dream (eval, temporal, recurrence) and embed (the keyword embed, which the
// cache short-circuits — the row still has to EXIST or EmbedChain fails the
// cycle before the eval). TrustFull because the fixture blocks would fold to
// the credentials class if their sensitivity were ever left at the
// fail-closed column default.
func crBackend() backends.Backend {
	return backends.Backend{
		ID: "capretry-it-gpu", Name: "capretry-it-gpu", Host: crBackendOrigin,
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream, backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m-capretry-it"}},
		Priority: 100, Enabled: true,
	}
}

// armCapRetryBlock turns an inserted fixture row into a block PickBlock will
// actually claim, and one whose cycle reaches the eval without any wire call
// but the scripted ones:
//
//   - embedding NOT NULL — a hard conjunct of the pick predicate;
//   - dream_keywords pre-seeded — GenerateKeywords is skipped entirely, so
//     the keyword pipeline cannot interfere with the eval-call count;
//   - dream_cooldown_until NULL — eligible right now;
//   - sensitivity 'internal' — the column default is 'credentials'
//     (fail-closed), and the eval resolves its chain at the MAX over source
//     and candidates, so leaving the default would make the whole assertion
//     depend on the trust gate instead of on the cap.
//
// lifecycle_state ('knowledge') and type_name ('knowledge') stay at their
// column defaults: both are inside the pick allowlist of a booted registry.
func armCapRetryBlock(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET
			embedding = $2,
			dream_keywords = $3::text[],
			dream_cooldown_until = NULL,
			sensitivity = 'internal'
		 WHERE id = $1::uuid`,
		id, crVec(), []string{crKeyword}); err != nil {
		t.Fatalf("arm block %s: %v", id, err)
	}
}

// seedKeywordEmbed writes the keyword's embedding into context_embed_cache
// under the production key (embedcache.HashKey(prefix, text), model), so
// embedcache.EmbedChain takes its cache fast-path and contacts no backend.
// The alternative — a second seam for the embed wire — does not exist in this
// package, and a cycle that fails to embed its keyword never reaches the eval
// at all (searchByKeywords error ⇒ transient cooldown), which is precisely
// the booking this test must not see by accident.
func seedKeywordEmbed(t *testing.T, pool *pgxpool.Pool, b backends.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
		 VALUES ($1, $2, $3, $4)`,
		embedcache.HashKey(embed.PrefixQuery, crKeyword),
		b.ModelFor(backends.RoleEmbed).Model,
		crVec(), crKeyword); err != nil {
		t.Fatalf("seed keyword embed cache: %v", err)
	}
}

// TestRunDreamCycle_DoubleCapHit_BooksInertBackoff_DB is spec test 7: a full
// RunDreamCycle whose eval is truncated at the output cap on BOTH the regular
// attempt and the bounded retry books the block as a completed-but-inert eval
// — not as a transient failure.
//
// Asserted on the real row after the cycle:
//
//   - the returned error carries ErrOutputCapHit (the signal the branch keys
//     on; without it the cycle would fall through to the transient path);
//   - dream_eval_count advanced by exactly 1 — SetDreamCooldown's advance,
//     which SetDreamCooldownMinutes does NOT do (pinned from the other side in
//     preempt_semantics_integration_test.go, P-N6);
//   - dream_last_inert is true — the inert branch, i.e. the InertOffset was
//     applied to the curve;
//   - dream_cooldown_until is FAR out, not the 5-minute transient stamp.
//
// The 1-hour threshold on the last one is chosen from the BackoffConfig this
// test passes (the production defaults: exp / factor 1.6 / grace 0 / min 12h /
// cap 1080h / inert-offset 7). The block enters at dream_eval_count = 0, so
// SetDreamCooldown's exponent is (0+1) - 0 + 7 = 8 and the cooldown is
// 12h * 1.6^8 ≈ 515h ≈ 21 days — three orders of magnitude clear of the
// threshold, while both wrong bookings sit far BELOW it: the transient stamp
// is 5 minutes, and PickBlock's own claim TTL (CycleTimeout 700s + 1 min
// grace ≈ 12 min) is what the row would still carry if the branch wrote
// nothing at all. One threshold, three outcomes separated.
func TestRunDreamCycle_DoubleCapHit_BooksInertBackoff_DB(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insertBlock(t, pool, crSrcID, crScope, "decisions", "Cap retry source block", tEarly, tEarly)
	insertBlock(t, pool, crCandID, crScope, "projects", "Cap retry candidate block", tEarly, tEarly)
	armCapRetryBlock(t, pool, crSrcID)
	armCapRetryBlock(t, pool, crCandID)

	backend := crBackend()
	seedKeywordEmbed(t, pool, backend)

	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{backend})

	// The registry is booted from the test DB (seed rows), the production
	// resolution path — the pick allowlist and the candidate sieve both read
	// this snapshot, never a compiled-in list.
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	// Empty dispatch policy = pass-through admission (the shape every dream
	// test router binds); CapRetryFactor 2 is what arms the retry at all.
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	r := &dream.Router{
		Pool:           p,
		Blocktypes:     reg,
		Admit:          llm.Admission{Admitter: d, Class: dispatch.ClassBackground},
		CapRetryFactor: 2,
	}

	// The seam: eval calls truncate at the cap and SAY SO (finish_reason
	// "length" is the provider-stated signal capHit ranks first); every other
	// dream pipeline answers benignly. evalCaps records the RESOLVED output
	// cap of each eval attempt — resolved, because ChatChainVia hands
	// applyModelParams' output to the seam, which is where NumPredictScale
	// lands.
	var evalCalls atomic.Int32
	var evalCaps []int
	swapChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, systemPrompt, _ string, opts llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		if !strings.Contains(systemPrompt, crEvalMarker) {
			return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: crBenign}}, nil
		}
		evalCalls.Add(1)
		evalCaps = append(evalCaps, opts.NumPredict)
		return &llm.ChatResponse{
			Message:      llm.Message{Role: "assistant", Content: crTruncated},
			EvalCount:    opts.NumPredict,
			PromptTokens: 400,
			FinishReason: "length",
		}, nil
	})

	// Production back-off defaults — the numbers the 1-hour threshold above is
	// derived from.
	backoff := dream.BackoffConfig{Mode: "exp", Factor: 1.6, Grace: 0, MinHours: 12, CapHours: 1080, InertOffset: 7}

	written, err := dream.RunDreamCycle(ctx, pool, r, dream.DreamOptions(), backoff,
		[]string{crScope}, dream.NoThrottle)
	if err == nil {
		t.Fatal("a twice-truncated eval must fail the cycle, got nil error")
	}
	if !errors.Is(err, dream.ErrOutputCapHit) {
		t.Fatalf("errors.Is(err, ErrOutputCapHit) = false, err = %v", err)
	}
	if written != 0 {
		t.Fatalf("cycle wrote %d link(s), want 0 — the cycle returns before WriteLinks", written)
	}
	if n := evalCalls.Load(); n != 2 {
		t.Fatalf("dream-eval wire calls = %d, want exactly 2 (one attempt + ONE bounded retry)", n)
	}
	// The retry is only a retry if it actually widened the cap: base 600
	// (DreamOptions) then 600*2. A second call at 600 would mean the factor
	// never reached the chain walk.
	if len(evalCaps) != 2 || evalCaps[0] != dream.DefaultNumPredict || evalCaps[1] != 2*dream.DefaultNumPredict {
		t.Fatalf("eval output caps = %v, want [%d %d]", evalCaps, dream.DefaultNumPredict, 2*dream.DefaultNumPredict)
	}
	if n := countLinks(t, pool, crSrcID); n != 0 {
		t.Fatalf("cap-hit cycle persisted %d dream link(s), want 0", n)
	}

	var evalCount int
	var lastInert bool
	var cooldownSecs float64
	if err := pool.QueryRow(ctx,
		`SELECT dream_eval_count, dream_last_inert,
		        EXTRACT(EPOCH FROM (dream_cooldown_until - now()))
		 FROM context_blocks WHERE id = $1::uuid`, crSrcID).
		Scan(&evalCount, &lastInert, &cooldownSecs); err != nil {
		t.Fatalf("read block state after cap-hit cycle: %v", err)
	}
	// Entered at 0 (fresh fixture), so exactly 1 is "advanced by one".
	if evalCount != 1 {
		t.Errorf("dream_eval_count = %d, want 1 — the cap-hit branch books a COMPLETED eval (SetDreamCooldownMinutes would leave it at 0)", evalCount)
	}
	if !lastInert {
		t.Errorf("dream_last_inert = false, want true — a twice-capped eval produced no links and must be booked inert")
	}
	if cooldownSecs <= 3600 {
		t.Errorf("dream_cooldown_until is %.0fs out, want > 3600s — 5 min is the transient stamp, ~12 min the unreplaced PickBlock claim, ~515h the inert back-off this branch owes", cooldownSecs)
	}
}
