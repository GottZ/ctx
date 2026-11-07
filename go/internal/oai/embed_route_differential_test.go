package oai_test

// The route differential of FOLGE-E (gate G5, lead decision 1: the wire
// stays byte-equal except where a booking passes the budget). Every body of
// the W02-09 parse differential's corpus (oai.EmbedRouteDiffCorpus) and the
// gate bodies of the wave (input_ff, model_ff, input_ascii, input_esc,
// input_surr at 20 100, 65 600, 262 144 B and at the cap) go over the real
// route — the strict body cap and the in-flight budget in front of the
// handler with the production defaults (memBudget 256 MiB, memCap 8 MiB) —
// one at a time (n = 1), to both kinds of alias (credentials: no scan;
// public: the scan runs), each with embedding.native_dims (8) and without it
// (FOLGE-E hardening 1: the answer then reserves the ceiling of
// embedding_max_floats and the decode its transport cap), and with body
// logging none, errors and full. Each
// combination is one JSON line of the dump: under "gate" the status, the
// answer body (UUIDs normalized), the headers (without X-Request-ID and
// Date), every upstream body (length and sha256, sorted), the stored llmlog
// row (llmlog.GatewayEntry, as production's Record builds it; times
// normalized, the host left out), the access record (without the queue
// wait) and what the limiter saw (the estimate and how the reservation
// ended); under "info" what may differ by design (the raw row's Request,
// the reservation the request held). Run it against two trees and compare:
//
//	CTX_FOLGEE_DUMP=/path/dump.jsonl go test -run '^TestEmbedRouteDump$' -count=1 ./internal/oai/
//
// Skipped without CTX_FOLGEE_DUMP: a comparison tool, not a gate of the
// default suite. CTX_FOLGEE_DUMP_MAX limits the corpus bodies (a smoke run).
// A tree that sets rdumpExtra (embed_route_dump_classgate_test.go, copied
// into the trees that have the class gate) adds the class gate's bodies to
// the alias classes with native_dims, body logging none.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// rdumpMeter admits everything and logs what it was asked and how each
// reservation ended.
type rdumpMeter struct {
	mu sync.Mutex
	ev []string
}

func (m *rdumpMeter) add(s string) {
	m.mu.Lock()
	m.ev = append(m.ev, s)
	m.mu.Unlock()
}

func (m *rdumpMeter) take() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev := m.ev
	m.ev = nil
	return ev
}

func (m *rdumpMeter) Admit(_ context.Context, _ *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	m.add(fmt.Sprintf("admit alias=%s est=%+v", a.Name, est))
	return rdumpRes{m}, nil
}

type rdumpRes struct{ m *rdumpMeter }

func (r rdumpRes) Settle(u dispatch.Usage)               { r.m.add(fmt.Sprintf("settle %+v", u)) }
func (r rdumpRes) Keep()                                 { r.m.add("keep") }
func (r rdumpRes) Abort()                                { r.m.add("abort") }
func (r rdumpRes) Reject(reason string, _ time.Duration) { r.m.add("reject " + reason) }
func (r rdumpRes) Headers() http.Header                  { return nil }
func (r rdumpRes) Held() int64                           { return 0 }

// rdumpRow is one stored llmlog row as the dump compares it.
type rdumpRow struct {
	ReqLen  int    `json:"req_len"`
	ReqSHA  string `json:"req_sha"`
	ReqHead string `json:"req_head"`
	RespLen int    `json:"resp_len"`
	RespSHA string `json:"resp_sha"`
	Err     string `json:"err"`
	Fields  string `json:"fields"`
	Meta    string `json:"meta"`
}

// rdumpSink is the route's Record: it builds the stored entry as
// production's llmlog.RecordGateway does (GatewayEntry, synchronously) and
// keeps the raw row's Request apart (info: its length and hash).
type rdumpSink struct {
	mu     sync.Mutex
	stored []rdumpRow
	raw    []string
}

var rdumpMs = regexp.MustCompile(`"([a-z_]*ms)":-?[0-9]+`)

