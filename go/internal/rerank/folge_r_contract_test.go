package rerank

// The contract tests of FOLGE-R (RC-D S1, cluster C8): the rerank wire body
// with "return_documents":false and without json.Marshal, and the strict
// answer decoder — red against the S1 stubs (wire_body.go), green after S2.
// TestInternalRerankBitIdenticalToBase and FuzzDecodeAnswerIsTheBase are the
// preservation pins of the internal rerank path (handler/query.go →
// rrf.RerankCrossEncoder → Score): green on the base by construction, they
// hold S2's decoder to the base's scores, bit for bit.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// folgeRForms are the byte classes of a document the wire body has to
// write: plain text, the HTML-escaped bytes of json.Marshal, control
// characters, quote and backslash, an invalid byte, U+2028/U+2029 and
// multi-byte runes.
var folgeRForms = []struct{ name, unit string }{
	{"ascii", "the quick brown fox 0123456789 "},
	{"html", "<a href=\"x\">&amp;</a> "},
	{"ctl", "\x01\x02\x1f\t\n\r\b\f"},
	{"quote", `"\\"`},
	{"invalid", "\xff\xfe a"},
	{"sep", "   "},
	{"multi", "äöü€😀"},
}

func folgeRDoc(unit string, size int) string {
	return strings.Repeat(unit, size/len(unit)+1)[:size]
}

// folgeRDocs is n documents of size bytes of form unit (each a little
// different, so no two documents share their backing array).
func folgeRDocs(unit string, n, size int) []string {
	docs := make([]string, n)
	for i := range docs {
		docs[i] = folgeRDoc(unit, size-1) + strconv.Itoa(i%10)
	}
	return docs
}

// folgeRRequest is the base's request body (rerank.go rerankRequest at
// ab6b2415), the oracle of the texts.
type folgeRRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

