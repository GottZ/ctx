package oai_test

// RC-D cluster C8, budget package (S2): gateway.scan_max_request_bytes. A
// request to an alias below credentials whose body is longer than the
// effective scan cap (the setting, or gateway.max_request_bytes when that is
// smaller — oai.EffectiveScanCap) gets 413 request_too_large before the
// scan's booking, the scan, the meter's admission and any wire call, and the
// refusal ledger counts it as capacity/request_too_large. Its twin at
// credentials (not scanned) takes the same body; a body exactly at the cap
// is scanned and served.

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// scanCapTest is the cap the route pins run under: small enough that a body
// past it is cheap, larger than every route's smallest valid body.
const scanCapTest = 4096

// scanCapHeld wraps a route handler: held is the request's reservation at
// the handler's end (what the request booked in all — the reservation only
// grows).
func scanCapHeld(h http.Handler, held *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if res := oai.ReservationFrom(r.Context()); res != nil {
			held.Store(oai.ReservationHeld(res))
		}
	})
}

// scanCapRoute is one inference route under the scan cap pins.
type scanCapRoute struct {
	name string
	// mount builds the rig with the scan probe and the held probe; hits
	// counts the engine's wire calls.
	mount func(t *testing.T, p *scanProbe, held *atomic.Int64) (rig *chatRig, hits func() int64)
	post  func(t *testing.T, rig *chatRig, body string) chatAnswer
	// body is a request to model of exactly size bytes.
	body      func(t *testing.T, model string, size int) string
	pub, cred string
}

// padTo fills the one run of 'a' in tmpl (marked by %s) so the body is
// exactly size bytes.
func padTo(t *testing.T, tmpl string, size int) string {
	t.Helper()
	pad := size - (len(tmpl) - len("%s"))
	if pad < 1 {
		t.Fatalf("body template %q is longer than %d bytes", tmpl, size)
	}
	return strings.Replace(tmpl, "%s", strings.Repeat("a", pad), 1)
}

func scanCapRoutes() []scanCapRoute {
	return []scanCapRoute{
		{
			name: "chat", pub: "pub", cred: "chat",
			mount: func(t *testing.T, p *scanProbe, held *atomic.Int64) (*chatRig, func() int64) {
				u := newUpstream(t, answerSGLang)
				var rig *chatRig
				rig = newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withScanProbe(p, &rig))
				rig.router = rig.mount(scanCapHeld(oai.ChatCompletions(rig.deps), held))
				return rig, u.hits.Load
			},
			post: func(t *testing.T, rig *chatRig, body string) chatAnswer { return rig.post(t, body) },
			body: func(t *testing.T, model string, size int) string {
				return padTo(t, `{"model":"`+model+`","messages":[{"role":"user","content":"%s"}]}`, size)
			},
		},
		{
			name: "embeddings", pub: "embpub", cred: "embsec",
			mount: func(t *testing.T, p *scanProbe, held *atomic.Int64) (*chatRig, func() int64) {
				e := newEmbedEngine(t, 8)
				var rig *chatRig
				rig = newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withScanProbe(p, &rig))
				rig.reg.reload(t, embedScanAliases())
				rig.router = rig.mountEmbed(scanCapHeld(oai.Embeddings(rig.deps), held))
				return rig, e.hits.Load
			},
			post: func(t *testing.T, rig *chatRig, body string) chatAnswer { return rig.postEmbed(t, body) },
			body: func(t *testing.T, model string, size int) string {
				return padTo(t, `{"model":"`+model+`","input":["%s"]}`, size)
			},
		},
		{
			name: "rerank", pub: "rrpub", cred: "rrwide",
			mount: func(t *testing.T, p *scanProbe, held *atomic.Int64) (*chatRig, func() int64) {
				e := newRerankEngine(t)
				var rig *chatRig
				rig = newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withScanProbe(p, &rig))
				rig.router = rig.mountRerank(scanCapHeld(oai.Rerank(rig.deps, oai.RerankJina), held), oai.Rerank(rig.deps, oai.RerankCohere))
				return rig, e.hits.Load
			},
			post: func(t *testing.T, rig *chatRig, body string) chatAnswer { return rig.postRerank(t, "/v1/rerank", body) },
			body: func(t *testing.T, model string, size int) string {
				return padTo(t, `{"model":"`+model+`","query":"which document is relevant","documents":["%s"]}`, size)
			},
		},
	}
}

