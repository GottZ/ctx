//go:build integration

// BA15 (design/02 §5.1, Vor-Welle V-9): the dream candidate search must not
// spend its RRF slots on types the loop can never link.
//
// The defect this file pins: searchByKeywords bound ctx_rrf's p_types_visible
// to typeSet.VisibleTypes() and passed nil/nil for p_damped_types /
// p_damped_factors. A `damped` type is IN VisibleTypes (blocktype/set.go
// RetrievalDamped → s.visible), so it competed for the MaxCandidatesPerKeyword
// slots at factor 1.0 — the whole damping apparatus was inert on this path —
// and the post-RRF sieve then threw every one of those hits away again because
// the type is dream.linkable=false. The slots were gone either way.
//
// The fixture reproduces the post-E-4 stance the design plans for `insight`
// (retrieval excluded → damped) inside the test container's registry. Live and
// root keep `insight` at `excluded`; nothing here touches a live row.
package dream_test

import (
	"context"
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
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	// Own scope so the pick, all four RRF arms and the same-scope candidate
	// filter see exactly this fixture (context_blocks.scope is VARCHAR(20)).
	baScope = "ba15-it"

	// The ONE keyword the cycle searches with. Deliberately a nonsense token:
	// it appears in no title and no content, so websearch_to_tsquery matches
	// nothing in either full-text arm and similarity(title, p_query) stays
	// below the trigram arm's 0.05 floor. Only the semantic arm ranks, and the
	// fixture's cosine distances therefore decide the whole result order.
	baKeyword = "zqvvkw"

	baBackendOrigin = "http://gpu-ba15-it:8091"

	baSourceID = "019d0000-0000-7000-9000-0000ba150001"

	// crEvalMarker's counterpart — the first line of dreamSystemPrompt. The
	// chatJSON seam is ONE process-wide hook for all five dream chat
	// pipelines; this substring separates the link evaluation from the
	// temporal review and the recurrence classifier.
	baEvalMarker = "source block and candidate blocks"

	baEmbedDim = 1024
)

// baKnowledgeIDs are the five dream-linkable candidates the cycle is SUPPOSED
// to evaluate; baInsightIDs are five blocks of a damped, non-linkable type
// that outrank them in the semantic arm.
var (
	baKnowledgeIDs = []string{
		"019d0000-0000-7000-9000-0000ba150011",
		"019d0000-0000-7000-9000-0000ba150012",
		"019d0000-0000-7000-9000-0000ba150013",
		"019d0000-0000-7000-9000-0000ba150014",
		"019d0000-0000-7000-9000-0000ba150015",
	}
	baInsightIDs = []string{
		"019d0000-0000-7000-9000-0000ba150021",
		"019d0000-0000-7000-9000-0000ba150022",
		"019d0000-0000-7000-9000-0000ba150023",
		"019d0000-0000-7000-9000-0000ba150024",
		"019d0000-0000-7000-9000-0000ba150025",
	}
)

// baVec builds a 1024-wide vector from a two-segment description: `head`
// components carry `hi`, the rest carries `lo`. Cosine against baQueryVec
// falls monotonically as `lo` drops, which is the only ordering lever the
// fixture needs.
func baVec(head int, hi, lo float32) pgvec.Vector {
	v := make([]float32, baEmbedDim)
	for i := range v {
		if i < head {
			v[i] = hi
		} else {
			v[i] = lo
		}
	}
	return pgvec.NewVector(v)
}

// baQueryVec is the cached keyword embedding AND the insight blocks' embedding
// — cosine 1.0, semantic ranks 1..5.
func baQueryVec() pgvec.Vector { return baVec(baEmbedDim, 0.02, 0.02) }

// baKnowledgeVec: cosine ≈ 0.949 against the query — semantic ranks 6..10,
// strictly behind every insight block and strictly ahead of the source.
func baKnowledgeVec() pgvec.Vector { return baVec(512, 0.02, 0.01) }

// baSourceVec: cosine ≈ 0.473 — last of the eleven. The source is excluded
// from the candidate set by ID anyway (`seen`), but it still occupies an RRF
// slot, so it has to rank behind everything the assertions count.
func baSourceVec() pgvec.Vector { return baVec(128, 0.02, 0.001) }