// TestWireBodyDecodesToTheRequest is the shape of the body (FOLGE-R, F-1):
// it decodes to exactly the members model, query, documents and
// return_documents — the first three as json.Marshal's body decodes, the
// last false — and its length is WireBodyLen. Red on the S1 stub: the
// base's body has no return_documents.
func TestWireBodyDecodesToTheRequest(t *testing.T) {
	for _, f := range folgeRForms {
		for _, n := range []int{1, 3} {
			model, query := "bge-"+f.unit, "q "+f.unit
			docs := folgeRDocs(f.unit, n, 70)
			body, err := wireBody(model, query, docs)
			if err != nil {
				t.Fatalf("%s n=%d: %v", f.name, n, err)
			}
			if got := WireBodyLen(model, query, docs); got != len(body) {
				t.Errorf("%s n=%d: WireBodyLen %d, body %d B", f.name, n, got, len(body))
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(body, &members); err != nil {
				t.Fatalf("%s n=%d: the body is no JSON object: %v", f.name, n, err)
			}
			keys := slices.Sorted(func(yield func(string) bool) {
				for k := range members {
					if !yield(k) {
						return
					}
				}
			})
			if want := []string{"documents", "model", "query", "return_documents"}; !slices.Equal(keys, want) {
				t.Errorf("%s n=%d: the body carries %v, want %v (FOLGE-R F-1: \"return_documents\":false on the wire)", f.name, n, keys, want)
				continue
			}
			if string(members["return_documents"]) != "false" {
				t.Errorf("%s n=%d: return_documents is %s, want false", f.name, n, members["return_documents"])
			}
			// The texts read back as json.Marshal's body reads back (an
			// invalid byte as U+FFFD on both sides).
			oracle, _ := json.Marshal(folgeRRequest{Model: model, Query: query, Documents: docs})
			var want, got folgeRRequest
			_ = json.Unmarshal(oracle, &want)
			_ = json.Unmarshal(body, &got)
			if got.Model != want.Model || got.Query != want.Query || !slices.Equal(got.Documents, want.Documents) {
				t.Errorf("%s n=%d: the body decodes to other texts than json.Marshal's", f.name, n)
			}
		}
	}
}

// TestScoreSendsReturnDocumentsFalse is F-1 over the wire: the upstream
// sees "return_documents": false in every Score request. Red on the S1 stub.
func TestScoreSendsReturnDocumentsFalse(t *testing.T) {
	var mu sync.Mutex
	var seen []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		seen = append(seen, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"index":1,"relevance_score":0.5},{"index":0,"relevance_score":-1}]}`)
	}))
	t.Cleanup(srv.Close)
	scores, _, err := Score(context.Background(), backends.Backend{Host: srv.URL}, "m", "q", []string{"a", "<b>"}, DomainAuto)
	if err != nil || len(scores) != 2 {
		t.Fatalf("Score: %v %v", scores, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	if v, ok := seen[0]["return_documents"]; !ok || string(v) != "false" {
		t.Errorf("the upstream saw return_documents=%q (present %v), want false — a ctx peer's /v1/rerank and the Jina API echo every document without it (FOLGE-R F-1)", v, ok)
	}
}

// TestRerankPackageHasNoJSONMarshal is the census of F-3: no json.Marshal,
// json.MarshalIndent or json.NewEncoder in a non-test file of the package —
// the encoder's pool allocates up to the size of an earlier Marshal in the
// process, whatever the body. Red on the S1 stub (wireBody).
func TestRerankPackageHasNoJSONMarshal(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var hits []string
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "json" {
				switch sel.Sel.Name {
				case "Marshal", "MarshalIndent", "NewEncoder":
					hits = append(hits, fmt.Sprintf("%s json.%s", fset.Position(sel.Pos()), sel.Sel.Name))
				}
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("no non-test file found: the census ran in the wrong directory")
	}
	if len(hits) > 0 {
		t.Errorf("the rerank wire encodes with encoding/json (FOLGE-R F-3, S-W1): %v", hits)
	}
}

// TestDecodeAnswerRefusesSurplusFirst is the strict decoder's first rule:
// an answer with an entry past the documents is refused as such
// (errSurplusResults), whatever the surplus entry looks like — before it is
// decoded. Red on the S1 stub (the base decodes every entry, then names a
// duplicate or an out-of-range index).
func TestDecodeAnswerRefusesSurplusFirst(t *testing.T) {
	for _, c := range []struct {
		name string
		n    int
		raw  string
	}{
		{"one past, valid indices", 2, `{"results":[{"index":0,"relevance_score":1},{"index":1,"relevance_score":2},{"index":0,"relevance_score":3}]}`},
		{"one past, index out of range", 2, `{"results":[{"index":0,"relevance_score":1},{"index":1,"relevance_score":2},{"index":2,"relevance_score":3}]}`},
		{"numbers", 1, `{"results":[0,0,0]}`},
		{"empty objects", 1, `{"results":[{},{}]}`},
		{"data container", 1, `{"data":[{"index":0,"relevance_score":0.5},{"index":0,"relevance_score":0.5}]}`},
		{"echo past the end", 1, `{"results":[{"index":0,"relevance_score":1,"document":{"text":"a"}},{"index":0,"relevance_score":1,"document":{"text":"b"}}]}`},
	} {
		_, _, err := decodeAnswer([]byte(c.raw), c.n, DomainAuto)
		if !errors.Is(err, errSurplusResults) {
			t.Errorf("%s: decodeAnswer = %v, want errSurplusResults (FOLGE-R S-W2: the first entry past the documents is refused before its decode)", c.name, err)
		}
	}
}

// baseRerankResponse, baseResolveEntries and baseDecodeAnswer are the
// base's decode (ab6b2415, rerank.go), copied verbatim as the oracle of the
// internal path — S2 replaces decodeAnswer, this copy stays.
type baseRerankResult struct {
	Index          int      `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
}

