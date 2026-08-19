package goldbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpecConfigParse pinnt die strikte Dekodierung: unbekannte Felder,
// fehlendes algorithm und ein deklariertes drafter_sha_verified sind Fehler.
func TestSpecConfigParse(t *testing.T) {
	if _, err := ParseSpecConfig(`{"algorithm":"dspark","gamma":7}`); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for _, bad := range []string{
		`{"gamma":7}`,                     // algorithm fehlt
		`{"algorithm":"dspark","gama":7}`, // Tippfehler
		`{"algorithm":"dspark","drafter_sha_verified":true}`, // Selbstbescheinigung
		`{}`,
		`{"algorithm":"dspark","gamma":7} rm -rf /`,   // Müll nach dem Objekt
		`{"algorithm":"dspark"}{"algorithm":"eagle"}`, // zweites Objekt
		`{"algorithm":"WAS-AUCH-IMMER"}`,              // außerhalb der Enum
	} {
		if _, err := ParseSpecConfig(bad); !errors.Is(err, ErrSpecConfig) {
			t.Fatalf("%s must be rejected, got %v", bad, err)
		}
	}
}

// TestResolveDrafterSHA: lokal lesbar ⇒ selbst gehasht (verified), Konflikt
// mit Deklaration ⇒ ErrSpecProvenance, unlesbar ⇒ Deklaration, unverified.
func TestResolveDrafterSHA(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, drafterWeightsFile), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("weights"))
	want := hex.EncodeToString(sum[:])

	sc := &SpecConfig{Algorithm: "dspark", DrafterPath: dir}
	if err := ResolveDrafterSHA(sc); err != nil || sc.DrafterSHA256 != want || !sc.DrafterSHAVerified {
		t.Fatalf("self-hash: err=%v sha=%s verified=%v", err, sc.DrafterSHA256, sc.DrafterSHAVerified)
	}
	// Deklaration stimmt (case-insensitiv) ⇒ verified.
	sc = &SpecConfig{Algorithm: "dspark", DrafterPath: dir, DrafterSHA256: strings.ToUpper(want)}
	if err := ResolveDrafterSHA(sc); err != nil || !sc.DrafterSHAVerified {
		t.Fatalf("matching declaration: err=%v verified=%v", err, sc.DrafterSHAVerified)
	}
	// Deklaration falsch ⇒ Provenienz-Konflikt (der kopierte Alt-Hash).
	sc = &SpecConfig{Algorithm: "dspark", DrafterPath: dir, DrafterSHA256: "deadbeef"}
	if err := ResolveDrafterSHA(sc); !errors.Is(err, ErrSpecProvenance) {
		t.Fatalf("stale declaration must be a provenance conflict, got %v", err)
	}
	// Pfad existiert NICHT (Remote-Lauf) ⇒ Deklaration gilt, unverified.
	sc = &SpecConfig{Algorithm: "dspark", DrafterPath: filepath.Join(dir, "nope"), DrafterSHA256: "abc"}
	if err := ResolveDrafterSHA(sc); err != nil || sc.DrafterSHAVerified || sc.DrafterSHA256 != "abc" {
		t.Fatalf("missing path: err=%v verified=%v sha=%s", err, sc.DrafterSHAVerified, sc.DrafterSHA256)
	}
	// Sharded Layout (RadixArk-NVFP4: model-0000N-of-0000M.safetensors) ⇒ alle
	// Shards sortiert in EINEN Hash, verified.
	sh := filepath.Join(dir, "sharded")
	if err := os.MkdirAll(sh, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, c := range []string{"bb", "aa"} {
		if err := os.WriteFile(filepath.Join(sh, []string{"model-00002-of-00002.safetensors", "model-00001-of-00002.safetensors"}[i]), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	shSum := sha256.Sum256([]byte("aabb")) // sortiert: 00001 (aa) dann 00002 (bb)
	sc = &SpecConfig{Algorithm: "dspark", DrafterPath: sh}
	if err := ResolveDrafterSHA(sc); err != nil || !sc.DrafterSHAVerified || sc.DrafterSHA256 != hex.EncodeToString(shSum[:]) {
		t.Fatalf("sharded: err=%v verified=%v sha=%s", err, sc.DrafterSHAVerified, sc.DrafterSHA256)
	}
	// Existierendes Verzeichnis OHNE Gewichte ⇒ harter Fehler (nicht still unverified).
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	sc = &SpecConfig{Algorithm: "dspark", DrafterPath: empty}
	if err := ResolveDrafterSHA(sc); !errors.Is(err, ErrSpecConfig) {
		t.Fatalf("dir without weights must be a hard error, got %v", err)
	}
}

// TestEnvStampSpec pinnt das E1-Gate (design/04 §7): ROT — ein Report ohne
// -spec-config trägt weder spec noch concurrency (ein Gate-Leser meldet
// „fehlt"; Dry-Run byte-stabil zum Bestand); GRÜN — mit Flag trägt der Report
// die strukturierte Provenienz und den Concurrency-Stempel.
func TestEnvStampSpec(t *testing.T) {
	srv := fakeChatServer(t)
	defer srv.Close()
	base := Config{DataDir: testDataDir(t), Endpoint: srv.URL, Model: "fake-model", N: 1, Concurrency: 3, Seed: 1, TimeoutSec: 30, TempOverride: -1}

	// ROT-Referenz: ohne Flag keine Felder.
	rep, err := Run(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Env.Spec != nil {
		t.Fatalf("env without -spec-config must not carry spec: %+v", rep.Env.Spec)
	}
	if rep.Env.Concurrency != 3 {
		t.Fatalf("concurrency stamp: got %d want 3", rep.Env.Concurrency)
	}
	// Dry-Run ohne Flag: weder spec noch concurrency (Bestands-Protokoll byte-stabil).
	dry := base
	dry.DryRun, dry.Endpoint = true, ""
	rep, err = Run(context.Background(), dry)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep.Env)
	if strings.Contains(string(raw), `"spec"`) || strings.Contains(string(raw), `"concurrency"`) {
		t.Fatalf("dry-run env must stay byte-stable: %s", raw)
	}

	// GRÜN: mit Flag.
	sc, err := ParseSpecConfig(`{"algorithm":"dspark","gamma":7,"engine_build":"sha256:abc","kv_cache_dtype":"fp8_e4m3","target_quant":"nvfp4"}`)
	if err != nil {
		t.Fatal(err)
	}
	withSpec := base
	withSpec.SpecConfig = sc
	rep, err = Run(context.Background(), withSpec)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Env.Spec == nil || rep.Env.Spec.Algorithm != "dspark" || rep.Env.Spec.Gamma != 7 || rep.Env.Concurrency != 3 {
		t.Fatalf("env with -spec-config: %+v c=%d", rep.Env.Spec, rep.Env.Concurrency)
	}
	raw, _ = json.Marshal(rep.Env)
	for _, k := range []string{`"spec"`, `"algorithm":"dspark"`, `"gamma":7`, `"drafter_sha_verified":false`, `"concurrency":3`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("env json missing %s: %s", k, raw)
		}
	}
}
