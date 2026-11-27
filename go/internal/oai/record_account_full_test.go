package oai_test

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestRecordedChatWithAFullAccount (issue #571, promise 3 under memory
// pressure): when the recording account cannot take a recorded request's
// texts, the request is answered exactly as without the switch and its row
// is written without them, naming both (record_lost, cause memory) — the
// account is never the request's budget.
func TestRecordedChatWithAFullAccount(t *testing.T) {
	answer := `{"id":"gen-1","object":"chat.completion","created":1700000000,"model":"M",` +
		`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`
	body := `{"model":"pub","messages":[{"role":"user","content":"` + strings.Repeat("w", 4096) + `"}]}`
	u := newUpstream(t, answer)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	rig.ca.Store(recordingConsumer(false))
	off := rig.post(t, body)
	// Fill the account to its last byte.
	filled := int64(0)
	for step := int64(1 << 26); step > 0; step /= 2 {
		for llmlog.ReserveRecorded(step) {
			filled += step
		}
	}
	defer llmlog.ReleaseRecorded(filled)
	rig.ca.Store(recordingConsumer(true))
	on := rig.post(t, body)
	rows := rig.rows.all()
	defer releaseRows(rows)
	if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
		t.Fatalf("status off %d on %d, answers equal %v — a full account must not change the answer", off.status, on.status, sameAnswer(off.body, on.body))
	}
	if len(u.raws) != 2 || string(u.raws[0]) != string(u.raws[1]) {
		t.Errorf("the upstream bodies differ with a full account")
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	r := rows[1]
	stored := llmlog.RecordedGatewayEntry(r)
	if !r.Recorded || stored.RequestUser != "" || stored.ResponseContent != "" || r.RecordedHeld != 0 ||
		strings.Join(r.RecordLost, ",") != "request,response" || r.RecordLostCause != llmlog.RecordLostMemory {
		t.Errorf("row with a full account: recorded %v, %d/%d body bytes, holds %d, lost %v cause %q — want recorded, no bodies, nothing held, both lost, memory",
			r.Recorded, len(stored.RequestUser), len(stored.ResponseContent), r.RecordedHeld, r.RecordLost, r.RecordLostCause)
	}
}