type baseRerankResponse struct {
	Results []baseRerankResult `json:"results"`
	Data    json.RawMessage `json:"data"`
	Usage   struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func baseResolveEntries(result *baseRerankResponse, raw []byte, scoreDomain string) ([]baseRerankResult, bool, error) {
	entries := result.Results
	fromData := false
	if len(entries) == 0 && len(result.Data) > 0 {
		if err := json.Unmarshal(result.Data, &entries); err != nil {
			return nil, false, fmt.Errorf("rerank: decode data: %w; body: %.256s", err, raw)
		}
		fromData = true
	}
	if len(entries) == 0 {
		return nil, false, fmt.Errorf("rerank: response contains neither \"results\" nor \"data\" entries (unknown backend dialect?); body: %.256s", raw)
	}
	isProbability := fromData
	switch scoreDomain {
	case DomainLogit:
		isProbability = false
	case DomainProbability:
		isProbability = true
	}
	return entries, isProbability, nil
}

func baseDecodeAnswer(raw []byte, n int, scoreDomain string) ([]float64, int, error) {
	var result baseRerankResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, 0, fmt.Errorf("rerank: decode: %w", err)
	}
	entries, isProbability, err := baseResolveEntries(&result, raw, scoreDomain)
	if err != nil {
		return nil, 0, err
	}
	scores := make([]float64, n)
	seen := make([]bool, n)
	got := 0
	for _, r := range entries {
		if r.Index < 0 || r.Index >= n {
			return nil, 0, fmt.Errorf("rerank: result index %d out of range [0,%d)", r.Index, n)
		}
		if seen[r.Index] {
			return nil, 0, fmt.Errorf("rerank: duplicate result index %d", r.Index)
		}
		seen[r.Index] = true
		if r.RelevanceScore == nil {
			return nil, 0, fmt.Errorf("rerank: result index %d missing relevance_score (unknown score field name?)", r.Index)
		}
		score := *r.RelevanceScore
		if isProbability {
			if score < 0 || score > 1 {
				return nil, 0, fmt.Errorf("rerank: score %g at index %d outside [0,1] (probability score domain)", score, r.Index)
			}
			score = probabilityToLogit(score)
		}
		scores[r.Index] = score
		got++
	}
	if got != n {
		return nil, 0, fmt.Errorf("rerank: result count mismatch: got %d scores for %d documents", got, n)
	}
	promptTokens := result.Usage.PromptTokens
	if promptTokens <= 0 && result.Usage.TotalTokens > 0 {
		promptTokens = result.Usage.TotalTokens
	}
	return scores, promptTokens, nil
}

// folgeRAnswer is one answer of the bit-identity corpus: raw to n
// documents; pinText: the base's error text is pinned too (an answer of at
// most n entries refused for a reason of the re-alignment, not of syntax).
type folgeRAnswer struct {
	name    string
	raw     string
	n       int
	pinText bool
}

