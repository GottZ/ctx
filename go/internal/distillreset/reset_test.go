package distillreset

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
)

// TestInstanceKindConstantParity hält die eigene Konstante an der des
// Sweep-Treibers fest. Der Import ist hier folgenlos — im Produktivpfad des
// Werkzeugs zöge er rrf, goldset und evalscore mit.
func TestInstanceKindConstantParity(t *testing.T) {
	if InstanceKindMeasureCopy != armsweep.InstanceKindMeasureCopy {
		t.Fatalf("InstanceKindMeasureCopy = %q, armsweep = %q — die beiden Gates lesen denselben Schlüssel",
			InstanceKindMeasureCopy, armsweep.InstanceKindMeasureCopy)
	}
}

// measureCopy ist die kleinste vollständige Identität.
func measureCopy() Identity {
	return Identity{
		InstanceKind: InstanceKindMeasureCopy,
		Category:     "session-insights",
		Scope:        "private",
		ToType:       "insight",
	}
}

// TestGatesRunBeforeAnyDatabaseContact fährt jedes Vor-Gate mit einem NIL-Pool.
// Das ist die Probe selbst: käme die Datenbank vor dem Gate, würde der Test
// nicht fehlschlagen, sondern abstürzen.
func TestGatesRunBeforeAnyDatabaseContact(t *testing.T) {
	ctx := context.Background()
	mut := func(f func(*Identity)) Identity {
		id := measureCopy()
		f(&id)
		return id
	}
	cases := []struct {
		name string
		id   Identity
		opts Options
		want error
	}{
		{"Live-Instanz", mut(func(i *Identity) { i.InstanceKind = "live" }),
			Options{FromType: "x", Apply: true}, ErrNotMeasureCopy},
		{"Instanz ohne Etikett", mut(func(i *Identity) { i.InstanceKind = "" }),
			Options{FromType: "x", Apply: true}, ErrNotMeasureCopy},
		{"leere Kategorie", mut(func(i *Identity) { i.Category = "" }),
			Options{FromType: "x", Apply: true}, ErrIdentity},
		{"leerer Blocktyp", mut(func(i *Identity) { i.ToType = "" }),
			Options{FromType: "x", Apply: true}, ErrIdentity},
		{"kein Scope aufgelöst", mut(func(i *Identity) { i.Scope = "" }),
			Options{FromType: "x", Apply: true}, ErrIdentity},
		{"kein Quelltyp", measureCopy(), Options{Apply: true}, ErrIdentity},
		{"Quelltyp gleich Zieltyp", measureCopy(),
			Options{FromType: "insight", Apply: true}, ErrIdentity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(ctx, nil, tc.id, tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run = %v, want %v", err, tc.want)
			}
		})
	}
}
