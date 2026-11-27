package llmlog

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The recorded row (issue #571): a row of a consumer with record_bodies keeps
// its request and its answer whole — under every gateway.body_logging value
// and every required sensitivity (credentials, unset and unknown included),
// past the 64 KiB cap of the body policy — and says so in metadata.gw.

// recordedRow is a chat row of a recording consumer with a request past the
// cap that carries a credentials pattern.
func recordedRow(pol, sens string) GatewayRow {
	secret := "Authorization: Bearer sk-live-571-" + strings.Repeat("Z", 40)
	return GatewayRow{
		Pipeline: PipelineGatewayChat, ConsumerID: "0195a6f0-0000-7000-8000-00000000c571",
		Gateway:     Gateway{Alias: "chat", Finish: FinishStop},
		Telemetry:   Entry{RequiredSensitivity: sens, Metadata: map[string]any{}},
		Request:     `[{"role":"user","content":"` + secret + strings.Repeat("x", 100<<10) + `"}]`,
		Response:    `{"choices":[{"message":{"content":"` + strings.Repeat("y", 90<<10) + `"}}]}`,
		BodyLogging: pol,
		Recorded:    true,
	}
}

// gwWire reads metadata.gw back as JSON.
func gwWire(t *testing.T, e Entry) map[string]any {
	t.Helper()
	raw, err := json.Marshal(e.Metadata["gw"])
	if err != nil {
		t.Fatalf("marshal gw: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal gw: %v", err)
	}
	return m
}

func TestRecordedRowKeepsWholeBodies(t *testing.T) {
	for _, pol := range goldenPolicies {
		for _, sens := range goldenSensitivities {
			r := recordedRow(pol, sens)
			e := RecordedGatewayEntry(r)
			if e.RequestUser != r.Request || e.ResponseContent != r.Response || e.RequestSystem != "" {
				t.Errorf("policy %q sensitivity %q: stored request %d / answer %d bytes, want the whole %d / %d",
					pol, sens, len(e.RequestUser), len(e.ResponseContent), len(r.Request), len(r.Response))
			}
			if !strings.Contains(e.RequestUser, "sk-live-571-") {
				t.Errorf("policy %q sensitivity %q: the credentials pattern is not in the recorded row", pol, sens)
			}
			gw := gwWire(t, e)
			if gw["recorded"] != true || gw["record_lost"] != nil {
				t.Errorf("policy %q sensitivity %q: metadata.gw %v, want recorded true without record_lost", pol, sens, gw)
			}
			if e.ConsumerID != r.ConsumerID || e.Pipeline != PipelineGatewayChat || gw["op"] != GatewayOpChat {
				t.Errorf("policy %q sensitivity %q: row %s/%s op %v — the attribution is GatewayEntry's", pol, sens, e.Pipeline, e.ConsumerID, gw["op"])
			}
		}
	}
}

// TestGatewayEntryIgnoresTheRecordingFields: GatewayEntry builds the row of
// an unrecorded call; recording fields on its input change nothing (the
// marker and the whole bodies are RecordedGatewayEntry's), and a recorded row
// built by it would be sealed like any other.
func TestGatewayEntryIgnoresTheRecordingFields(t *testing.T) {
	names, rows := unrecordedMatrix()
	for _, n := range names {
		r := rows[n]
		base := goldenLine(n, GatewayEntry(r))
		r.RecordLost, r.RecordLostCause, r.RecordedHeld = []string{RecordLostRequest}, RecordLostMemory, 42
		if got := goldenLine(n, GatewayEntry(r)); got != base {
			t.Fatalf("%s: recording fields on an unrecorded row changed it:\n got  %s\n want %s", n, got, base)
		}
	}
}

func TestRecordedRowNamesWhatItLost(t *testing.T) {
	r := recordedRow(BodyLoggingNone, "credentials")
	r.Request = ""
	r.RecordLost = []string{"bogus", RecordLostResponse, RecordLostRequest, RecordLostRequest}
	r.RecordLostCause = RecordLostMemory
	gw := gwWire(t, RecordedGatewayEntry(r))
	lost, _ := json.Marshal(gw["record_lost"])
	if string(lost) != `["request","response"]` || gw["record_lost_cause"] != RecordLostMemory {
		t.Errorf("record_lost %s cause %v, want [request response] (vocabulary order, once each, unknown dropped) and memory", lost, gw["record_lost_cause"])
	}
	r.RecordLostCause = "made-up"
	if gw := gwWire(t, RecordedGatewayEntry(r)); gw["record_lost_cause"] != nil {
		t.Errorf("an unknown cause is stored: %v", gw["record_lost_cause"])
	}
	r.RecordLost = []string{"bogus"}
	if gw := gwWire(t, RecordedGatewayEntry(r)); gw["record_lost"] != nil || gw["record_lost_cause"] != nil {
		t.Errorf("no known lost body, yet record_lost %v cause %v", gw["record_lost"], gw["record_lost_cause"])
	}
}

// recordedAccount resets the recording account for a test and checks it is
// back at zero afterwards.
func recordedAccount(t *testing.T) {
	t.Helper()
	if n := recordedHeld.Load(); n != 0 {
		t.Fatalf("the recording account holds %d bytes before the test", n)
	}
	t.Cleanup(func() {
		if n := recordedHeld.Load(); n != 0 {
			t.Errorf("the recording account holds %d bytes after the test, want 0", n)
			recordedHeld.Store(0)
		}
	})
}

func TestRecordedAccountBound(t *testing.T) {
	recordedAccount(t)
	if !ReserveRecorded(maxRecordedHeld-10) || !ReserveRecorded(10) {
		t.Fatal("the account refuses bytes within its bound")
	}
	if ReserveRecorded(1) {
		t.Fatal("the account takes a byte past maxRecordedHeld")
	}
	if !ReserveRecorded(0) || !ReserveRecorded(-5) {
		t.Fatal("a reservation of nothing is refused")
	}
	if RecordedHeld() != maxRecordedHeld {
		t.Fatalf("held %d, want %d", RecordedHeld(), maxRecordedHeld)
	}
	ReleaseRecorded(maxRecordedHeld)
	ReleaseRecorded(-3)
	if RecordedHeld() != 0 {
		t.Fatalf("held %d after the release, want 0", RecordedHeld())
	}
	// Concurrent reservations never pass the bound together.
	var wg sync.WaitGroup
	var mu sync.Mutex
	took := int64(0)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ReserveRecorded(maxRecordedHeld / 8) {
				mu.Lock()
				took += maxRecordedHeld / 8
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if took != maxRecordedHeld || RecordedHeld() != maxRecordedHeld {
		t.Fatalf("64 concurrent reservations of an eighth took %d (held %d), want exactly the bound %d", took, RecordedHeld(), int64(maxRecordedHeld))
	}
	ReleaseRecorded(took)
}

// TestWriterGivesTheRecordedTextsBack: a recorded row's texts go back to the
// account once the writer is done with them — inserted, failed, dropped at
// a full queue, or written to no pool at all.
func TestWriterGivesTheRecordedTextsBack(t *testing.T) {
	recordedAccount(t)
	pool := &pgxpool.Pool{}
	block := make(chan struct{})
	var inserted atomic.Int64
	w := newWriter(pool, func(*pgxpool.Pool, Entry) error {
		<-block
		inserted.Add(1)
		return nil
	})
	row := recordedRow(BodyLoggingNone, "credentials")
	if !ReserveRecorded(int64(len(row.Request) + len(row.Response))) {
		t.Fatal("reserve")
	}
	row.RecordedHeld = int64(len(row.Request) + len(row.Response))
	w.enqueue(RecordedGatewayEntry(row))
	if RecordedHeld() != row.RecordedHeld {
		t.Fatalf("held %d while the row is queued, want %d", RecordedHeld(), row.RecordedHeld)
	}
	close(block)
	for deadline := time.Now().Add(10 * time.Second); w.pending.Load() > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if RecordedHeld() != 0 || inserted.Load() != 1 {
		t.Fatalf("after the insert held %d inserted %d, want 0 and 1", RecordedHeld(), inserted.Load())
	}
	// A full queue drops the row and gives its texts back.
	full := newWriter(pool, func(*pgxpool.Pool, Entry) error { return nil })
	full.queue = make([]Entry, maxQueuedInserts)
	full.running = maxConcurrentInserts
	if !ReserveRecorded(100) {
		t.Fatal("reserve")
	}
	before := InsertDropped()
	full.enqueue(Entry{Pipeline: PipelineGatewayChat, recordedHeld: 100})
	if RecordedHeld() != 0 || InsertDropped() != before+1 {
		t.Fatalf("a dropped recorded row: held %d dropped +%d, want 0 and +1", RecordedHeld(), InsertDropped()-before)
	}
	// No pool: nothing is written, the texts go back.
	if !ReserveRecorded(7) {
		t.Fatal("reserve")
	}
	r := recordedRow(BodyLoggingNone, "public")
	r.RecordedHeld = 7
	RecordGateway(nil, r)
	if RecordedHeld() != 0 {
		t.Fatalf("a row without a pool keeps %d bytes", RecordedHeld())
	}
}