func folgeRCorpus() []folgeRAnswer {
	lits := []string{"-11.25", "6", "1e-7", "-0", "0.1", "0.30000000000000004", "1.7976931348623157e308", "5e-324", "123456789012345678901234567890", "1E+2", "-1.5e-10", "0.9999999", "1", "0"}
	var c []folgeRAnswer
	for i, l := range lits {
		c = append(c,
			folgeRAnswer{"llama/" + l, fmt.Sprintf(`{"model":"m","object":"list","usage":{"prompt_tokens":%d,"total_tokens":%d},"results":[{"index":1,"relevance_score":%s},{"index":0,"relevance_score":-3.5}]}`, 10+i, 11+i, l), 2, true},
			folgeRAnswer{"voyage/" + l, fmt.Sprintf(`{"object":"list","data":[{"relevance_score":%s,"index":0},{"relevance_score":0.25,"index":1}],"model":"rerank-2.5","usage":{"total_tokens":%d}}`, l, 7+i), 2, true},
		)
	}
	c = append(c,
		folgeRAnswer{"echo jina object", `{"results":[{"index":0,"relevance_score":2.5,"document":{"text":"a ä 😀 <b>"}},{"index":1,"relevance_score":-1,"document":{"text":"b"}}],"usage":{"prompt_tokens":3}}`, 2, true},
		folgeRAnswer{"echo string", `{"results":[{"index":0,"relevance_score":2.5,"document":"aaa"},{"index":1,"relevance_score":-1,"document":"bbb"}]}`, 2, true},
		folgeRAnswer{"echo vllm", `{"id":"rerank-1","model":"m","usage":{"total_tokens":9},"results":[{"index":1,"document":{"text":"x","multi_modal":null},"relevance_score":0.7},{"index":0,"document":{"text":"y","multi_modal":null},"relevance_score":0.1}]}`, 2, true},
		folgeRAnswer{"whitespace", "{ \"results\" : [ { \"index\" : 0 , \"relevance_score\" : 1.5 } ] , \"usage\" : { \"prompt_tokens\" : 4 } }\n", 1, true},
		folgeRAnswer{"duplicate member last wins", `{"results":[{"index":1,"index":0,"relevance_score":9,"relevance_score":1.25}]}`, 1, true},
		folgeRAnswer{"case-insensitive members", `{"RESULTS":[{"Index":0,"Relevance_Score":4.5}],"Usage":{"Prompt_Tokens":2}}`, 1, true},
		folgeRAnswer{"top_n truncated", `{"results":[{"index":2,"relevance_score":1}]}`, 3, true},
		folgeRAnswer{"duplicate index", `{"results":[{"index":0,"relevance_score":1},{"index":0,"relevance_score":2}]}`, 2, true},
		folgeRAnswer{"out of range", `{"results":[{"index":0,"relevance_score":1},{"index":5,"relevance_score":2}]}`, 2, true},
		folgeRAnswer{"negative index", `{"results":[{"index":-1,"relevance_score":1}]}`, 1, true},
		folgeRAnswer{"missing score", `{"results":[{"index":0,"score":1}]}`, 1, true},
		folgeRAnswer{"null score", `{"results":[{"index":0,"relevance_score":null}]}`, 1, true},
		folgeRAnswer{"probability out of range", `{"data":[{"index":0,"relevance_score":1.5}]}`, 1, true},
		folgeRAnswer{"neither container", `{"error":{"message":"overloaded"}}`, 1, true},
		folgeRAnswer{"null results, no data", `{"results":null}`, 1, true},
		folgeRAnswer{"empty results", `{"results":[]}`, 1, true},
		folgeRAnswer{"results beside non-array data", `{"results":[{"index":0,"relevance_score":3}],"data":{"x":1}}`, 1, true},
		folgeRAnswer{"negative prompt tokens", `{"results":[{"index":0,"relevance_score":3}],"usage":{"prompt_tokens":-4,"total_tokens":6}}`, 1, true},
		folgeRAnswer{"usage absent", `{"results":[{"index":0,"relevance_score":3}]}`, 1, true},
		folgeRAnswer{"extra top-level members", `{"id":"x","meta":{"api_version":{"version":"1"}},"results":[{"index":0,"relevance_score":3}]}`, 1, true},
		// Syntax and type errors: an error on both sides, texts free.
		folgeRAnswer{"malformed", `{"results":[{"index":0,"relevance_score":3}]`, 1, false},
		folgeRAnswer{"trailing garbage", `{"results":[{"index":0,"relevance_score":3}]}x`, 1, false},
		folgeRAnswer{"index as float", `{"results":[{"index":0.0,"relevance_score":3}]}`, 1, false},
		folgeRAnswer{"index too large", `{"results":[{"index":99999999999999999999,"relevance_score":3}]}`, 1, false},
		folgeRAnswer{"score as string", `{"results":[{"index":0,"relevance_score":"3"}]}`, 1, false},
		folgeRAnswer{"score overflow", `{"results":[{"index":0,"relevance_score":1e400}]}`, 1, false},
		folgeRAnswer{"usage as string", `{"results":[{"index":0,"relevance_score":3}],"usage":"x"}`, 1, false},
		folgeRAnswer{"non-array data", `{"data":{"x":1}}`, 1, false},
		folgeRAnswer{"numbers", `{"results":[0]}`, 1, false},
		folgeRAnswer{"not an object", `[{"index":0,"relevance_score":3}]`, 1, false},
		folgeRAnswer{"surplus", `{"results":[{"index":0,"relevance_score":1},{"index":1,"relevance_score":2}]}`, 1, false},
	)
	return c
}