func (s *rdumpSink) record(row llmlog.GatewayRow) {
	rawReq := fmt.Sprintf("%d:%s", len(row.Request), rdumpSHA([]byte(row.Request)))
	e := llmlog.GatewayEntry(row)
	errText := ""
	if e.Err != nil {
		errText = e.Err.Error()
	}
	meta, _ := json.Marshal(e.Metadata)
	head := e.RequestUser
	if len(head) > 120 {
		head = head[:120]
	}
	r := rdumpRow{
		ReqLen: len(e.RequestUser), ReqSHA: rdumpSHA([]byte(e.RequestUser)), ReqHead: strconv.Quote(head),
		RespLen: len(e.ResponseContent), RespSHA: rdumpSHA([]byte(e.ResponseContent)), Err: errText,
		Fields: fmt.Sprintf("pipeline=%s model=%s backend=%s trust=%s locality=%s sens=%s attempt=%d tokens=%d/%d consumer=%s apikey=%s class=%s abort=%s",
			e.Pipeline, e.Model, e.BackendName, e.BackendTrust, e.BackendLocality, e.RequiredSensitivity, e.Attempt,
			e.PromptTokens, e.CompletionTokens, e.ConsumerID, e.APIKeyID, e.DispatchClass, e.DispatchAbort),
		Meta: rdumpMs.ReplaceAllString(string(meta), `"$1":0`),
	}
	s.mu.Lock()
	s.stored = append(s.stored, r)
	s.raw = append(s.raw, rawReq)
	s.mu.Unlock()
}

func (s *rdumpSink) take() ([]rdumpRow, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, raw := s.stored, s.raw
	s.stored, s.raw = nil, nil
	return st, raw
}

func rdumpSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// rdumpCapture is what the mount saw of the last request.
type rdumpCapture struct {
	rec  *oai.AccessRecord
	held int64
}

// rdumpMount is embedMemMount (strict body cap, in-flight budget, the
// reservation probe) that also keeps the request's access record.
func rdumpMount(r *chatRig, h http.Handler, c *rdumpCapture) chi.Router {
	rt := chi.NewRouter()
	rt.Route("/v1", func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rec := &oai.AccessRecord{}
				c.rec = rec
				ctx := oai.WithAccessRecord(fedhop.WithIdentity(req.Context(), rigIdentity), rec)
				if ca := r.ca.Load(); ca != nil {
					ctx = context.WithValue(ctx, consumerKey{}, ca)
				}
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		g.Use(oai.Recover)
		g.Use(federation.PeerIngress(federation.PeerIngressConfig{Consumer: r.deps.Consumer, Logger: slog.New(slog.DiscardHandler)}))
		g.Use(handler.MaxBodySizeStrictWith(func() int64 { return r.settings.Load().MaxRequestBytes }, oai.RefuseRequestTooLarge))
		g.Use(oai.InflightBudget(&r.budget, func() oai.Settings { return *r.settings.Load() }))
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req)
				if res := oai.ReservationFrom(req.Context()); res != nil {
					c.held = oai.ReservationHeld(res)
				}
			})
		})
		g.Method(http.MethodPost, "/embeddings", h)
	})
	return rt
}

