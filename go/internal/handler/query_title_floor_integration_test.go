//go:build integration

// Issue #507 through the production request path (POST /api/query, HandleQuery
// with a stub embed backend): a block among near-duplicates — a family of
// nightly reports whose titles differ in the date only — is not among the ten
// sources for its own title, because the fusion puts it behind its siblings
// (the title arm finds it first and weighs least, the semantic arm sees near
// identical texts, the date in the question fills both full-text arms with
// build logs of the same month). Every block is written through
// store.UpsertBlock (full-text vectors from the production trigger,
// content_times from the production extraction) and gets its vector through
// store.StoreEmbedding; the stub hands every question the same vector, so the
// semantic order is the one the seeded cosines set.
//
// Pins, one per class of question that resembles a title without being one
// (the floor must move nothing there): a question about the family's topic, a
// question that contains a short title, and a title made of one frequent word
// that other titles contain. "Moves nothing" is read off the response itself:
// the sources at limit 10 are the first ten sources at limit 20 — the floor is
// the one stage that can break that prefix.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const titleFloorScope = "tf507"

// titleFloorVec is a unit vector whose cosine with the question vector
// (axis 0) is cos; axis k keeps the vectors of different blocks apart.
func titleFloorVec(cos float64, k int) []float32 {
	v := make([]float32, 1024)
	v[0] = float32(cos)
	v[1+k%1023] = float32(math.Sqrt(1 - cos*cos))
	return v
}

type titleFloorEnv struct {
	h      *QueryHandler
	ids    map[string]string // title -> id
	target string
	twins  []string // two blocks whose titles fold to the same form
}

func titleFloorSetup(t *testing.T) *titleFloorEnv {
	t.Helper()
	return titleFloorSetupWith(t, nil)
}

// titleFloorSetupWith builds the corpus and a handler whose configuration mod
// may adjust (the confidence thresholds of the synthesis tests).
func titleFloorSetupWith(t *testing.T, mod func(*config.Config)) *titleFloorEnv {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	env := &titleFloorEnv{ids: map[string]string{}}
	k := 0
	putIn := func(category, title, content string, cos float64) {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, category, title, content, nil, nil, titleFloorScope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed %q: %v", title, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "stub-tf507", titleFloorVec(cos, k)); err != nil {
			t.Fatalf("seed %q: embedding: %v", title, err)
		}
		k++
		env.ids[category+"/"+title] = b.ID
		if category == "learnings" {
			env.ids[title] = b.ID
		}
	}
	put := func(title, content string, cos float64) { t.Helper(); putIn("learnings", title, content, cos) }
	day := func(i int) string { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02") }

	// The family: 45 nightly reports, one per day, the day in the title only
	// and near-identical text. The semantic order runs from the first report
	// down; the named one (day 30) sits at cosine rank 31 of the family.
	for i := 0; i < 45; i++ {
		put(fmt.Sprintf("Orbit nightly report %s", day(i)),
			fmt.Sprintf("Orbit nightly report, run %d. Build green, tests passed, artefacts uploaded.", i),
			0.95-0.01*float64(i))
	}
	env.target = env.ids["Orbit nightly report "+day(30)]

	// Build logs of the same month that name many of its days: they fill the
	// full-text arms through the date expansion of a question naming a day.
	for j := 0; j < 110; j++ {
		var dates []string
		for d := 0; d < 31; d += 3 {
			dates = append(dates, day((j+d)%31))
		}
		put(fmt.Sprintf("Build log batch %03d", j),
			"Build log covering "+strings.Join(dates, ", ")+" and the August 2026 runs "+day(30)+".",
			0.40-0.001*float64(j))
	}

	// Short title, contained in a later question.
	put("Orbit", "The build orchestrator of the project.", 0.05)
	// A title of one frequent word, which other titles contain.
	put("Deploy", "Notes kept short.", 0.04)
	for j := 0; j < 12; j++ {
		put(fmt.Sprintf("Deploy runbook %02d", j),
			fmt.Sprintf("Deploy steps: deploy the service, verify the deploy, roll back a failed deploy (runbook %02d).", j),
			0.60-0.01*float64(j))
	}

	// Two blocks carrying the same title (two categories), far from the
	// question vector; the first sits nearer to it.
	// Their title is every word of fifteen artefact indexes, which repeat it
	// and so outrank them in the full-text arms (and sit nearer the question
	// vector).
	for j := 0; j < 15; j++ {
		put(fmt.Sprintf("Artefact index %02d", j),
			strings.Repeat("build log batch 2026-08, ", 6)+fmt.Sprintf("artefacts of index %02d.", j), 0.50-0.005*float64(j))
	}
	putIn("learnings", "Build log batch 2026-08", "Summary, first copy.", 0.03)
	putIn("decisions", "Build log batch 2026-08", "Summary, second copy.", 0.02)
	env.twins = []string{env.ids["learnings/Build log batch 2026-08"], env.ids["decisions/Build log batch 2026-08"]}

	// The stub: one vector, axis 0, for every question; a fixed answer for
	// the synthesis.
	qvec := make([]float64, 1024)
	qvec[0] = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/embed":
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{qvec}})
		case "/api/chat":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": "The report says the build was green [1]."},
				"done":    true, "eval_count": 1, "prompt_eval_count": 1,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "tf507-embed", Host: srv.URL, Protocol: backends.ProtocolOllama, Model: "stub-tf507",
			Trust: backends.TrustFull, Enabled: true, Priority: 1, Roles: []string{backends.RoleEmbed}},
		{ID: "s", Name: "tf507-synthesis", Host: srv.URL, Protocol: backends.ProtocolOllama, Model: "stub-chat",
			Trust: backends.TrustFull, Enabled: true, Priority: 1, NumCtx: 8192, Roles: []string{backends.RoleSynthesis}},
	})
	cfg := bw2Config()
	q := config.Defaults().Query
	q.Timezone = time.UTC
	q.Translate = "off"
	cfg.Query = q
	if mod != nil {
		mod(cfg)
	}
	env.h = NewQueryHandler(pool, config.NewStore(cfg), bpool, nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))
	return env
}