// folgeRSame compares decodeAnswer with the base's decode of one answer:
// the same scores bit for bit and the same prompt tokens, or an error on
// both sides (with the same text when pinText).
func folgeRSame(t *testing.T, name string, raw []byte, n int, domain string, pinText bool) {
	t.Helper()
	got, gotTok, gotErr := decodeAnswer(raw, n, domain)
	want, wantTok, wantErr := baseDecodeAnswer(raw, n, domain)
	switch {
	case (gotErr != nil) != (wantErr != nil):
		t.Errorf("%s (%s): decodeAnswer err=%v, base err=%v", name, domain, gotErr, wantErr)
	case gotErr != nil:
		if pinText && gotErr.Error() != wantErr.Error() {
			t.Errorf("%s (%s): error %q, base %q", name, domain, gotErr, wantErr)
		}
	default:
		if gotTok != wantTok || len(got) != len(want) {
			t.Errorf("%s (%s): %d scores %d tokens, base %d scores %d tokens", name, domain, len(got), gotTok, len(want), wantTok)
			return
		}
		for i := range got {
			if math.Float64bits(got[i]) != math.Float64bits(want[i]) {
				t.Errorf("%s (%s): score %d is %v (%#x), base %v (%#x)", name, domain, i, got[i], math.Float64bits(got[i]), want[i], math.Float64bits(want[i]))
			}
		}
	}
}

// TestInternalRerankBitIdenticalToBase is the gate of the internal rerank
// path (handler/query.go → rrf.RerankCrossEncoder → Score): over every
// answer of the corpus and every score domain, decodeAnswer reads the
// base's scores bit for bit, the base's prompt tokens and — for an answer
// of at most n entries refused by the re-alignment — the base's error text;
// and Score over the wire reads the same as the decode. A preservation pin:
// green on the base by construction (S1 decodes with the base's code).
func TestInternalRerankBitIdenticalToBase(t *testing.T) {
	domains := []string{"", DomainAuto, DomainLogit, DomainProbability}
	for _, a := range folgeRCorpus() {
		for _, d := range domains {
			folgeRSame(t, a.name, []byte(a.raw), a.n, d, a.pinText)
		}
	}
	// Over the wire, through Score: the answer as the server sends it.
	var cur sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		raw, _ := cur.Load("raw")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, raw.(string))
	}))
	t.Cleanup(srv.Close)
	b := backends.Backend{Host: srv.URL}
	for _, a := range folgeRCorpus() {
		cur.Store("raw", a.raw)
		docs := make([]string, a.n)
		for i := range docs {
			docs[i] = "d" + strconv.Itoa(i)
		}
		for _, d := range domains {
			got, gotTok, gotErr := Score(context.Background(), b, "m", "q", docs, d)
			want, wantTok, wantErr := baseDecodeAnswer([]byte(a.raw), a.n, d)
			if (gotErr != nil) != (wantErr != nil) {
				t.Errorf("Score %s (%s): err=%v, base err=%v", a.name, d, gotErr, wantErr)
				continue
			}
			if gotErr != nil {
				if a.pinText && gotErr.Error() != wantErr.Error() {
					t.Errorf("Score %s (%s): error %q, base %q", a.name, d, gotErr, wantErr)
				}
				continue
			}
			if gotTok != wantTok || !slices.EqualFunc(got, want, func(x, y float64) bool { return math.Float64bits(x) == math.Float64bits(y) }) {
				t.Errorf("Score %s (%s): %v/%d, base %v/%d", a.name, d, got, gotTok, want, wantTok)
			}
		}
	}
}

// FuzzDecodeAnswerIsTheBase is the fuzzer of the same gate: any answer to
// 1…16 documents in any domain — an error on both sides or the same scores
// bit for bit and the same tokens. The corpus seeds it; under plain go test
// it runs the seeds.
func FuzzDecodeAnswerIsTheBase(f *testing.F) {
	for _, a := range folgeRCorpus() {
		f.Add([]byte(a.raw), uint8(a.n), uint8(0))
	}
	domains := []string{"", DomainAuto, DomainLogit, DomainProbability}
	f.Fuzz(func(t *testing.T, raw []byte, n, d uint8) {
		docs := int(n%16) + 1
		folgeRSame(t, "fuzz", raw, docs, domains[int(d)%len(domains)], false)
	})
}
