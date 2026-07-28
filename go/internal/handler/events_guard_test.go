package handler

// RC-1 wave S2 — guard_review on the SSE push contract.
//
// The probes here are the RULE, not a hand-copied field list: probe (a) derives
// the set of fields the push frame owes its clients from the SOURCE of the two
// pull paths (status.go assemble / status_tenant.go SnapshotForTenant) and only
// subtracts an ENUMERATED exclusion list. A section that a future wave adds to
// both pull paths therefore fails this test until the push frame carries it too
// — which is exactly the class of gap S2 closes.

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// sseFrameExcluded is the ENUMERATED exclusion list of probe (a): the only two
// fields that are set on BOTH pull paths and still legitimately absent from the
// status frame. Every entry names WHY, and the probe fails if an entry stops
// being part of the intersection (a stale exemption is a silent hole).
var sseFrameExcluded = map[string]string{
	"success":  "JSON envelope flag of the /api/status response — the SSE frame's envelope is the event name",
	"backends": "carried by its OWN stream event so the two diff independently (events.go runLoop / backendsDiffKey)",
}

// statusResponseWireNames maps every statusResponse Go field onto its wire name,
// so a rule expressed over source-level composite literals can be compared with
// the JSON field set of the push frame.
func statusResponseWireNames(t *testing.T) map[string]string {
	t.Helper()
	rt := reflect.TypeOf(statusResponse{})
	out := make(map[string]string, rt.NumField())
	for i := range rt.NumField() {
		f := rt.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		out[f.Name] = name
	}
	return out
}

// jsonWireFields returns the wire field names of a struct value's type.
func jsonWireFields(t *testing.T, v any) map[string]bool {
	t.Helper()
	rt := reflect.TypeOf(v)
	out := make(map[string]bool, rt.NumField())
	for i := range rt.NumField() {
		f := rt.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
	return out
}

// statusResponseLiteralFields parses one source file and returns the field names
// a named function SETS on a statusResponse composite literal. `Field: nil` is
// read as an explicit NOT-carried marker (assemble's Activity), not as a set
// field — "the path deliberately leaves this empty" must not become "the path
// carries this".
func statusResponseLiteralFields(t *testing.T, file, fn string) map[string]bool {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var decl *ast.FuncDecl
	for _, d := range parsed.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
			decl = fd
			break
		}
	}
	if decl == nil {
		t.Fatalf("func %s not found in %s — the rule lost its anchor", fn, file)
	}
	out := map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "statusResponse" {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "nil" {
				continue
			}
			out[key.Name] = true
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("no statusResponse literal fields found in %s/%s — the rule lost its anchor", file, fn)
	}
	return out
}

// TestStatusEventCarriesEveryDualPathField is probe S2(a), the RULE-Golden:
// every statusResponse field that BOTH pull paths set — minus the enumerated
// exclusion list above — must exist as a same-named JSON field on the SSE
// status frame. A section that both a server-admin and a tenant can pull is a
// section the "push signal" owes its clients; deriving that set from the source
// instead of restating it keeps the next such section from silently shipping
// pull-only.
func TestStatusEventCarriesEveryDualPathField(t *testing.T) {
	admin := statusResponseLiteralFields(t, "status.go", "assemble")
	tenant := statusResponseLiteralFields(t, "status_tenant.go", "SnapshotForTenant")
	wire := statusResponseWireNames(t)
	frame := jsonWireFields(t, statusEvent{})

	var soll []string
	seenExcluded := map[string]bool{}
	for field := range admin {
		if !tenant[field] {
			continue
		}
		name, ok := wire[field]
		if !ok {
			t.Fatalf("field %s appears in a statusResponse literal but not on the struct", field)
		}
		if _, skip := sseFrameExcluded[name]; skip {
			seenExcluded[name] = true
			continue
		}
		soll = append(soll, name)
	}
	sort.Strings(soll)
	if len(soll) == 0 {
		t.Fatal("the rule produced an EMPTY Sollmenge — the source parse or the exclusion list is broken")
	}
	t.Logf("Sollmenge (set on both pull paths, minus exclusions): %v", soll)

	for name, why := range sseFrameExcluded {
		if !seenExcluded[name] {
			t.Errorf("exclusion %q (%s) is no longer part of the dual-path set — a stale exemption hides a real gap", name, why)
		}
	}
	for _, name := range soll {
		if !frame[name] {
			t.Errorf("statusEvent lacks a %q JSON field: it is set on BOTH pull paths and is not on the exclusion list, so the push signal must carry it", name)
		}
	}
}

