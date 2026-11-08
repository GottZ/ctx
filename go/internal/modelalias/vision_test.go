package modelalias

// FOLGE-Z Z2 gates: the effective vision of an alias (policy wins when set,
// else the chain's probe answer, unknown = not capable), the door's reading
// of a loaded alias, and the registry resolving it at every reload.

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// fakeView answers ImageCapable from a fixed set of "role|sens|scope" keys
// and records every question.
type fakeView struct {
	capable map[string]bool
	asked   []string
}

func (v *fakeView) ImageCapable(role string, sens backends.Sensitivity, scope string) bool {
	k := role + "|" + string(sens) + "|" + scope
	v.asked = append(v.asked, k)
	return v.capable[k]
}

func visionPolicy(t *testing.T, raw string) Policy {
	t.Helper()
	p, err := ParsePolicy(KindChat, []byte(raw))
	if err != nil {
		t.Fatalf("policy %s: %v", raw, err)
	}
	return p
}

func TestEffectiveVision(t *testing.T) {
	capable := &fakeView{capable: map[string]bool{"chat|credentials|_global": true}}
	incapable := &fakeView{}
	for _, tc := range []struct {
		name   string
		policy string
		view   VisionView
		want   bool
		source VisionSource
	}{
		{"unset, chain capable", `{}`, capable, true, VisionBackend},
		{"unset, chain not capable", `{}`, incapable, false, VisionNone},
		{"unset, no view", `{}`, nil, false, VisionNone},
		{"policy true beats an incapable chain", `{"vision":true}`, incapable, true, VisionPolicy},
		{"policy false beats a capable chain", `{"vision":false}`, capable, false, VisionPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, src := EffectiveVision(visionPolicy(t, tc.policy), "chat", backends.SensCredentials, backends.GlobalScope, tc.view)
			if got != tc.want || src != tc.source {
				t.Errorf("Z2 EffectiveVision = (%v, %q), want (%v, %q)", got, src, tc.want, tc.source)
			}
		})
	}
	// The view is asked for exactly the alias's role, floor and scope.
	v := &fakeView{}
	EffectiveVision(visionPolicy(t, `{}`), "synthesis", backends.SensPersonal, "tenant-a", v)
	if len(v.asked) != 1 || v.asked[0] != "synthesis|personal|tenant-a" {
		t.Errorf("Z2 EffectiveVision asked %q, want exactly [synthesis|personal|tenant-a]", v.asked)
	}
}

func TestDoorVision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		a      Alias
		want   bool
		source VisionSource
	}{
		{"policy true, nothing resolved", Alias{Policy: visionPolicy(t, `{"vision":true}`)}, true, VisionPolicy},
		{"policy false over a resolved backend true", Alias{Policy: visionPolicy(t, `{"vision":false}`), EffectiveVision: true, VisionSource: VisionBackend}, false, VisionPolicy},
		{"unset, resolved from the backend", Alias{EffectiveVision: true, VisionSource: VisionBackend}, true, VisionBackend},
		{"unset, resolved none", Alias{VisionSource: VisionNone}, false, VisionNone},
		{"built outside Reload", Alias{}, false, VisionNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, src := DoorVision(tc.a)
			if got != tc.want || src != tc.source {
				t.Errorf("Z2 DoorVision = (%v, %q), want (%v, %q)", got, src, tc.want, tc.source)
			}
		})
	}
}

// TestRegistryResolvesEffectiveVision: every Reload resolves each alias
// against the installed view; a later reload sees the view's new answer
// (the pool's change hook triggers exactly such a reload).
func TestRegistryResolvesEffectiveVision(t *testing.T) {
	r, mock, _ := newTestRegistry(t)
	view := &fakeView{capable: map[string]bool{}}
	r.SetVisionView(view)
	load := func() {
		t.Helper()
		mock.ExpectQuery(loadQuery).WillReturnRows(mockRows(
			row{id: "1", scope: backends.GlobalScope, name: "chat", kind: "chat", policy: `{}`, enabled: true},
			row{id: "2", scope: backends.GlobalScope, name: "vis", kind: "chat", policy: `{"vision":true}`, enabled: true},
			row{id: "3", scope: backends.GlobalScope, name: "novis", kind: "chat", policy: `{"vision":false}`, enabled: true},
		))
		if err := r.Reload(context.Background()); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}
	check := func(stage, name string, want bool, src VisionSource) {
		t.Helper()
		a, ok := Lookup(r.Snapshot(), name, backends.GlobalScope)
		if !ok {
			t.Fatalf("%s: alias %s not offered", stage, name)
		}
		if a.EffectiveVision != want || a.VisionSource != src {
			t.Errorf("Z2 registry %s: %s effective_vision=%v source=%q, want %v/%q", stage, name, a.EffectiveVision, a.VisionSource, want, src)
		}
	}
	load()
	check("chain not capable", "chat", false, VisionNone)
	check("chain not capable", "vis", true, VisionPolicy)
	check("chain not capable", "novis", false, VisionPolicy)
	view.capable["chat|credentials|_global"] = true
	load()
	check("chain capable", "chat", true, VisionBackend)
	check("chain capable", "novis", false, VisionPolicy)
}