func baBackend() backends.Backend {
	return backends.Backend{
		ID: "ba15-it-gpu", Name: "ba15-it-gpu", Host: baBackendOrigin,
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream, backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m-ba15-it"}},
		Priority: 100, Enabled: true,
	}
}

// baArmBlock gives a fixture row the columns the cycle reads: an embedding
// (hard conjunct of the pick predicate and the only RRF arm in play), a
// sensitivity that is not the fail-closed `credentials` column default (the
// eval chain resolves at the max over source and candidates), and either an
// open cooldown (the ONE pickable block) or a cooldown a day out (everything
// else, so PickBlock's choice is deterministic).
func baArmBlock(t *testing.T, pool *pgxpool.Pool, id string, vec pgvec.Vector, pickable bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cooldown := "now() + interval '1 day'"
	keywords := []string{}
	if pickable {
		cooldown = "NULL"
		keywords = []string{baKeyword}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET
			embedding = $2,
			dream_keywords = $3::text[],
			dream_cooldown_until = `+cooldown+`,
			sensitivity = 'internal'
		 WHERE id = $1::uuid`,
		id, vec, keywords); err != nil {
		t.Fatalf("arm block %s: %v", id, err)
	}
}

// baSeedKeywordEmbed puts the keyword's vector into context_embed_cache under
// the production key so embedcache.EmbedChain takes its cache fast path and
// the cycle contacts no backend for the embed.
func baSeedKeywordEmbed(t *testing.T, pool *pgxpool.Pool, b backends.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
		 VALUES ($1, $2, $3, $4)`,
		embedcache.HashKey(embed.PrefixQuery, baKeyword),
		b.ModelFor(backends.RoleEmbed).Model,
		baQueryVec(), baKeyword); err != nil {
		t.Fatalf("seed keyword embed cache: %v", err)
	}
}

