package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// Wave X-W3a, seam 1 — the dump stamp is complete (X-W1 N9, X-W2b N-B).
//
// Before this wave the driver asked the instance what it was ONLY when the run
// named shadow types (gateInstance, commands.go:186-191), and
// DumpStamp.InstanceKind is omitempty — so an ordinary dump carried no kind at
// all, and F-32 ("all dumps of one campaign come from ONE instance") was
// unguarded for exactly the dumps a campaign is built out of. X-W2b §4.2
// measured the consequence: a compare over two measure-copy dumps and a LIVE
// noise pair ran through with exit 0, because empty compares equal to empty.
//
// The two tests below are that measurement, as a gate: every non-dry dump
// stamps a kind, and the mixed campaign is refused with exit 4.

// xw3aFakeCtx serves the three surfaces `dump` touches, with a configurable
// server.instance_kind — one fake per simulated instance.
type xw3aFakeCtx struct {
	srv  *httptest.Server
	kind string
	// kindReads counts the reads of server.instance_kind, so a test can tell
	// "the driver asked" from "the driver guessed".
	kindReads int
}

func xw3aNewFakeCtx(t *testing.T, kind string) *xw3aFakeCtx {
	t.Helper()
	f := &xw3aFakeCtx{kind: kind}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"db": map[string]any{
					"migrations_max": armsweep.TypeNameMigration,
					"hnsw":           map[string]any{"ef_search_effective": "40 (default)"},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/api/settings/"):
			key := strings.TrimPrefix(r.URL.Path, "/api/settings/")
			var value any = false
			switch key {
			case armsweep.SettingInstanceKind:
				f.kindReads++
				value = f.kind
			case "cluster.inject_max":
				value = float64(3)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"setting": map[string]any{"key": key, "value": value},
			})
		case r.URL.Path == "/api/query":
			xw3aWriteQuery(w)
		case r.URL.Path == "/api/manage":
			// The drift census. Frozen: this seam measures provenance, and a
			// moving corpus would abort the dump for an unrelated reason.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"drift": map[string]any{
					"at": "2026-08-27T00:00:00Z", "retrievable_blocks": 2,
					"types":    []any{},
					"gold_ids": []any{},
				},
			})
		default:
			t.Errorf("unerwarteter Pfad %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// xw3aWriteQuery answers one measurement request with two candidates, the gold
// one first. Identical on every instance: this seam is about provenance, not
// about numbers.
func xw3aWriteQuery(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"sources": []any{
			map[string]any{"id": "id-a"},
			map[string]any{"id": "id-b"},
		},
		"arm_ranks": map[string]any{
			"rows": []any{
				map[string]any{"id": "id-a", "rank_semantic": 1, "mass_factor": 1, "type_factor": 1, "type_name": "knowledge"},
				map[string]any{"id": "id-b", "rank_semantic": 2, "mass_factor": 1, "type_factor": 1, "type_name": "knowledge"},
			},
			"fusion_order":    []any{"id-a", "id-b"},
			"effective_query": "q",
			"embed_model":     "m",
			"selector":        map[string]any{"mode": "ann", "reason": "grey", "scan_tuples": 60000},
		},
	})
}

// xw3aCommon is a non-dry driver against one fake instance.
func xw3aCommon(dir, baseURL, runID string) *common {
	return &common{
		dir: dir, quiet: true, seed: 20260812, concurrency: 1, timeout: 5,
		slices: goldset.SliceKI, retries: armsweep.DefaultRetries,
		runID: runID, baseURL: baseURL, apiKey: "k",
	}
}

// xw3aDump dumps under runID against the given instance and returns the stamp
// as it was written to disk.
func xw3aDump(t *testing.T, dir, baseURL, runID, pinFile string) armsweep.DumpStamp {
	t.Helper()
	if err := cmdDump(context.Background(), xw3aCommon(dir, baseURL, runID), pinFile); err != nil {
		t.Fatalf("dump %s: %v", runID, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, armsweep.DumpDirName, runID+".stamp.json"))
	if err != nil {
		t.Fatalf("Stempel %s lesen: %v", runID, err)
	}
	var stamp armsweep.DumpStamp
	if err := json.Unmarshal(raw, &stamp); err != nil {
		t.Fatalf("Stempel %s parsen: %v", runID, err)
	}
	return stamp
}

