package oai_test

// The booking of the sensitivity scan at the embeddings route (W02-09
// rebase round 2, part D; FOLGE-P hardening H-8 at the chat route): the
// route scans the body of a request to an alias below credentials
// (federation.ScanRaise, every token of the body decoded into a string and
// matched) and books scanReserveFactor × the body + scanReserveBase of the
// in-flight budget before the scan runs — at credentials the scan returns at
// once and nothing is booked. The measurement that answered whether it must
// (bau report W02-09 §Rebase-Runde 2, Teil D) is embed_scan_mem_test.go.

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// The scan pin's aliases: native dimension 8, one below credentials and its
// twin at credentials — names of one length, so the twins' bodies are of one
// length too.
const (
	idEmbScanPub  = "00000000-0000-4000-8000-0000000e4b11"
	idEmbScanCred = "00000000-0000-4000-8000-0000000e4b12"
)

func embedScanAliases() []chatAlias {
	policy := `{"embedding.native_dims":8,"embedding.max_inputs":256,"embedding.sub_batch":256,"embedding.max_input_runes":32768}`
	return append(embedAliases(),
		chatAlias{idEmbScanPub, backends.GlobalScope, "embpub", "embedding", "embed", backends.SensPublic, policy, true},
		chatAlias{idEmbScanCred, backends.GlobalScope, "embsec", "embedding", "embed", backends.SensCredentials, policy, true})
}

// scanProbe wraps the rig's scan: it reports what the in-flight budget held
// when the scan ran and counts the scans.
type scanProbe struct {
	held  atomic.Int64
	scans atomic.Int64
}

func withScanProbe(p *scanProbe, rig **chatRig) rigOption {
	return withDeps(func(d *oai.Deps) {
		inner := d.Federation.Scan
		d.Federation.Scan = func(required backends.Sensitivity, body []byte) backends.Sensitivity {
			p.scans.Add(1)
			p.held.Store((*rig).budget.InUse())
			return inner(required, body)
		}
	})
}

// TestEmbedScanIsBookedBeforeItRuns: the same body to an alias below
// credentials and to its twin at credentials — at the scan the first holds
// exactly oai.ScanReserveFactor × the body + oai.ScanReserveBase more of the
// budget; a budget one byte short of that booking refuses the first with
// 503 server_overloaded before the scan and before any wire call, and serves
// the twin.
func TestEmbedScanIsBookedBeforeItRuns(t *testing.T) {
	inputs := make([]string, 16)
	for i := range inputs {
		inputs[i] = strings.Repeat("apikey:x ", 100)
	}
	e := newEmbedEngine(t, 8)
	var p scanProbe
	var rig *chatRig
	rig = newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1, withScanProbe(&p, &rig))
	rig.reg.reload(t, embedScanAliases())
	at := func(model string) (int64, chatAnswer) {
		p.held.Store(-1)
		a := rig.postEmbed(t, embedBody(model, inputs))
		return p.held.Load(), a
	}
	body := embedBody("embpub", inputs)
	booking := int64(oai.ScanReserveFactor)*int64(len(body)) + oai.ScanReserveBase
	cred, ac := at("embsec")
	pub, ap := at("embpub")
	t.Logf("W0209 scan booking: body %d B; held at the scan: credentials %d, public %d, difference %d (want %d)", len(body), cred, pub, pub-cred, booking)
	if ac.status != http.StatusOK || ap.status != http.StatusOK || cred <= 0 {
		t.Fatalf("answers %d / %d, held at the scan %d — want both served", ac.status, ap.status, cred)
	}
	if pub-cred != booking {
		t.Errorf("at the scan the public request holds %d B more than its twin at credentials, want the scan's booking %d B (%d × %d B + %d)",
			pub-cred, booking, oai.ScanReserveFactor, len(body), oai.ScanReserveBase)
	}
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = pub - 1 })
	scans, hits := p.scans.Load(), e.hits.Load()
	a := rig.postEmbed(t, body)
	code, _, _ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
		t.Errorf("a budget one byte short of the scan's booking: %d %s, want 503 server_overloaded", a.status, code)
	}
	if p.scans.Load() != scans || e.hits.Load() != hits {
		t.Errorf("scans %d, wire hits %d after the refusal — want none (refused before the scan)", p.scans.Load()-scans, e.hits.Load()-hits)
	}
	if _, a := at("embsec"); a.status != http.StatusOK {
		t.Errorf("the twin at credentials under the same budget: %d, want 200 (no booking at credentials)", a.status)
	}
}
