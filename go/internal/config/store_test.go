package config

import (
	"strings"
	"sync"
	"testing"
)

// generation builds a Validate-clean config whose chat tuple is internally
// branded: host, api_key and model all carry the same generation marker, so a
// torn read (host of gen A + key of gen B) is detectable.
func generation(t *testing.T, marker string) *Config {
	t.Helper()
	c, issues := cfgFrom(t, map[string]string{
		"chat.host":             "http://" + marker + ".example:8089",
		"chat.api_key":          "sk-" + marker + "-0123456789abcdefghijklmn",
		"chat.model":            "model-" + marker,
		"scheduler.read_scopes": "scope-" + marker + ",shared",
	})
	if len(issues) != 0 {
		t.Fatalf("generation %s: unexpected issues %v", marker, issues)
	}
	return c
}

// TestStoreSnapshotConsistencyUnderRace runs under -race: 8 readers pull
// snapshots and assert the chat tuple plus ReadScopes are internally
// consistent (single generation) while a writer flips generations. The
// red-proof for this mechanism is documented in the wave report: with the
// atomic.Pointer temporarily replaced by a plain *Config field, `go test
// -race` reports the data race this store exists to prevent — the same class
// as the legacy 4-field llm.ChatFallback package-var swap (torn read).
func TestStoreSnapshotConsistencyUnderRace(t *testing.T) {
	genA := generation(t, "gen-a")
	genB := generation(t, "gen-b")
	store := NewStore(genA)

	const readers = 8
	const iterations = 2000
	var wg sync.WaitGroup

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				cfg := store.Snapshot()
				b := cfg.ChatBackend()
				marker := "gen-a"
				if strings.Contains(b.Host, "gen-b") {
					marker = "gen-b"
				}
				if !strings.Contains(b.APIKey, marker) || !strings.Contains(b.Model, marker) {
					t.Errorf("torn tuple: host=%q key=%q model=%q", b.Host, b.APIKey, b.Model)
					return
				}
				if cfg.Scheduler.ReadScopes[0] != "scope-"+marker {
					t.Errorf("torn scopes: host=%q scopes=%v", b.Host, cfg.Scheduler.ReadScopes)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			next := genB
			if i%2 == 1 {
				next = genA
			}
			if err := store.Replace(next); err != nil {
				t.Errorf("Replace: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// TestStoreReplaceRejectsInvalid proves Replace is the F2 validation gate:
// a config with a SeverityError never becomes the published generation.
func TestStoreReplaceRejectsInvalid(t *testing.T) {
	good := generation(t, "good")
	store := NewStore(good)

	bad := *good
	bad.Query.ScoreThreshold = 0.5 // > ConfidentThreshold ⇒ V2 ERROR

	err := store.Replace(&bad)
	if err == nil {
		t.Fatal("Replace must reject a config with validation errors")
	}
	if !strings.Contains(err.Error(), "query.score_threshold") {
		t.Errorf("rejection error should name the field, got: %v", err)
	}
	if store.Snapshot() != good {
		t.Error("rejected Replace must keep the previous generation published")
	}
}

func TestStoreReplacePublishes(t *testing.T) {
	a := generation(t, "gen-a")
	b := generation(t, "gen-b")
	store := NewStore(a)
	if err := store.Replace(b); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if store.Snapshot() != b {
		t.Error("Replace must publish the new generation")
	}
}

// TestStoreCopyOnWrite pins the documented update idiom: shallow-copy the
// snapshot, change a field, Replace — the previous generation stays intact.
func TestStoreCopyOnWrite(t *testing.T) {
	a := generation(t, "gen-a")
	store := NewStore(a)

	c := *store.Snapshot()
	c.Rerank.MaxDocs = 25
	if err := store.Replace(&c); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if a.Rerank.MaxDocs == 25 {
		t.Error("copy-on-write must not mutate the previous generation")
	}
	if store.Snapshot().Rerank.MaxDocs != 25 {
		t.Error("new generation must carry the change")
	}
}
