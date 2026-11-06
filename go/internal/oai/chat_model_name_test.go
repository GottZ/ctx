package oai_test

// Hardening 5, B-1 (Copilot on PR #155): the model name is the one CTX value
// the parse decodes into a string, and only while its raw form can still be
// an alias name. A longer one — plain, invalid UTF-8 (the decode made each
// byte a U+FFFD of 3 bytes), escaped — is not decoded and answers exactly
// like an unknown short name: the same status, headers and body (no oracle,
// docs/api.md), the same access record, the same one grant probe. The
// longest alias name, every character escaped, still resolves.

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

const idLongName = "00000000-0000-4000-8000-0000000c4a7f"

// longestAliasName is an alias name of the full length migration 162 allows
// (^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$).
var longestAliasName = strings.Repeat("a", 128)

func nameBody(raw string) string {
	return `{"model":"` + raw + `","messages":[{"role":"user","content":"hi"}]}`
}

// overlongModels are raw model values past what an alias name can be in JSON
// (each of its at most 128 characters written as a 6-byte \u escape).
func overlongModels() []struct{ name, raw string } {
	esc := strings.Repeat(`\u0061`, 128)
	return []struct{ name, raw string }{
		{"one past the bound, escaped", esc + "a"},
		{"one escape past the bound", esc + `a`},
		{"1 MiB plain", strings.Repeat("a", 1<<20)},
		{"1 MiB invalid UTF-8", strings.Repeat("\xff", 1<<20)},
		{"1 MiB escapes", strings.Repeat(`\u00e9`, (1<<20)/6)},
	}
}

// normalized is an answer without the headers that differ per request.
func normalized(a chatAnswer) string {
	h := a.header.Clone()
	h.Del("X-Request-Id")
	h.Del("Date")
	keys := slices.Collect(maps.Keys(h))
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", a.status)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %q\n", k, h[k])
	}
	b.Write(a.body)
	return b.String()
}

func TestOverlongModelAnswersLikeAnUnknownModel(t *testing.T) {
	var dump strings.Builder
	for _, allModels := range []bool{true, false} {
		t.Run(fmt.Sprintf("all_models=%v", allModels), func(t *testing.T) {
			var mu sync.Mutex
			var probes []string
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) {
				inner := d.HasGrant
				d.HasGrant = func(ctx context.Context, consumerID, aliasID string) (bool, error) {
					mu.Lock()
					probes = append(probes, aliasID)
					mu.Unlock()
					return inner(ctx, consumerID, aliasID)
				}
			}))
			rig.reg.reload(t, append(chatAliases(), chatAlias{idLongName, backends.GlobalScope, longestAliasName, "chat", "chat", backends.SensCredentials, `{}`, true}))
			ca := urgentApp()
			ca.AllModels = allModels
			rig.ca.Store(ca)
			rig.setGrant(idLongName, true)
			take := func() ([]string, *oai.AccessRecord) {
				mu.Lock()
				defer mu.Unlock()
				p := probes
				probes = nil
				select {
				case rec := <-rig.recs:
					return p, rec
				case <-time.After(5 * time.Second):
					t.Fatal("no access record")
				}
				return nil, nil
			}

			ref := rig.post(t, nameBody("nosuchmodel"))
			refProbes, refRec := take()
			if code, _, _ := ref.envelope(t); ref.status != http.StatusNotFound || code != oai.CodeModelNotFound {
				t.Fatalf("unknown model: %d %s, want 404 model_not_found", ref.status, ref.body)
			}
			fmt.Fprintf(&dump, "all_models=%v reference probes=%v\n%s\n", allModels, refProbes, normalized(ref))
			for _, m := range overlongModels() {
				a := rig.post(t, nameBody(m.raw))
				p, rec := take()
				if got, want := normalized(a), normalized(ref); got != want {
					t.Errorf("%s: answer\n%s\nwant the unknown model's\n%s", m.name, got, want)
				}
				if !slices.Equal(p, refProbes) {
					t.Errorf("%s: grant probes %v, want the unknown model's %v", m.name, p, refProbes)
				}
				if !reflect.DeepEqual(rec, refRec) {
					t.Errorf("%s: access record %+v, want the unknown model's %+v", m.name, *rec, *refRec)
				}
				fmt.Fprintf(&dump, "all_models=%v %s probes=%v\n%s\n", allModels, m.name, p, normalized(a))
			}
			for _, raw := range []string{longestAliasName, strings.Repeat(`\u0061`, 128)} {
				a := rig.post(t, nameBody(raw))
				take()
				if a.status != http.StatusOK {
					t.Errorf("the longest alias name, %d raw bytes: %d %s, want 200", len(raw), a.status, a.body)
				}
				fmt.Fprintf(&dump, "all_models=%v longest name raw=%d status=%d\n", allModels, len(raw), a.status)
			}
		})
	}
	// CTX_FOLGEP_H5_DUMP=<file>: the answers for a byte comparison of two
	// builds (the base against hardening 5, report §B-1).
	if p := os.Getenv("CTX_FOLGEP_H5_DUMP"); p != "" {
		if err := os.WriteFile(p, []byte(dump.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOverlongModelTakesNoLessTime is the time side of B-1 (measurement test,
// CTX_FOLGEP_TIMING=1): a request whose model is past the bound is answered
// no faster than one for an unknown short name of the same body size — both
// read and check the whole body, neither reaches the policy parse.
func TestOverlongModelTakesNoLessTime(t *testing.T) {
	if os.Getenv("CTX_FOLGEP_TIMING") == "" {
		t.Skip("CTX_FOLGEP_TIMING not set (measurement test)")
	}
	rig := newChatRig(t, nil, 4, 1)
	size := 8<<20 - 64
	pre, suf := `{"model":"nosuchmodel","messages":[{"role":"user","content":"`, `"}]}`
	forms := []struct{ name, body string }{
		{"unknown short name", pre + strings.Repeat("a", size-len(pre)-len(suf)) + suf},
		{"overlong plain", nameBody(strings.Repeat("a", size-len(nameBody(""))))},
		{"overlong invalid UTF-8", nameBody(strings.Repeat("\xff", size-len(nameBody(""))))},
		{"overlong escapes", nameBody(strings.Repeat(`\u00e9`, (size-len(nameBody("")))/6))},
	}
	const rounds = 41
	times := make([][]time.Duration, len(forms))
	for range rounds {
		for i, f := range forms {
			t0 := time.Now()
			a := rig.post(t, f.body)
			times[i] = append(times[i], time.Since(t0))
			if a.status != http.StatusNotFound {
				t.Fatalf("%s: %d %s", f.name, a.status, a.body)
			}
		}
	}
	med := make([]time.Duration, len(forms))
	for i, f := range forms {
		slices.Sort(times[i])
		med[i] = times[i][rounds/2]
		t.Logf("FOLGEP timing form=%q bytes=%d median=%v p10=%v p90=%v ratio_to_unknown=%.2f", f.name, len(f.body), med[i],
			times[i][rounds/10], times[i][rounds*9/10], float64(med[i])/float64(med[0]))
	}
}
