package embed_test

// The internal embed path (embed.Embed) is bit-identical to the base before
// W02-09 (f6c6c189) — the differential of Fremd-Review 2 (Prüfpunkt 2) kept
// as a gate (hardening H-2): 3 276 cases (protocol × num_ctx × API key ×
// prefix × text × answer form) against one httptest fixture; per case the
// request bytes (method, path, headers, body) of every request and the result
// (vector bits, tokens, error). The output of the base tree hashes to
// bitIdentBaseSHA; any change of a request byte, a vector bit, a token count
// or an error text of the internal path turns this red. CTX_BITIDENT_OUT=<file>
// writes the lines for a diff against the base (cmp).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embed"
)

type bitIdentReq struct {
	Method, Path string
	Header       []string
	Body         string
}

type bitIdentLine struct {
	Case     string
	Requests []bitIdentReq
	VecLen   int
	VecSHA   string
	First4   []uint32
	Tokens   int
	Err      string
}

func bitIdentVec(seed, n int, style int) []float64 {
	v := make([]float64, n)
	x := uint64(seed*2654435761 + 12345)
	for i := range v {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		f := (float64(x%2000003)/1000001.5 - 1.0)
		switch style {
		case 1: // tiny / denormal-ish / exponent forms
			if i%7 == 0 {
				f *= 1e-30
			}
			if i%11 == 0 {
				f = math.Copysign(0, -1)
			}
			if i%13 == 0 {
				f *= 1e-310
			}
		case 2: // large magnitudes
			if i%5 == 0 {
				f *= 1e30
			}
		case 3: // constant ⇒ variance gate
			f = 0.03125
		}
		v[i] = f
	}
	return v
}

