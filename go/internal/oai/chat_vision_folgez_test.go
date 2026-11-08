package oai_test

// FOLGE-Z Z2 door gates: an alias without policy.vision accepts images when
// its chain reports an image-capable row (no extra step), refuses them when
// none does — naming the alias and the cause — and policy.vision=false stays
// a hard refusal that names the policy. Code and param are unchanged
// (unsupported_value, messages[i].content[j].image_url.url).

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

const idChatNoVision = "00000000-0000-4000-8000-0000000c4a7f"

// doorView answers ImageCapable with a fixed value.
type doorView bool

func (v doorView) ImageCapable(string, backends.Sensitivity, string) bool { return bool(v) }

func visionDoorAliases() []chatAlias {
	return append(chatAliases(),
		chatAlias{idChatNoVision, backends.GlobalScope, "novis", "chat", "chat", backends.SensCredentials, `{"vision":false}`, true})
}

func imageBody(model string) string {
	img := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":%q}}]}]}`, model, img)
}

func errorMessage(t *testing.T, a chatAnswer) string {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(a.body, &e); err != nil {
		t.Fatalf("answer %d is no envelope: %q", a.status, a.body)
	}
	return e.Error.Message
}

func TestDoorVisionFromTheChain(t *testing.T) {
	const param = "messages[0].content[1].image_url.url"
	t.Run("unset policy, capable chain: served", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang, withDeps(func(d *oai.Deps) { d.RowVision = func(*backends.Backend) bool { return true } }))
		rig.reg.reg.SetVisionView(doorView(true))
		rig.reg.reload(t, visionDoorAliases())
		before := u.hits.Load()
		a := rig.post(t, imageBody("chat"))
		if a.status != http.StatusOK {
			t.Fatalf("Z2 door: image on alias chat (policy.vision unset, chain capable) answered %d %s, want 200", a.status, a.body)
		}
		if hits := u.hits.Load() - before; hits != 1 {
			t.Errorf("Z2 door: %d upstream hits, want 1", hits)
		}
	})
	t.Run("unset policy, incapable chain: refused naming the alias", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang)
		rig.reg.reg.SetVisionView(doorView(false))
		rig.reg.reload(t, visionDoorAliases())
		before := u.hits.Load()
		a := rig.post(t, imageBody("chat"))
		pgRefused(t, a, u, before, oai.CodeUnsupportedValue, param)
		if got, want := errorMessage(t, a), "Unsupported value for "+param+": this model does not accept images (no image-capable backend in the chain of alias chat)."; got != want {
			t.Errorf("Z2 door message %q, want %q", got, want)
		}
	})
	t.Run("policy.vision=false beats a capable chain", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang)
		rig.reg.reg.SetVisionView(doorView(true))
		rig.reg.reload(t, visionDoorAliases())
		before := u.hits.Load()
		a := rig.post(t, imageBody("novis"))
		pgRefused(t, a, u, before, oai.CodeUnsupportedValue, param)
		if got, want := errorMessage(t, a), "Unsupported value for "+param+": this model does not accept images (policy.vision=false)."; got != want {
			t.Errorf("Z2 door message %q, want %q", got, want)
		}
	})
	t.Run("policy.vision=true beats an incapable chain", func(t *testing.T) {
		rig, u := pgRig(t, answerSGLang)
		rig.reg.reg.SetVisionView(doorView(false))
		rig.reg.reload(t, visionDoorAliases())
		before := u.hits.Load()
		if a := rig.post(t, imageBody("vis")); a.status != http.StatusOK {
			t.Fatalf("Z2 door: image on alias vis (policy.vision=true) answered %d %s, want 200", a.status, a.body)
		}
		if hits := u.hits.Load() - before; hits != 1 {
			t.Errorf("Z2 door: %d upstream hits, want 1", hits)
		}
	})
}