// baDampInsight moves the `insight` registry row to the stance E-4 plans:
// retrieval `damped` instead of `excluded`, factor 0.6. dream.linkable stays
// false — that combination is the whole point of BA15. The row is the M143
// `_global` seed of THIS test database; live and root are untouched.
func baDampInsight(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, err := pool.Exec(ctx,
		`UPDATE context_block_types
		    SET config = jsonb_set(
		          jsonb_set(config, '{retrieval,policy}', '"damped"'::jsonb, true),
		          '{retrieval,damping_factor}', '0.6'::jsonb, true)
		  WHERE name = 'insight' AND scope = '_global'`)
	if err != nil {
		t.Fatalf("damp insight type: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("insight _global registry row: %d rows updated, want 1", tag.RowsAffected())
	}
}

func baContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// baEvalBlockIDs polls context_llm_log for the dream-eval row BA15 names as
// its artifact and returns its block_ids. llmlog records asynchronously, hence
// the bounded wait; found==false means no eval row ever landed.
func baEvalBlockIDs(t *testing.T, pool *pgxpool.Pool) ([]string, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var ids []string
		err := pool.QueryRow(context.Background(),
			`SELECT block_ids::text[] FROM context_llm_log
			  WHERE pipeline = 'dream-eval' ORDER BY created_at LIMIT 1`).Scan(&ids)
		if err == nil {
			return ids, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDreamCandidateSearch_DampedNonLinkableType_NeverTakesCandidateSlot_BehaviourMatchesContract
// is the V-9 gate. Eleven blocks share one scope: one source, five
// dream-linkable `knowledge` candidates, five `insight` blocks whose type is
// damped-but-not-linkable and whose embedding is cosine 1.0 to the keyword.
// MaxCandidatesPerKeyword is 5 and the cycle runs one keyword, so the five
// slots are exactly the contested resource.
//
// RED against the pre-V-9 tree (allowlist = VisibleTypes, damping arrays nil):
// the five insight blocks take all five slots at factor 1.0, the post-RRF
// sieve drops every one of them, the cycle books "no candidates found" and
// NEVER reaches the eval — no dream-eval row, no evaluated knowledge block.
//
// GREEN: the candidate allowlist is intersect(VisibleTypes,
// DreamLinkableTypes), so no insight block is ever retrieved, the five
// knowledge blocks fill the slots, and the dream-eval row's block_ids carry
// the source plus exactly those five and no insight id.
func TestDreamCandidateSearch_DampedNonLinkableType_NeverTakesCandidateSlot_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	baSeedCorpus(t, pool, tEarly)

	backend := baBackend()
	baSeedKeywordEmbed(t, pool, backend)

	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{backend})

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	set := reg.Snapshot()

	// Precondition, not an assertion about the fix: without these three the
	// test could go green for the wrong reason (a type that never was
	// retrievable, or one the loop would have linked anyway).
	if !baContains(set.VisibleTypes(), "insight") {
		t.Fatalf("fixture precondition: insight must be retrieval-visible, VisibleTypes=%v", set.VisibleTypes())
	}
	if baContains(set.DreamLinkableTypes(), "insight") {
		t.Fatalf("fixture precondition: insight must stay dream.linkable=false, linkable=%v", set.DreamLinkableTypes())
	}
	dampedNames, dampedFactors := set.DampedTypesFor("")
	if !baContains(dampedNames, "insight") {
		t.Fatalf("fixture precondition: insight must be damped, damped=%v/%v", dampedNames, dampedFactors)
	}

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	r := &dream.Router{
		Pool:       p,
		Blocktypes: reg,
		Admit:      llm.Admission{Admitter: d, Class: dispatch.ClassBackground},
	}

	// The seam records every link-evaluation user prompt; the other dream
	// pipelines (temporal review, recurrence) get a benign empty object.
	var evalCalls atomic.Int32
	var evalUser string
	swapChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, systemPrompt, userPrompt string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		if !strings.Contains(systemPrompt, baEvalMarker) {
			return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: `{}`}}, nil
		}
		evalCalls.Add(1)
		evalUser = userPrompt
		// Well-formed empty array = inert verdict, no links written: the
		// assertion is about WHICH blocks reached the prompt, not about the
		// verdict.
		return &llm.ChatResponse{
			Message:      llm.Message{Role: "assistant", Content: `[]`},
			EvalCount:    8,
			PromptTokens: 400,
		}, nil
	})

	backoff := dream.BackoffConfig{Mode: "exp", Factor: 1.6, Grace: 0, MinHours: 12, CapHours: 1080, InertOffset: 7}
	if _, err := dream.RunDreamCycle(ctx, pool, r, dream.DreamOptions(), backoff,
		[]string{baScope}, dream.NoThrottle); err != nil {
		t.Fatalf("RunDreamCycle: %v", err)
	}

	// (1) The cycle must have reached the evaluation at all. This is the half
	// that is RED pre-V-9: five discarded insight hits leave zero candidates.
	if got := evalCalls.Load(); got != 1 {
		t.Fatalf("dream-eval calls = %d, want 1 (zero means the insight blocks took every candidate slot)", got)
	}

	// (2) Every dream-linkable candidate reached the prompt, no insight block did.
	for _, id := range baKnowledgeIDs {
		if !strings.Contains(evalUser, id) {
			t.Errorf("knowledge candidate %s missing from the eval prompt", id)
		}
	}
	for _, id := range baInsightIDs {
		if strings.Contains(evalUser, id) {
			t.Errorf("insight block %s reached the eval prompt", id)
		}
	}

	// (3) BA15's own artifact: block_ids of the dream-eval row in
	// context_llm_log — source plus exactly the five knowledge candidates.
	ids, ok := baEvalBlockIDs(t, pool)
	if !ok {
		t.Fatal("no dream-eval row in context_llm_log")
	}
	if len(ids) != 1+len(baKnowledgeIDs) {
		t.Errorf("dream-eval block_ids = %d entries %v, want %d", len(ids), ids, 1+len(baKnowledgeIDs))
	}
	if !baContains(ids, baSourceID) {
		t.Errorf("dream-eval block_ids %v missing the source block", ids)
	}
	for _, id := range baInsightIDs {
		if baContains(ids, id) {
			t.Errorf("dream-eval block_ids carry insight block %s", id)
		}
	}
	for _, id := range baKnowledgeIDs {
		if !baContains(ids, id) {
			t.Errorf("dream-eval block_ids %v missing knowledge candidate %s", ids, id)
		}
	}
}