// TestStatusEventDiffIgnoresGuardBuiltAt is probe S2(b): the guard_review
// section's built_at advances with EVERY generation (status_guard.go), so for
// diff purposes it is an as_of-class field. 100 ticks with unchanged counts and
// a walking stamp must produce ZERO extra status frames; a real count change
// must still fire. Mutation probe for this test: drop the built_at nulling in
// statusEvent.diffKey and every tick diffs.
func TestStatusEventDiffIgnoresGuardBuiltAt(t *testing.T) {
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	oldest := base.Add(-72 * time.Hour)
	frameAt := func(builtAt time.Time, needsReview int) statusEvent {
		return statusEventOf(statusResponse{
			AsOf:  builtAt,
			Dream: dreamStatus{Mode: "on"},
			GuardReview: &guardReviewStatus{
				NeedsReview: needsReview, NearDuplicate: 2, PossibleDuplicate: 1,
				OldestUpdatedAt: &oldest, BuiltAt: &builtAt,
			},
		})
	}

	prev := frameAt(base, 114).diffKey()
	fired := 0
	for i := 1; i <= 100; i++ {
		key := frameAt(base.Add(time.Duration(i)*5*time.Second), 114).diffKey()
		if !bytes.Equal(key, prev) {
			fired++
		}
		prev = key
	}
	if fired != 0 {
		t.Errorf("100 ticks with unchanged guard counts fired %d extra status frames, want 0 — built_at must be zeroed in diffKey like as_of", fired)
	}

	worked := frameAt(base.Add(505*time.Second), 113).diffKey()
	if bytes.Equal(worked, prev) {
		t.Error("a resolved needs_review block MUST alter the status diff key")
	}

	// diffKey works on a VALUE copy of the frame, but the guard section behind
	// it is a pointer into the shared per-tick generation (status_guard.go): a
	// dozen SSE hubs and every pull read the same struct. Zeroing built_at in
	// place would corrupt that generation for all of them.
	shared := &guardReviewStatus{NeedsReview: 7, OldestUpdatedAt: &oldest, BuiltAt: &base}
	_ = statusEventOf(statusResponse{GuardReview: shared}).diffKey()
	if shared.BuiltAt == nil || !shared.BuiltAt.Equal(base) {
		t.Errorf("diffKey mutated the SHARED generation row: built_at = %v, want %v", shared.BuiltAt, base)
	}
}

// TestStatusEventGuardReviewThreeCasesOnTheWire is probe S2(c): "no fresh
// generation", "queue genuinely empty" and "counts from an older generation"
// must be three DISTINGUISHABLE states on the wire, never one. Mutation probe
// for this test: drop omitempty (or make the field a value type) on
// statusEvent.GuardReview and case 1 renders as "0 offen" / an explicit null
// instead of an absent section.
func TestStatusEventGuardReviewThreeCasesOnTheWire(t *testing.T) {
	fresh := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	old := fresh.Add(-30 * time.Minute)

	raw := func(s *guardReviewStatus) map[string]json.RawMessage {
		t.Helper()
		b, err := json.Marshal(statusEventOf(statusResponse{AsOf: fresh, GuardReview: s}))
		if err != nil {
			t.Fatalf("marshal status frame: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal status frame: %v", err)
		}
		return m
	}

	// Case 1 — no fresh generation: the section is ABSENT, not a zeroed object
	// and not an explicit null. "I cannot tell you" must not render as "0 open".
	if v, present := raw(nil)["guard_review"]; present {
		t.Errorf("no generation must OMIT guard_review, got %s", v)
	}

	// Case 2 — the queue is genuinely empty, freshly measured.
	empty := raw(&guardReviewStatus{BuiltAt: &fresh})
	emptySection, ok := empty["guard_review"]
	if !ok {
		t.Fatal("a fresh empty queue must still carry the guard_review section")
	}
	assertKeys(t, "status frame guard_review", emptySection, []string{
		"needs_review", "near_duplicate", "possible_duplicate", "oldest_updated_at", "built_at",
	})
	if got := string(mustField(t, emptySection, "needs_review")); got != "0" {
		t.Errorf("empty queue needs_review = %s, want 0", got)
	}
	if got := string(mustField(t, emptySection, "built_at")); !strings.Contains(got, "12:00:00") {
		t.Errorf("empty queue built_at = %s, want the fresh stamp", got)
	}

	// Case 3 — same counts, but measured by an older generation.
	staleSection, ok := raw(&guardReviewStatus{BuiltAt: &old})["guard_review"]
	if !ok {
		t.Fatal("a stale-but-served generation must still carry the guard_review section")
	}
	if bytes.Equal(emptySection, staleSection) {
		t.Errorf("fresh and older generation render identically (%s) — the age of the counts is invisible", emptySection)
	}
}
