package dream

// T04-9 llmlog row fixture for the five dream chat pipelines. It pins the two
// shapes that differ from the common body row and would otherwise be one
// careless edit away from silently changing persisted rows:
//
//   - dream-daily-synthesis carries NO block ids at construction. A nil slice
//     reaches pgx unchanged (llmlog.go passes e.BlockIDs straight through, no
//     nil guard) and persists as a NULL block_ids column; []string{} would
//     persist as an empty array instead. The row only learns its report block
//     id after the report block exists.
//   - dream-keywords carries metadata.attempt from the retry loop AND
//     metadata.chain from the telemetry funnel in ONE map. applyChainTelemetry
//     writes into whatever map it finds, so the counter has to be in place
//     before it runs — a map assigned afterwards would drop the chain.
//
// The expectations are the row as it was persisted before the constructors
// existed; they are deliberately spelled out as JSON, not as field asserts.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
)

// dreamRowFixture projects the columns the two deviations live in, with the
// nil semantics llmlog.insert applies: metadata nil becomes {}, block_ids nil
// stays NULL.
type dreamRowFixture struct {
	Pipeline     string         `json:"pipeline"`
	RequestSyst  string         `json:"request_system"`
	RequestUser  string         `json:"request_user"`
	BlockIDs     []string       `json:"block_ids"`
	DreamVersion *int16         `json:"dream_version"`
	Metadata     map[string]any `json:"metadata"`
	Attempt      int            `json:"attempt"`
	DurationMs   int64          `json:"duration_ms"`
}

func dreamRow(t *testing.T, e llmlog.Entry) string {
	t.Helper()
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	b, err := json.Marshal(dreamRowFixture{
		Pipeline: e.Pipeline, RequestSyst: e.RequestSystem, RequestUser: e.RequestUser,
		BlockIDs: e.BlockIDs, DreamVersion: e.DreamVersion, Metadata: meta,
		Attempt: e.Attempt, DurationMs: e.Duration.Milliseconds(),
	})
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	return string(b)
}

func fixtureRouter() *Router {
	return &Router{Admit: llm.Admission{Class: dispatch.ClassBackground}}
}

func fixtureAttempts() []llm.ChainAttempt {
	return []llm.ChainAttempt{{Backend: "gpu-a", Class: "ok", Ms: 200, WaitMs: 7}}
}

// TestNewDreamEntryVersionPointersAreDistinct: dream_version is a *int16 on
// the row, so every entry needs its OWN addressable copy of the version. A
// shared pointer would couple five pipelines that all keep their entry alive
// until a deferred Record fires.
func TestNewDreamEntryVersionPointersAreDistinct(t *testing.T) {
	a := newDreamEntry("dream-eval", "", "", nil)
	b := newDreamEntry("dream-keywords", "", "", nil)
	if a.DreamVersion == nil || b.DreamVersion == nil {
		t.Fatal("dream_version must be set on every dream row")
	}
	if a.DreamVersion == b.DreamVersion {
		t.Error("two entries share one dream_version pointer — the rows are not independent")
	}
	if *a.DreamVersion != int16(Version) {
		t.Errorf("dream_version = %d, want %d", *a.DreamVersion, Version)
	}
}

// TestDreamRowFixture walks the three distinct dream row shapes end to end
// through the telemetry funnel and the body slim, and compares the persisted
// projection against the recorded row.
func TestDreamRowFixture(t *testing.T) {
	r := fixtureRouter()
	attempts := fixtureAttempts()

	t.Run("body row (dream-eval)", func(t *testing.T) {
		entry := newDreamEntry("dream-eval", "SYS", "USR", []string{"b1", "b2"})
		entry.Duration = 5 * time.Second
		r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, nil, nil, attempts, nil)
		got := dreamRow(t, entry.Slimmed(false))
		want := `{"pipeline":"dream-eval","request_system":"SYS","request_user":"USR","block_ids":["b1","b2"],"dream_version":5,"metadata":{"chain":[{"backend":"gpu-a","err_class":"ok","ms":200,"wait_ms":7}]},"attempt":1,"duration_ms":200}`
		if got != want {
			t.Errorf("dream-eval row\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("no block ids (dream-daily-synthesis)", func(t *testing.T) {
		entry := newDreamEntry("dream-daily-synthesis", "SYS", "USR", nil)
		r.applyChainTelemetry(entry, backends.RoleDigest, backends.SensInternal, nil, nil, attempts, nil)
		if entry.BlockIDs != nil {
			t.Fatalf("block_ids = %#v, want nil (NULL column, not an empty array)", entry.BlockIDs)
		}
		got := dreamRow(t, entry.Slimmed(false))
		want := `{"pipeline":"dream-daily-synthesis","request_system":"SYS","request_user":"USR","block_ids":null,"dream_version":5,"metadata":{"chain":[{"backend":"gpu-a","err_class":"ok","ms":200,"wait_ms":7}]},"attempt":1,"duration_ms":200}`
		if got != want {
			t.Errorf("dream-daily-synthesis row\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("attempt and chain in one map (dream-keywords)", func(t *testing.T) {
		entry := newDreamEntry("dream-keywords", "SYS", "USR", []string{"b1"})
		entry.Duration = 1500 * time.Millisecond
		entry.Metadata = map[string]any{"attempt": 2}
		r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, nil, nil, attempts, nil)
		if entry.Metadata["attempt"] != 2 {
			t.Errorf("metadata.attempt = %v, want 2 (the retry counter must survive the funnel)", entry.Metadata["attempt"])
		}
		if _, ok := entry.Metadata["chain"]; !ok {
			t.Error("metadata.chain missing — the funnel must add the walk to the counter's map")
		}
		got := dreamRow(t, entry.Slimmed(false))
		want := `{"pipeline":"dream-keywords","request_system":"SYS","request_user":"USR","block_ids":["b1"],"dream_version":5,"metadata":{"attempt":2,"chain":[{"backend":"gpu-a","err_class":"ok","ms":200,"wait_ms":7}]},"attempt":1,"duration_ms":200}`
		if got != want {
			t.Errorf("dream-keywords row\n got: %s\nwant: %s", got, want)
		}
	})
}