// TestRRFAllowlist_VisibleVsDreamLinkable_BehaviourMatchesContract is the
// DIAGNOSIS behind the gate above, and it is green on both sides of V-9: it
// asserts nothing about dream, only what ctx_rrf returns for the two candidate
// allowlists over the identical fixture. Without it, the gate's red state
// ("dream-eval calls = 0") would be compatible with a fixture that retrieves
// nothing at all. With it, the cause is pinned: under VisibleTypes the five
// slots go to five insight blocks — at factor 1.0, because the pre-V-9 call
// site passed nil damping arrays — and under intersect(VisibleTypes,
// DreamLinkableTypes) the same five slots go to the five knowledge blocks.
func TestRRFAllowlist_VisibleVsDreamLinkable_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	baSeedCorpus(t, pool, tEarly)

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	set := reg.Snapshot()

	qvec := baQueryVec().Slice()

	// (a) The pre-V-9 allowlist, with the pre-V-9 nil damping arrays.
	got, _, err := rrf.Search(ctx, pool, qvec, baKeyword, baKeyword, []string{baScope}, nil, nil,
		dream.MaxCandidatesPerKeyword, "", "", set.VisibleTypes(), nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search over VisibleTypes: %v", err)
	}
	insightHits := 0
	for _, res := range got {
		if res.TypeName == "insight" {
			insightHits++
		}
	}
	if len(got) != dream.MaxCandidatesPerKeyword || insightHits != dream.MaxCandidatesPerKeyword {
		t.Errorf("VisibleTypes allowlist returned %d hits, %d of them insight; want %d/%d "+
			"(the fixture no longer reproduces the BA15 displacement)",
			len(got), insightHits, dream.MaxCandidatesPerKeyword, dream.MaxCandidatesPerKeyword)
	}

	// (b) The V-9 allowlist: the same search, intersected with dream.linkable.
	linkable := make(map[string]bool, len(set.DreamLinkableTypes()))
	for _, n := range set.DreamLinkableTypes() {
		linkable[n] = true
	}
	var intersect []string
	for _, n := range set.VisibleTypes() {
		if linkable[n] {
			intersect = append(intersect, n)
		}
	}
	got, _, err = rrf.Search(ctx, pool, qvec, baKeyword, baKeyword, []string{baScope}, nil, nil,
		dream.MaxCandidatesPerKeyword, "", "", intersect, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search over intersect(VisibleTypes, DreamLinkableTypes): %v", err)
	}
	if len(got) != dream.MaxCandidatesPerKeyword {
		t.Fatalf("intersected allowlist returned %d hits, want %d", len(got), dream.MaxCandidatesPerKeyword)
	}
	for _, res := range got {
		if res.TypeName == "insight" {
			t.Errorf("intersected allowlist still returned insight block %s", res.ID)
		}
		if !baContains(baKnowledgeIDs, res.ID) {
			t.Errorf("unexpected hit %s (type %s) — want only the five knowledge candidates", res.ID, res.TypeName)
		}
	}
}

// baSeedCorpus plants the eleven-block fixture and moves the insight type to
// its post-E-4 stance. Shared by the gate and its diagnosis so both reason
// about literally the same corpus.
func baSeedCorpus(t *testing.T, pool *pgxpool.Pool, tEarly time.Time) {
	t.Helper()
	insertBlock(t, pool, baSourceID, baScope, "decisions", "Quelle der Kandidatensuche", tEarly, tEarly)
	baArmBlock(t, pool, baSourceID, baSourceVec(), true)
	for i, id := range baKnowledgeIDs {
		insertBlock(t, pool, id, baScope, "projects", "Wissensblock Nummer "+string(rune('A'+i)), tEarly, tEarly)
		baArmBlock(t, pool, id, baKnowledgeVec(), false)
	}
	for i, id := range baInsightIDs {
		insertBlock(t, pool, id, baScope, "learnings", "Abgeleiteter Block Nummer "+string(rune('A'+i)), tEarly, tEarly)
		setBlockType(t, pool, id, "insight")
		baArmBlock(t, pool, id, baQueryVec(), false)
	}

	baDampInsight(t, pool)
}