type titleFloorResp struct {
	Confidence string `json:"confidence"`
	Sources    []struct {
		ID            string  `json:"id"`
		Title         string  `json:"title"`
		Score         float64 `json:"score"`
		CitationIndex *int    `json:"citation_index"`
	} `json:"sources"`
	ArmRanks *struct {
		FusionOrder []string `json:"fusion_order"`
	} `json:"arm_ranks"`
}

func (e *titleFloorEnv) ask(t *testing.T, q string, limit int, armRanks bool) titleFloorResp {
	t.Helper()
	return e.askPinned(t, q, limit, armRanks, "")
}

// askPinned is ask with a pinned translation (measurement seam, admin only):
// retrieval then runs on the pinned text while the caller's question stays q.
// askSynth asks with synthesis (the stub answers); a source carries
// citation_index exactly when it stood in the synthesis prompt.
func (e *titleFloorEnv) askSynth(t *testing.T, q string, limit int) titleFloorResp {
	t.Helper()
	return e.do(t, q, map[string]any{"query": q, "limit": limit}, false)
}

func (e *titleFloorEnv) askPinned(t *testing.T, q string, limit int, armRanks bool, pinned string) titleFloorResp {
	t.Helper()
	body := map[string]any{"query": q, "limit": limit, "synthesize": false}
	if armRanks {
		body["arm_ranks"] = true
	}
	if pinned != "" {
		body["pinned_translation"] = pinned
	}
	return e.do(t, q, body, armRanks)
}

func (e *titleFloorEnv) do(t *testing.T, q string, body map[string]any, armRanks bool) titleFloorResp {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ar := &auth.AuthResult{ApiKeyID: "tf507-key", HomeScope: titleFloorScope, ReadScopes: []string{titleFloorScope},
		IsValid: true, IsAdmin: armRanks}
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	e.h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%q: status %d: %s", q, rec.Code, rec.Body.String())
	}
	var out titleFloorResp
	if err := json.Unmarshal(bytes.TrimSpace(rec.Body.Bytes()), &out); err != nil {
		t.Fatalf("%q: decode: %v", q, err)
	}
	return out
}