// TestXW3aEveryDumpStampsTheInstanceKind is the first half of seam 1: a dump
// that names NO shadow types still says which instance it came from.
//
// RED before X-W3a: gateInstance returns "" without shadow types
// (commands.go:186-191) and the field is omitempty (run.go:254) — the stamp
// carries no instance_kind at all.
func TestXW3aEveryDumpStampsTheInstanceKind(t *testing.T) {
	for _, kind := range []string{armsweep.InstanceKindLive, armsweep.InstanceKindMeasureCopy} {
		t.Run(kind, func(t *testing.T) {
			dir := tinyGold(t)
			f := xw3aNewFakeCtx(t, kind)
			if err := cmdPrime(context.Background(), xw3aCommon(dir, f.srv.URL, "prime1")); err != nil {
				t.Fatalf("prime: %v", err)
			}
			stamp := xw3aDump(t, dir, f.srv.URL, "plain", "pins-prime1.jsonl")
			if len(stamp.ShadowTypes) != 0 {
				t.Fatalf("die Sonde ist kein Nicht-Schatten-Dump: %v", stamp.ShadowTypes)
			}
			if stamp.InstanceKind != kind {
				t.Errorf("stamp.instance_kind = %q, erwartet %q — ein Dump ohne Stempel macht die F-32-Kampagnenregel unbewachbar",
					stamp.InstanceKind, kind)
			}
			if f.kindReads == 0 {
				t.Errorf("der Treiber hat %s nie gelesen", armsweep.SettingInstanceKind)
			}
		})
	}
}

// TestXW3aMixedInstanceCampaignIsRefused is the second half, and it is the
// X-W2b §4.2 (b-1) measurement inverted: base and cond off a measure copy, the
// noise pair off a LIVE instance — the real shape of that campaign — must be
// exit 4 and not exit 0.
func TestXW3aMixedInstanceCampaignIsRefused(t *testing.T) {
	dir := tinyGold(t)
	copyInst := xw3aNewFakeCtx(t, armsweep.InstanceKindMeasureCopy)
	liveInst := xw3aNewFakeCtx(t, armsweep.InstanceKindLive)

	if err := cmdPrime(context.Background(), xw3aCommon(dir, copyInst.srv.URL, "prime1")); err != nil {
		t.Fatalf("prime: %v", err)
	}
	pins := "pins-prime1.jsonl"
	xw3aDump(t, dir, copyInst.srv.URL, "B1", pins)
	xw3aDump(t, dir, copyInst.srv.URL, "B0", pins)
	xw3aDump(t, dir, liveInst.srv.URL, "V0", pins)
	xw3aDump(t, dir, liveInst.srv.URL, "V0P", pins)

	reports := filepath.Join(t.TempDir(), armsweep.ReportDirName)
	c := xw3aCommon(dir, copyInst.srv.URL, "cmp")
	err := cmdCompare(c, "B1.jsonl", "B0.jsonl", "V0.jsonl,V0P.jsonl", reports, "mix", "")
	if !errors.Is(err, armsweep.ErrStampIncongruent) {
		t.Fatalf("cmdCompare = %v, erwartet ErrStampIncongruent (Exit 4) — ein Live/Kopie-Mix ist keine Kampagne", err)
	}
	if code := exitCodeFor(err); code != 4 {
		t.Errorf("exitCodeFor = %d, erwartet 4", code)
	}
	if !strings.Contains(err.Error(), "instance_kind") {
		t.Errorf("die Abweisung nennt das Feld nicht: %v", err)
	}
}

// TestXW3aRefusedDeclarationWritesNoReport: an undeclarable field is exit 3 AND
// leaves no artefact behind.
//
// `compare` writes its report even when it refuses — a red noise floor is a
// finding, and the artefact is the evidence for it. A rejected declaration is
// not that: nothing was walked, so the body is empty, and writing it would put
// a table of zeros under a "G-NOISE bestanden" line. That reads as a result.
func TestXW3aRefusedDeclarationWritesNoReport(t *testing.T) {
	dir := tinyGold(t)
	inst := xw3aNewFakeCtx(t, armsweep.InstanceKindMeasureCopy)
	if err := cmdPrime(context.Background(), xw3aCommon(dir, inst.srv.URL, "prime1")); err != nil {
		t.Fatalf("prime: %v", err)
	}
	pins := "pins-prime1.jsonl"
	for _, run := range []string{"B1", "B0", "V0", "V0P"} {
		xw3aDump(t, dir, inst.srv.URL, run, pins)
	}

	reports := filepath.Join(t.TempDir(), armsweep.ReportDirName)
	c := xw3aCommon(dir, inst.srv.URL, "cmp")
	err := cmdCompare(c, "B1.jsonl", "B0.jsonl", "V0.jsonl,V0P.jsonl", reports, "neg", "migrations_max")
	if !errors.Is(err, armsweep.ErrGateRefused) {
		t.Fatalf("cmdCompare = %v, erwartet ErrGateRefused (Exit 3)", err)
	}
	if code := exitCodeFor(err); code != 3 {
		t.Errorf("exitCodeFor = %d, erwartet 3", code)
	}
	for _, name := range []string{"neg.json", "neg.md"} {
		if _, statErr := os.Stat(filepath.Join(reports, name)); statErr == nil {
			t.Errorf("%s wurde geschrieben — ein Report ohne gelaufene Messung liest sich als Ergebnis", name)
		}
	}
}