// TestScanCapRefusesBeforeTheScan: per route, a body one byte past the
// scan cap to the alias below credentials is 413 request_too_large, declared
// capacity/request_too_large, with no scan, no scan booking (the request
// holds less than the at-cap request held at its scan, by at least the
// scan's factor share), no meter admission and no wire call; the same body
// to the credentials twin is served, and a body exactly at the cap to the
// public alias is scanned and served.
func TestScanCapRefusesBeforeTheScan(t *testing.T) {
	for _, rt := range scanCapRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			var p scanProbe
			var held atomic.Int64
			rig, hits := rt.mount(t, &p, &held)
			rig.setSettings(func(s *oai.Settings) { s.ScanMaxRequestBytes = scanCapTest })

			// At the cap: scanned and served — the booking at the scan is the
			// reference the refusal below must stay under.
			p.held.Store(-1)
			at := rt.body(t, rt.pub, scanCapTest)
			if a := rt.post(t, rig, at); a.status != http.StatusOK {
				t.Fatalf("public body at the scan cap (%d B): %d %.300s, want 200", len(at), a.status, a.body)
			}
			_ = censusRecord(t, rig)
			if n := p.scans.Load(); n != 1 {
				t.Fatalf("public body at the scan cap: %d scans, want 1", n)
			}
			heldAtScan := p.held.Load()

			// One byte past the cap: 413 before the scan.
			p.scans.Store(0)
			hits0, admits0 := hits(), rig.limiter.admits.Load()
			over := rt.body(t, rt.pub, scanCapTest+1)
			a := rt.post(t, rig, over)
			rec := censusRecord(t, rig)
			if a.status != http.StatusRequestEntityTooLarge {
				t.Fatalf("public body past the scan cap (%d B): %d %.300s, want 413", len(over), a.status, a.body)
			}
			if code, _, _ := a.envelope(t); code != oai.CodeRequestTooLarge {
				t.Errorf("code %q, want %q", code, oai.CodeRequestTooLarge)
			}
			if rej, ok := rec.Rejection(); !ok || rej.Reason != keyquota.RejectCapacity || rej.Detail != keyquota.DetailRequestTooLarge {
				t.Errorf("declared %+v (%v), want capacity/request_too_large", rej, ok)
			}
			if n := p.scans.Load(); n != 0 {
				t.Errorf("%d scans, want 0 (the refusal comes before the scan)", n)
			}
			if n := hits() - hits0; n != 0 {
				t.Errorf("%d wire calls, want 0", n)
			}
			if n := rig.limiter.admits.Load() - admits0; n != 0 {
				t.Errorf("%d meter admissions, want 0", n)
			}
			if h, limit := held.Load(), heldAtScan-oai.ScanReserveFactor*scanCapTest; h >= limit {
				t.Errorf("refused request held %d B, want < %d (the at-cap request's %d B at its scan less the scan's factor share): the scan was booked", h, limit, heldAtScan)
			}
			for _, w := range []string{"4096 bytes", "credentials"} {
				if !strings.Contains(string(a.body), w) {
					t.Errorf("message %s does not name %q", a.body, w)
				}
			}

			// The same body to the credentials twin: served, not scanned.
			p.scans.Store(0)
			twin := rt.body(t, rt.cred, scanCapTest+1)
			if a := rt.post(t, rig, twin); a.status != http.StatusOK {
				t.Fatalf("credentials body past the scan cap (%d B): %d %.300s, want 200", len(twin), a.status, a.body)
			}
			_ = censusRecord(t, rig)
		})
	}
}

// TestEffectiveScanCap: the setting where it is below the body cap, the body
// cap where the setting is above it (V58: legal, without effect) and where
// it is unset (a Settings without the key).
func TestEffectiveScanCap(t *testing.T) {
	for _, c := range []struct{ scan, body, want int64 }{
		{2 << 20, 8 << 20, 2 << 20},
		{8 << 20, 8 << 20, 8 << 20},
		{16 << 20, 8 << 20, 8 << 20},
		{0, 8 << 20, 8 << 20},
		{1, 8 << 20, 1},
	} {
		if got := oai.EffectiveScanCap(c.scan, c.body); got != c.want {
			t.Errorf("EffectiveScanCap(%d, %d) = %d, want %d", c.scan, c.body, got, c.want)
		}
	}
}
