package oai

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/llmlog"
)

// The capture of a recorded request (record.go): the recording account holds
// exactly the stream buffer's capacity while the stream grows, hands it to
// the row, and gives everything back on a refusal or when no row takes it;
// the shutdown's forced row of a recording consumer names what it lacks.

func TestRecordingStreamHoldsItsCapacity(t *testing.T) {
	base := llmlog.RecordedHeld()
	r := recording{on: true}
	line := []byte("data: " + strings.Repeat("x", 300) + "\n\n")
	for range 1000 {
		r.addStream(line)
		if got := llmlog.RecordedHeld() - base; got != int64(r.stream.Cap()) || r.held != got || r.streamHeld != got {
			t.Fatalf("held %d (request %d, stream %d), buffer capacity %d — the account must hold exactly the buffer", got, r.held, r.streamHeld, r.stream.Cap())
		}
	}
	if got := r.streamText(); got != strings.Repeat(string(line), 1000) || r.lost != nil {
		t.Fatalf("stream text %d bytes, lost %v — want the 1000 lines whole", len(got), r.lost)
	}
	var row llmlog.GatewayRow
	r.mark(&row)
	if !row.Recorded || row.RecordedHeld != int64(r.stream.Cap()) || r.held != 0 || row.RecordLost != nil {
		t.Fatalf("row recorded %v holds %d (request keeps %d), lost %v — want the buffer handed to the row", row.Recorded, row.RecordedHeld, r.held, row.RecordLost)
	}
	llmlog.ReleaseRecorded(row.RecordedHeld)
	if llmlog.RecordedHeld() != base {
		t.Fatalf("the account holds %d bytes after the row gave its texts back", llmlog.RecordedHeld()-base)
	}
}

func TestRecordingRefusalDropsTheStreamAndGivesBack(t *testing.T) {
	base := llmlog.RecordedHeld()
	r := recording{on: true}
	r.addStream([]byte("data: first\n\n"))
	// Fill the account to its last byte, so the next growth is refused.
	rest := int64(0)
	for step := int64(1 << 26); step > 0; step /= 2 {
		for llmlog.ReserveRecorded(step) {
			rest += step
		}
	}
	defer llmlog.ReleaseRecorded(rest)
	r.addStream([]byte(strings.Repeat("y", 4096)))
	if r.streamText() != "" || !r.streamLost || len(r.lost) != 1 || r.lost[0] != llmlog.RecordLostResponse {
		t.Fatalf("after a refused growth: text %q lost %v %v — want the stream dropped and the answer named lost", r.streamText(), r.streamLost, r.lost)
	}
	r.addStream([]byte("data: later\n\n"))
	if r.streamText() != "" || r.held != 0 || llmlog.RecordedHeld()-base != rest {
		t.Fatalf("after the refusal the request holds %d, the account %d beyond the filler — want 0 and 0", r.held, llmlog.RecordedHeld()-base-rest)
	}
	if got := r.text([]byte(`[{"role":"user","content":"z"}]`), llmlog.RecordLostRequest); got != "" || len(r.lost) != 2 {
		t.Fatalf("a request text the full account cannot take: %q, lost %v", got, r.lost)
	}
	var row llmlog.GatewayRow
	r.mark(&row)
	if row.RecordLostCause != llmlog.RecordLostMemory || len(row.RecordLost) != 2 {
		t.Fatalf("row lost %v cause %q, want both bodies, memory", row.RecordLost, row.RecordLostCause)
	}
}

func TestRecordingReleaseWithoutARow(t *testing.T) {
	base := llmlog.RecordedHeld()
	r := recording{on: true}
	r.addStream([]byte("data: a\n\n"))
	r.release()
	if r.held != 0 || llmlog.RecordedHeld() != base {
		t.Fatalf("release left %d held (account +%d)", r.held, llmlog.RecordedHeld()-base)
	}
	off := recording{}
	off.addStream([]byte("data: b\n\n"))
	var row llmlog.GatewayRow
	off.mark(&row)
	if off.stream.Len() != 0 || row.Recorded || llmlog.RecordedHeld() != base {
		t.Fatalf("a consumer without the switch captured %d bytes, row recorded %v", off.stream.Len(), row.Recorded)
	}
}

func TestForcedRowOfARecordingConsumerNamesWhatItLacks(t *testing.T) {
	for _, c := range []struct {
		pipeline string
		want     []string
	}{
		{chatPipeline, []string{llmlog.RecordLostRequest, llmlog.RecordLostResponse}},
		{embedPipeline, []string{llmlog.RecordLostRequest}},
		{rerankPipeline, []string{llmlog.RecordLostRequest, llmlog.RecordLostResponse}},
	} {
		row := llmlog.GatewayRow{Pipeline: c.pipeline}
		markForced(&row, true)
		if !row.Recorded || strings.Join(row.RecordLost, ",") != strings.Join(c.want, ",") || row.RecordLostCause != llmlog.RecordLostShutdown {
			t.Errorf("%s: forced row recorded %v lost %v cause %q, want %v shutdown", c.pipeline, row.Recorded, row.RecordLost, row.RecordLostCause, c.want)
		}
		plain := llmlog.GatewayRow{Pipeline: c.pipeline}
		markForced(&plain, false)
		if plain.Recorded || plain.RecordLost != nil {
			t.Errorf("%s: the forced row of a consumer without the switch is marked", c.pipeline)
		}
	}
}