// TestDoorVisionRoutesImagesToCapableRows (Fremd-Review 1 R1-1/R1-3): on an
// alias whose vision comes from the backends, an image request walks only
// the rows of ITS chain that reported vision=true — a higher-priority text
// row is skipped, not tried first; a text request keeps the whole chain;
// a request chain without a capable row (narrower than the alias's: the
// consumer's sensitivity, the export rule) is refused like the door, before
// any upstream contact; policy.vision=true forces images through unfiltered.
func TestDoorVisionRoutesImagesToCapableRows(t *testing.T) {
	const param = "messages[0].content[1].image_url.url"
	text, vision := newUpstream(t, answerSGLang), newUpstream(t, answerSGLang)
	rows := []backends.Backend{text.row("text-row", backends.TrustFull, 20), vision.row("vision-row", backends.TrustFull, 10)}
	capable := func(names ...string) rigOption {
		return withDeps(func(d *oai.Deps) {
			d.RowVision = func(b *backends.Backend) bool {
				for _, n := range names {
					if b.Name == n {
						return true
					}
				}
				return false
			}
		})
	}
	hits := func() (int64, int64) { return text.hits.Load(), vision.hits.Load() }

	t.Run("image skips the higher-priority text row", func(t *testing.T) {
		rig := newChatRig(t, rows, 4, 1, capable("vision-row"))
		rig.reg.reg.SetVisionView(doorView(true))
		rig.reg.reload(t, visionDoorAliases())
		t0, v0 := hits()
		if a := rig.post(t, imageBody("chat")); a.status != http.StatusOK {
			t.Fatalf("R1-3: image on chat answered %d %s, want 200", a.status, a.body)
		}
		if t1, v1 := hits(); t1-t0 != 0 || v1-v0 != 1 {
			t.Errorf("R1-3: upstream hits text %d vision %d, want 0/1 — the image went to a row that never reported vision", t1-t0, v1-v0)
		}
	})
	t.Run("text keeps the whole chain", func(t *testing.T) {
		rig := newChatRig(t, rows, 4, 1, capable("vision-row"))
		rig.reg.reg.SetVisionView(doorView(true))
		rig.reg.reload(t, visionDoorAliases())
		t0, v0 := hits()
		if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
			t.Fatalf("R1-3: text on chat answered %d %s, want 200", a.status, a.body)
		}
		if t1, v1 := hits(); t1-t0 != 1 || v1-v0 != 0 {
			t.Errorf("R1-3: text upstream hits text %d vision %d, want 1/0 (priority order unchanged)", t1-t0, v1-v0)
		}
	})
	t.Run("no capable row in the request's chain: refused before any upstream", func(t *testing.T) {
		rig := newChatRig(t, rows, 4, 1, capable())
		rig.reg.reg.SetVisionView(doorView(true))
		rig.reg.reload(t, visionDoorAliases())
		t0, v0 := hits()
		a := rig.post(t, imageBody("chat"))
		if a.status != http.StatusBadRequest {
			t.Fatalf("R1-1: answered %d %s, want 400", a.status, a.body)
		}
		if code, p, _ := a.envelope(t); code != oai.CodeUnsupportedValue || p != param {
			t.Errorf("R1-1: code %q param %q, want %q %q", code, p, oai.CodeUnsupportedValue, param)
		}
		if got, want := errorMessage(t, a), "Unsupported value for "+param+": this model does not accept images (no image-capable backend in the chain of alias chat)."; got != want {
			t.Errorf("R1-1: message %q, want %q", got, want)
		}
		if t1, v1 := hits(); t1 != t0 || v1 != v0 {
			t.Errorf("R1-1: upstream contacted for a refused image request")
		}
	})
	t.Run("policy.vision=true is not filtered", func(t *testing.T) {
		rig := newChatRig(t, rows, 4, 1, capable())
		rig.reg.reg.SetVisionView(doorView(false))
		rig.reg.reload(t, visionDoorAliases())
		t0, v0 := hits()
		if a := rig.post(t, imageBody("vis")); a.status != http.StatusOK {
			t.Fatalf("R1-3: image on vis (policy true) answered %d %s, want 200", a.status, a.body)
		}
		if t1, v1 := hits(); t1-t0 != 1 || v1-v0 != 0 {
			t.Errorf("R1-3: policy-forced image hits text %d vision %d, want 1/0 (the operator asserted it)", t1-t0, v1-v0)
		}
	})
}

// TestDoorVisionAfterTheEgressGate (PR #178 CodeRabbit 2): the image filter
// judges the rows the request may actually reach — after the tenant quota
// and egress gate. A local text row plus an external vision row the
// consumer's egress policy drops is "no image-capable backend", the door's
// 400 with the cause, not 503 no_eligible_backend.
func TestDoorVisionAfterTheEgressGate(t *testing.T) {
	const param = "messages[0].content[1].image_url.url"
	local, ext := newUpstream(t, answerSGLang), newUpstream(t, answerOpenRouter)
	rows := []backends.Backend{local.row("local-text", backends.TrustFull, 20), externalRow(ext, "ext-vision", backends.ProviderOpenRouter, 10)}
	rig := newChatRig(t, rows, 4, 1, withEgress(backends.EgressPolicy{}),
		withDeps(func(d *oai.Deps) { d.RowVision = func(b *backends.Backend) bool { return b.Name == "ext-vision" } }))
	rig.reg.reg.SetVisionView(doorView(true))
	rig.reg.reload(t, visionDoorAliases())
	l0, e0 := local.hits.Load(), ext.hits.Load()
	a := rig.post(t, imageBody("chat"))
	if a.status != http.StatusBadRequest {
		t.Fatalf("CR-2: answered %d %s, want 400 unsupported_value (the only image-capable row is egress-denied)", a.status, a.body)
	}
	if code, p, _ := a.envelope(t); code != oai.CodeUnsupportedValue || p != param {
		t.Errorf("CR-2: code %q param %q, want %q %q", code, p, oai.CodeUnsupportedValue, param)
	}
	if got, want := errorMessage(t, a), "Unsupported value for "+param+": this model does not accept images (no image-capable backend in the chain of alias chat)."; got != want {
		t.Errorf("CR-2: message %q, want %q", got, want)
	}
	if local.hits.Load() != l0 || ext.hits.Load() != e0 {
		t.Errorf("CR-2: an upstream was contacted for a refused image request")
	}
	// A text request on the same chain is served by the local row.
	if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
		t.Errorf("CR-2: text request answered %d %s, want 200 from the local row", a.status, a.body)
	}
}