func bitIdentNum(f float64, form int) string {
	switch form {
	case 1:
		return strconv.FormatFloat(f, 'e', -1, 64)
	case 2:
		return strconv.FormatFloat(f, 'f', 20, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func bitIdentArr(v []float64, form int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(bitIdentNum(f, form))
	}
	b.WriteByte(']')
	return b.String()
}

type bitIdentResp struct {
	status int
	body   string
	hdr    map[string]string
}

// bitIdentBaseSHA is sha256 over the base tree's output (f6c6c189,
// Fremd-Review 2 logs/bitident-base.jsonl, 3 276 lines); bitIdentCases its
// line count.
const (
	bitIdentBaseSHA = "8cd7b9956e8dde1e5311649e35b000302db7cc80d0765b53cf3373a3dd5e88e5"
	bitIdentCases   = 3276
)

func TestInternalEmbedBitIdenticalToBase(t *testing.T) {
	out := os.Getenv("CTX_BITIDENT_OUT")
	var mu sync.Mutex
	var reqs []bitIdentReq
	var plan []bitIdentResp
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var hs []string
		for k, vs := range r.Header {
			for _, v := range vs {
				hs = append(hs, k+": "+v)
			}
		}
		sort.Strings(hs)
		mu.Lock()
		reqs = append(reqs, bitIdentReq{r.Method, r.URL.RequestURI(), hs, string(body)})
		var p bitIdentResp
		if len(plan) > 0 {
			p = plan[0]
			if len(plan) > 1 {
				plan = plan[1:]
			}
		}
		mu.Unlock()
		for k, v := range p.hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(p.status)
		_, _ = io.WriteString(w, p.body)
	}))
	defer srv.Close()

	texts := map[string]string{
		"plain":   "hello world",
		"html":    `<b>a & "b"</b>` + "  ",
		"ctrl":    "\x00\x01\t\n\r\x1f",
		"badutf8": "ok\xff\xfe\xc0end",
		"long":    strings.Repeat("é", 5000),
		"emoji":   "🧪 – „Zitat“",
		"empty":   "",
	}
	textNames := []string{"plain", "html", "ctrl", "badutf8", "long", "emoji", "empty"}
	prefixes := map[string]embed.Prefix{"query": embed.PrefixQuery, "doc": embed.PrefixDocument, "none": embed.Prefix(99)}
	prefixNames := []string{"query", "doc", "none"}

	openaiForms := func(seed int) []struct {
		name string
		resp []bitIdentResp
	} {
		v1024 := bitIdentVec(seed, 1024, 0)
		v4096 := bitIdentVec(seed, 4096, 0)
		vt := bitIdentVec(seed, 1536, 1)
		vl := bitIdentVec(seed, 1024, 2)
		return []struct {
			name string
			resp []bitIdentResp
		}{
			{"std4096", []bitIdentResp{{200, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":` + bitIdentArr(v4096, 0) + `}],"model":"x","usage":{"prompt_tokens":7,"total_tokens":7}}`, nil}}},
			{"noindex1024", []bitIdentResp{{200, `{"data":[{"embedding":` + bitIdentArr(v1024, 0) + `}],"usage":{"prompt_tokens":3}}`, nil}}},
			{"totalonly", []bitIdentResp{{200, `{"data":[{"embedding":` + bitIdentArr(v1024, 1) + `,"index":0}],"usage":{"total_tokens":11}}`, nil}}},
			{"tinyforms", []bitIdentResp{{200, `{"data":[{"index":0,"embedding":` + bitIdentArr(vt, 1) + `}]}`, nil}}},
			{"fixed20", []bitIdentResp{{200, `{"data":[{"index":0,"embedding":` + bitIdentArr(v1024, 2) + `}],"usage":{"prompt_tokens":1,"total_tokens":2}}`, nil}}},
			{"large", []bitIdentResp{{200, `{"data":[{"index":0,"embedding":` + bitIdentArr(vl, 0) + `}]}`, nil}}},
			{"short1000", []bitIdentResp{{200, `{"data":[{"index":0,"embedding":` + bitIdentArr(bitIdentVec(seed, 1000, 0), 0) + `}]}`, nil}}},
			{"constant", []bitIdentResp{{200, `{"data":[{"index":0,"embedding":` + bitIdentArr(bitIdentVec(seed, 1024, 3), 0) + `}]}`, nil}}},
			{"emptydata", []bitIdentResp{{200, `{"data":[]}`, nil}}},
			{"emptyvec", []bitIdentResp{{200, `{"data":[{"index":0,"embedding":[]}]}`, nil}}},
			{"extrafields", []bitIdentResp{{200, `{"id":"e","data":[{"index":0,"embedding":` + bitIdentArr(v1024, 0) + `,"x":{"y":[1,2]}}],"usage":{"prompt_tokens":5,"total_tokens":5,"z":1},"w":null}`, nil}}},
			{"err500", []bitIdentResp{{500, `{"error":"boom"}`, nil}}},
			{"err429", []bitIdentResp{{429, `{"error":"slow"}`, map[string]string{"Retry-After": "3"}}}},
			{"err400", []bitIdentResp{{400, `{"error":{"message":"too long"}}`, nil}}},
			{"retry502then200", []bitIdentResp{{502, `bad gateway`, nil}, {200, `{"data":[{"index":0,"embedding":` + bitIdentArr(v1024, 0) + `}],"usage":{"prompt_tokens":2,"total_tokens":2}}`, nil}}},
			{"badjson", []bitIdentResp{{200, `{"data":[{"index":0,"embedding":[1,2,`, nil}}},
			{"nullindex", []bitIdentResp{{200, `{"data":[{"index":null,"embedding":` + bitIdentArr(v1024, 0) + `}]}`, nil}}},
		}
	}
	ollamaForms := func(seed int) []struct {
		name string
		resp []bitIdentResp
	} {
		v1024 := bitIdentVec(seed, 1024, 0)
		v4096 := bitIdentVec(seed, 4096, 0)
		vt := bitIdentVec(seed, 2048, 1)
		return []struct {
			name string
			resp []bitIdentResp
		}{
			{"std4096", []bitIdentResp{{200, `{"model":"m","embeddings":[` + bitIdentArr(v4096, 0) + `],"prompt_eval_count":9}`, nil}}},
			{"std1024", []bitIdentResp{{200, `{"embeddings":[` + bitIdentArr(v1024, 1) + `]}`, nil}}},
			{"tinyforms", []bitIdentResp{{200, `{"embeddings":[` + bitIdentArr(vt, 1) + `],"prompt_eval_count":4}`, nil}}},
			{"short1000", []bitIdentResp{{200, `{"embeddings":[` + bitIdentArr(bitIdentVec(seed, 1000, 0), 0) + `]}`, nil}}},
			{"constant", []bitIdentResp{{200, `{"embeddings":[` + bitIdentArr(bitIdentVec(seed, 1024, 3), 0) + `]}`, nil}}},
			{"emptylist", []bitIdentResp{{200, `{"embeddings":[]}`, nil}}},
			{"emptyvec", []bitIdentResp{{200, `{"embeddings":[[]]}`, nil}}},
			{"err500", []bitIdentResp{{500, `{"error":"boom"}`, nil}}},
			{"err404", []bitIdentResp{{404, `{"error":"model not found"}`, nil}}},
			{"retry503then200", []bitIdentResp{{503, `x`, nil}, {200, `{"embeddings":[` + bitIdentArr(v1024, 0) + `],"prompt_eval_count":1}`, nil}}},
			{"badjson", []bitIdentResp{{200, `{"embeddings":[[1,2`, nil}}},
		}
	}

	sum := sha256.New()
	var w io.Writer = sum
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		w = io.MultiWriter(sum, f)
	}
	enc := json.NewEncoder(w)
	cases := 0
	seed := 0
	for _, proto := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama, ""} {
		for _, numCtx := range []int{0, 2048} {
			for _, apiKey := range []string{"", "row-key"} {
				for _, pn := range prefixNames {
					for _, tn := range textNames {
						seed++
						forms := ollamaForms(seed)
						if proto == backends.ProtocolOpenAI {
							forms = openaiForms(seed)
						}
						for _, form := range forms {
							mu.Lock()
							reqs = nil
							plan = form.resp
							mu.Unlock()
							b := backends.Backend{Host: srv.URL, Protocol: proto, Model: "qwen3-embedding:8b", APIKey: apiKey, NumCtx: numCtx}
							vec, tok, err := embed.Embed(context.Background(), b, texts[tn], prefixes[pn])
							line := bitIdentLine{Case: fmt.Sprintf("%s/ctx%d/key%t/%s/%s/%s", proto, numCtx, apiKey != "", pn, tn, form.name), Tokens: tok}
							mu.Lock()
							line.Requests = append([]bitIdentReq(nil), reqs...)
							mu.Unlock()
							if err != nil {
								line.Err = err.Error()
							}
							line.VecLen = len(vec)
							h := sha256.New()
							for i, x := range vec {
								bits := math.Float32bits(x)
								_, _ = fmt.Fprintf(h, "%08x", bits)
								if i < 4 {
									line.First4 = append(line.First4, bits)
								}
							}
							line.VecSHA = hex.EncodeToString(h.Sum(nil))
							if err := enc.Encode(line); err != nil {
								t.Fatal(err)
							}
							cases++
						}
					}
				}
			}
		}
	}
	got := hex.EncodeToString(sum.Sum(nil))
	t.Logf("internal path: %d cases, sha256 %s (base %s)", cases, got, bitIdentBaseSHA)
	if cases != bitIdentCases || got != bitIdentBaseSHA {
		t.Errorf("the internal embed path left the base: %d cases, sha256 %s — want %d, %s (CTX_BITIDENT_OUT=<file> and cmp against the base)",
			cases, got, bitIdentCases, bitIdentBaseSHA)
	}
}
