//go:build integration

// Issue #528 through the production request path (POST /api/query, HandleQuery
// with stub backends), on corpora written through store.UpsertBlock and
// store.StoreEmbedding:
//   - point 2: of two blocks whose titles fold to the same form, the one whose
//     title is the question byte for byte (case and white space aside) comes
//     first, also when the pipeline ranks its twin higher;
//   - point 3: a title asked inside quotation marks is placed;
//   - point 4: a title asked behind a leading phrase is placed, a topic
//     question with the same start is not;
//   - point 6 lives in query_label_scale_528_integration_test.go;
//   - point 7: the placed source carries its own score in a field of its own.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// seededEnv builds a title-floor environment on a corpus seed writes through
// put (store.UpsertBlock and store.StoreEmbedding, the vector at cosine cos to
// the stub's question vector).
func seededEnv(t *testing.T, seed func(put func(title, content string, cos float64) string)) *titleFloorEnv {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	env := &titleFloorEnv{ids: map[string]string{}, pool: pool}
	k := 0
	seed(func(title, content string, cos float64) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "learnings", title, content, nil, nil, titleFloorScope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed %q: %v", title, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "stub-tf507", titleFloorVec(cos, k)); err != nil {
			t.Fatalf("seed %q: embedding: %v", title, err)
		}
		k++
		env.ids[title] = b.ID
		return b.ID
	})
	env.h = titleFloorHandler(t, pool, nil, 0)
	return env
}

// twinsEnv: two blocks whose titles fold alike ("Release notes alpha?" ranked
// higher, "Release notes alpha" lower) behind twenty notes whose text repeats
// the words of the title, so they lead every arm but the title arm.
func twinsEnv(t *testing.T) (*titleFloorEnv, string, string) {
	t.Helper()
	var higher, lower string
	e := seededEnv(t, func(put func(title, content string, cos float64) string) {
		for j := 0; j < 20; j++ {
			put(fmt.Sprintf("Changelog digest %02d", j),
				strings.Repeat("release notes alpha, ", 5)+fmt.Sprintf("digest %02d.", j), 0.90-0.01*float64(j))
		}
		higher = put("Release notes alpha?", "Release notes of the alpha channel.", 0.30)
		lower = put("Release notes alpha", "Release notes of the alpha channel.", 0.20)
	})
	return e, higher, lower
}

func TestQueryTitleFloorTwinsByteEqualFirst(t *testing.T) {
	e, higher, lower := twinsEnv(t)
	fo := e.ask(t, "Release notes alpha", 10, true).ArmRanks.FusionOrder
	ph, pl := position(fo, higher), position(fo, lower)
	if ph == 0 || pl == 0 || ph >= pl {
		t.Fatalf("precondition: fusion positions %d (twin with the mark) and %d (byte-equal) — want both in the window, the twin ahead", ph, pl)
	}
	if got := e.ask(t, "Release notes alpha", 10, false).ids(); got[0] != lower {
		t.Errorf("question equal to the lower-ranked twin byte for byte: first source %s (fusion %d/%d), want the byte-equal block %s", got[0], ph, pl, lower)
	}
	if got := e.ask(t, "Release notes alpha?", 10, false).ids(); got[0] != higher {
		t.Errorf("question equal to the twin with the mark: first source %s, want %s", got[0], higher)
	}
	// A question that equals neither title before folding: the rank decides,
	// as before.
	if got := e.ask(t, "Release notes alpha!", 10, false).ids(); got[0] != higher {
		t.Errorf("question equal to neither twin: first source %s, want the higher-ranked %s", got[0], higher)
	}
}

func TestQueryTitleFloorQuotedAndPrefixed(t *testing.T) {
	e := titleFloorSetup(t)
	const title = "Orbit nightly report 2026-08-31"
	if fp := position(e.ask(t, title, 10, true).ArmRanks.FusionOrder, e.target); fp <= 10 {
		t.Fatalf("precondition: fusion position of the named block = %d (want inside the window, past 10)", fp)
	}
	for _, q := range []string{
		"„" + title + "“",
		"\"" + title + "\"?",
		"zeig mir " + title,
		"Was steht in „" + title + "“?",
		"show me " + title,
	} {
		if got := e.ask(t, q, 10, false).ids(); position(got, e.target) != 1 {
			t.Errorf("%q: named block at source %d, want 1", q, position(got, e.target))
		}
	}
	// A question that contains a quotation, and topic questions with the same
	// start: the ten sources are the first ten of twenty.
	for _, q := range []string{
		"Was steht in „" + title + "“ über den Build?",
		"zeig mir alle Orbit nightly reports",
		"zeig mir Orbit nightly report",
		"show me Deploy",
	} {
		ten := e.ask(t, q, 10, false).ids()
		twenty := e.ask(t, q, 20, false).ids()
		if len(twenty) < 10 || strings.Join(ten, ",") != strings.Join(twenty[:10], ",") {
			t.Errorf("%q: the ten sources are not the first ten of twenty:\n ten %v\n 20  %v", q, ten, twenty)
		}
	}
}

// labelResp reads the fields the label and the score-field tests need.
type labelResp struct {
	Confidence string `json:"confidence"`
	Sources    []struct {
		ID               string   `json:"id"`
		Score            float64  `json:"score"`
		RRFScoreOriginal *float64 `json:"rrf_score_original"`
		TitleFloorScore  *float64 `json:"title_floor_score"`
		CitationIndex    *int     `json:"citation_index"`
	} `json:"sources"`
}

func askRaw(t *testing.T, h *QueryHandler, body map[string]any) labelResp {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ar := &auth.AuthResult{ApiKeyID: "tf507-key", HomeScope: titleFloorScope, ReadScopes: []string{titleFloorScope}, IsValid: true}
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out labelResp
	if err := json.Unmarshal(bytes.TrimSpace(rec.Body.Bytes()), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// Point 7: the placed source carries its own score in title_floor_score; score
// and rrf_score_original stay the place's. No other source carries the field.
func TestQueryTitleFloorOwnScoreField(t *testing.T) {
	e := titleFloorSetup(t)
	const q = "Orbit nightly report 2026-08-31"
	for _, body := range []map[string]any{
		{"query": q, "limit": 10, "synthesize": false},
		{"query": q, "limit": 10},
	} {
		r := askRaw(t, e.h, body)
		if len(r.Sources) < 2 || r.Sources[0].ID != e.target {
			t.Fatalf("%v: precondition: the named block is not the first source", body)
		}
		own := r.Sources[0].TitleFloorScore
		if own == nil {
			t.Errorf("%v: the placed source carries no title_floor_score", body)
		} else if !(*own < r.Sources[0].Score) {
			t.Errorf("%v: title_floor_score %v, want the block's own score below the place's %v", body, *own, r.Sources[0].Score)
		}
		for i, s := range r.Sources[1:] {
			if s.TitleFloorScore != nil {
				t.Errorf("%v: source %d carries title_floor_score %v", body, i+2, *s.TitleFloorScore)
			}
		}
	}
	// No placement, no field.
	for _, s := range askRaw(t, e.h, map[string]any{"query": "Orbit nightly report", "limit": 10, "synthesize": false}).Sources {
		if s.TitleFloorScore != nil {
			t.Errorf("topic question: source %s carries title_floor_score", s.ID)
		}
	}
}