// rdumpGateBody is a gate body of the wave of at most lim bytes to model:
// "model_ff" a model name of 0xff filling the body; the input forms inputs
// of 32 KiB of their unit (at most 256, as many as fit — the heap gate's
// form, embedInvalidForm), or, when not one fits, one input filling the
// body. Units: input_ff 0xff, input_ascii 'a', input_esc the escape of
// U+0001, input_surr the escape of the lone surrogate D800 (both six bytes,
// a backslash and u and four hex digits).
func rdumpGateBody(form, model string, lim int) []byte {
	var b bytes.Buffer
	if form == "model_ff" {
		pre, post := `{"model":"`, `","input":"x"}`
		b.WriteString(pre + strings.Repeat("\xff", lim-len(pre)-len(post)) + post)
		return b.Bytes()
	}
	bs := "\\"
	unit := map[string]string{"input_ff": "\xff", "input_ascii": "a", "input_esc": bs + "u0001", "input_surr": bs + "ud800"}[form]
	in := `"` + strings.Repeat(unit, (32<<10)/len(unit)) + `"`
	b.WriteString(`{"model":"` + model + `","input":[`)
	i := 0
	for ; i < 256 && b.Len()+len(in)+3 <= lim; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(in)
	}
	if i == 0 {
		k := (lim - b.Len() - len(`""]}`)) / len(unit)
		b.WriteString(`"` + strings.Repeat(unit, k) + `"`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// rdumpItem is one body of the dump.
type rdumpItem struct {
	key  string // corpus:<index> or gate:<form>:<size>
	body []byte
	logs []string // the body loggings of the item; nil: all three
}

// rdumpExtra, when set, adds bodies to model to the dump's alias classes
// with native_dims.
var rdumpExtra func(model string) []rdumpItem

// rdumpNoDims are the dump's aliases without embedding.native_dims: the
// public and the credentials alias of embedScanAliases with the same policy
// less the dimension.
func rdumpNoDims(pub bool) []chatAlias {
	policy := `{"embedding.max_inputs":256,"embedding.sub_batch":256,"embedding.max_input_runes":32768}`
	out := append(embedAliases(),
		chatAlias{"00000000-0000-4000-8000-0000000e4b21", backends.GlobalScope, "embpub0", "embedding", "embed", backends.SensPublic, policy, true},
		chatAlias{"00000000-0000-4000-8000-0000000e4b22", backends.GlobalScope, "embsec0", "embedding", "embed", backends.SensCredentials, policy, true})
	for i := range out {
		if pub && out[i].name == "emb" {
			out[i].sens = backends.SensPublic
		}
	}
	return out
}

var rdumpUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func TestEmbedRouteDump(t *testing.T) {
	path := os.Getenv("CTX_FOLGEE_DUMP")
	if path == "" {
		t.Skip("CTX_FOLGEE_DUMP not set (comparison tool, see the file comment)")
	}
	corpus := oai.EmbedRouteDiffCorpus(t)
	if v, err := strconv.Atoi(os.Getenv("CTX_FOLGEE_DUMP_MAX")); err == nil && v >= 0 && v < len(corpus) {
		corpus = corpus[:v]
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)

	pub := embedScanAliases()
	for i := range pub {
		if pub[i].name == "emb" { // the corpus's embeddings alias, below credentials in the public class
			pub[i].sens = backends.SensPublic
		}
	}
	classes := []struct {
		name, model string
		aliases     []chatAlias
		dims        bool
	}{
		{"cred", "embsec", embedScanAliases(), true},
		{"pub", "embpub", pub, true},
		{"cred0", "embsec0", rdumpNoDims(false), false},
		{"pub0", "embpub0", rdumpNoDims(true), false},
	}
	sizes := []struct {
		name string
		lim  int
	}{{"20100", 20100}, {"65600", 65600}, {"262144", 262144}, {"cap", memCap - 16}}
	forms := []string{"input_ff", "model_ff", "input_ascii", "input_esc", "input_surr"}
	logs := []string{llmlog.BodyLoggingNone, llmlog.BodyLoggingErrors, llmlog.BodyLoggingFull}

	total, start := 0, time.Now()
	for _, cl := range classes {
		items := make([]rdumpItem, 0, len(corpus)+len(forms)*len(sizes))
		for i, b := range corpus {
			items = append(items, rdumpItem{key: fmt.Sprintf("corpus:%d", i), body: b})
		}
		for _, fm := range forms {
			for _, sz := range sizes {
				items = append(items, rdumpItem{key: fmt.Sprintf("gate:%s:%s", fm, sz.name), body: rdumpGateBody(fm, cl.model, sz.lim)})
			}
		}
		if rdumpExtra != nil && cl.dims {
			items = append(items, rdumpExtra(cl.model)...)
		}

		e := newEmbedEngine(t, 8)
		meter := &rdumpMeter{}
		sink := &rdumpSink{}
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withoutCensus,
			withDeps(func(d *oai.Deps) { d.Limiter, d.Record = meter, sink.record }))
		rig.reg.reload(t, cl.aliases)
		rig.setSettings(func(s *oai.Settings) {
			s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = memBudget, memCap, memRespCap
		})
		var c rdumpCapture
		rig.router = rdumpMount(rig, oai.Embeddings(rig.deps), &c)

		for _, it := range items {
			itLogs := logs
			if it.logs != nil {
				itLogs = it.logs
			}
			for _, lg := range itLogs {
				rig.setSettings(func(s *oai.Settings) { s.BodyLogging = lg })
				e.mu.Lock()
				e.bodies, e.calls = nil, nil
				e.mu.Unlock()
				meter.take()
				sink.take()
				c = rdumpCapture{}

				req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(it.body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				rig.router.ServeHTTP(w, req)

				e.mu.Lock()
				ups := make([]string, 0, len(e.bodies))
				for _, ub := range e.bodies {
					ups = append(ups, fmt.Sprintf("%d:%s", len(ub), rdumpSHA([]byte(ub))))
				}
				e.mu.Unlock()
				sort.Strings(ups)
				var hdr []string
				for k, vs := range w.Header() {
					if k == "X-Request-Id" || k == "Date" {
						continue
					}
					hdr = append(hdr, k+": "+strings.Join(vs, " | "))
				}
				sort.Strings(hdr)
				body := rdumpUUID.ReplaceAll(w.Body.Bytes(), []byte("<uuid>"))
				shown := string(body)
				if len(shown) > 2048 {
					shown = shown[:2048] + "…"
				}
				rec := "none"
				if r := c.rec; r != nil {
					rec = fmt.Sprintf("reason=%s op=%s alias=%s stream=%v tokens=%d/%d usage=%s class=%s hop=%d consumer=%s key=%s tenant=%s",
						r.Reason, r.Op, r.Alias, r.Stream, r.PromptTokens, r.CompletionTokens, r.Usage, r.Class, r.Hop, r.ConsumerID, r.ModelKeyID, r.TenantID)
				}
				stored, raw := sink.take()
				line := map[string]any{
					"key": it.key, "alias": cl.name, "log": lg, "bytes": len(it.body),
					"gate": map[string]any{
						"status": w.Code, "body_sha": rdumpSHA(body), "body": shown, "hdr": hdr,
						"upstream": ups, "rows": stored, "access": rec, "meter": meter.take(),
					},
					"info": map[string]any{"row_request": raw, "held": c.held},
				}
				if err := enc.Encode(line); err != nil {
					t.Fatal(err)
				}
				total++
			}
		}
	}
	t.Logf("FOLGE-E G5 dump: %d combinations (%d corpus bodies + %d gate bodies, 4 alias classes, 3 body loggings; class gate bodies %v) in %v to %s",
		total, len(corpus), len(forms)*len(sizes), rdumpExtra != nil, time.Since(start).Round(time.Millisecond), path)
}

// TestEmbedCapBodiesAtTheDefaultBudget is the pin of S-1 (FOLGE-E hardening
// 1): the route dump's gate bodies at the body cap, n = 1, the production
// defaults (budget 256 MiB, cap 8 MiB), body logging none, to all four alias
// classes answer as the route dump of the end state measured — the forms
// 4abba96b served at the cap are served again (84ffc7a8 refused plain text,
// control escapes and 0xff to the public alias, and 0xff to credentials
// without native_dims, 503: the upstream bodies booked at eight times their
// size). The public alias without native_dims stays 503 for plain text
// (269.03 MiB), 0xff (300.91), control escapes (262.38) and lone
// surrogates (261.05) — the reserves of an unknown dimension and the scan
// take most of the budget. Lone surrogates booked 255.84 MiB and were
// served before the route booked every block's retry (FOLGE-E hardening 2,
// L-4: 85 blocks × (32 KiB + 30 KiB) more, booked with the first call);
// they are refused again, as 4abba96b and root 829a45ec refuse them (there
// at the scan booking on top of the whole request share, here after the
// queue — the texts fit before it, the blocks' per-call parts not).
// Outside -short: eight-MiB bodies over the public scan.
func TestEmbedCapBodiesAtTheDefaultBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("8 MiB bodies over the public scan: outside -short (the CI's integration job)")
	}
	want := map[string]int{
		"cred/input_ascii": 200, "pub/input_ascii": 200, "cred0/input_ascii": 200, "pub0/input_ascii": 503,
		"cred/input_esc": 200, "pub/input_esc": 200, "cred0/input_esc": 200, "pub0/input_esc": 503,
		"cred/input_ff": 200, "pub/input_ff": 200, "cred0/input_ff": 200, "pub0/input_ff": 503,
		"cred/input_surr": 200, "pub/input_surr": 200, "cred0/input_surr": 200, "pub0/input_surr": 503,
		"cred/model_ff": 404, "pub/model_ff": 404, "cred0/model_ff": 404, "pub0/model_ff": 404,
	}
	pub := embedScanAliases()
	for i := range pub {
		if pub[i].name == "emb" {
			pub[i].sens = backends.SensPublic
		}
	}
	for _, cl := range []struct {
		name, model string
		aliases     []chatAlias
	}{
		{"cred", "embsec", embedScanAliases()},
		{"pub", "embpub", pub},
		{"cred0", "embsec0", rdumpNoDims(false)},
		{"pub0", "embpub0", rdumpNoDims(true)},
	} {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withoutCensus)
		rig.reg.reload(t, cl.aliases)
		rig.setSettings(func(s *oai.Settings) {
			s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = memBudget, memCap, memRespCap
			s.BodyLogging = llmlog.BodyLoggingNone
		})
		var c rdumpCapture
		rig.router = rdumpMount(rig, oai.Embeddings(rig.deps), &c)
		for _, form := range []string{"input_ascii", "input_esc", "input_ff", "input_surr", "model_ff"} {
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(rdumpGateBody(form, cl.model, memCap-16)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			rig.router.ServeHTTP(w, req)
			if k := cl.name + "/" + form; w.Code != want[k] {
				t.Errorf("%s at the cap: %d, want %d (held %.2f MiB)", k, w.Code, want[k], float64(c.held)/(1<<20))
			}
		}
	}
}