func (r titleFloorResp) ids() []string {
	out := make([]string, len(r.Sources))
	for i, s := range r.Sources {
		out[i] = s.ID
	}
	return out
}

func position(ids []string, id string) int {
	for i, x := range ids {
		if x == id {
			return i + 1
		}
	}
	return 0
}

// TestQueryTitleFloorFamily: the named block is among the ten sources for its
// own title, at the last place, and the nine before it are the nine the
// pipeline ranks first.
func TestQueryTitleFloorFamily(t *testing.T) {
	e := titleFloorSetup(t)
	const q = "Orbit nightly report 2026-08-31"

	// Precondition, read off the measurement seam: the fusion holds the block
	// in its candidate window and puts it behind its siblings.
	fo := e.ask(t, q, 10, true).ArmRanks.FusionOrder
	fp := position(fo, e.target)
	if fp == 0 || fp <= 10 {
		t.Fatalf("precondition: fusion position of the named block = %d (want inside the window, past 10)", fp)
	}

	ten := e.ask(t, q, 10, false).ids()
	got := position(ten, e.target)
	t.Logf("fusion position %d, source position %d", fp, got)
	if got == 0 {
		t.Fatalf("block for its own title in a family of 45 near-duplicates: not among the 10 sources (fusion position %d)", fp)
	}
	if got != 10 {
		t.Errorf("named block at source %d, want the last place (10)", got)
	}
	tw := e.ask(t, q, 20, false)
	var rest []string
	var restScores []float64
	for _, s := range tw.Sources {
		if s.ID != e.target {
			rest = append(rest, s.ID)
			restScores = append(restScores, s.Score)
		}
	}
	if strings.Join(ten[:9], ",") != strings.Join(rest[:9], ",") {
		t.Errorf("first nine sources moved:\n got %v\nwant %v", ten[:9], rest[:9])
	}
	// The placed block carries the score of the place it took — the score of
	// the source it displaced — so the sources stay sorted and the synthesis
	// path's score threshold judges that place.
	tenResp := e.ask(t, q, 10, false)
	if got := tenResp.Sources[9].Score; got != restScores[9] {
		t.Errorf("placed block's score %v, want the displaced source's %v", got, restScores[9])
	}

	// A translated question: retrieval runs on the translation, the floor
	// compares the question the caller sent.
	tr := e.askPinned(t, q, 10, true, "nightly Orbit report for the last day of August").ids()
	if p := position(tr, e.target); p != 10 {
		t.Errorf("translated question: named block at source %d, want 10", p)
	}
}

// TestQueryTitleFloorPins: questions that resemble a title without naming one
// block keep the ranking — the ten sources are the first ten of twenty.
func TestQueryTitleFloorPins(t *testing.T) {
	e := titleFloorSetup(t)
	cases := []struct {
		class, q, outside string
	}{
		// A question about the family's topic: no title equals it.
		{"family topic", "Orbit nightly report", e.target},
		// A question that contains a short title ("Orbit").
		{"short title inside the question", "Wie starte ich Orbit nach dem Update neu?", e.ids["Orbit"]},
		// A title of one frequent word that other titles contain.
		{"one frequent word", "deploy", e.ids["Deploy"]},
	}
	for _, c := range cases {
		t.Run(c.class, func(t *testing.T) {
			ten := e.ask(t, c.q, 10, false).ids()
			twenty := e.ask(t, c.q, 20, false).ids()
			if len(twenty) < 10 {
				t.Fatalf("precondition: %d sources at limit 20", len(twenty))
			}
			if strings.Join(ten, ",") != strings.Join(twenty[:10], ",") {
				t.Errorf("%q: the ten sources are not the first ten of twenty:\n ten %v\n 20  %v", c.q, ten, twenty[:10])
			}
			// Precondition of a pin that can fail: the block a wider rule
			// would lift is outside the first ten.
			if p := position(twenty, c.outside); p >= 1 && p <= 10 {
				t.Fatalf("precondition: the title neighbour already sits at source %d", p)
			}
		})
	}
}
