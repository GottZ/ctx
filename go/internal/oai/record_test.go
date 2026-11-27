package oai_test

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// The recording switch on the routes (issue #571): a consumer with
// RecordBodies has its row carry the whole request and the whole answer the
// client received — a non-streamed chat answer as written, a stream's relayed
// data lines, the inputs of embeddings and the query and documents of rerank
// — under every gateway.body_logging value and every alias class; and the
// request path is the same with and without the switch: the same upstream
// body (so the same chain, scan and limits), the same status, the same answer
// up to the request's own id and clock.

// recSecret is a credentials pattern in a prompt: the sensitivity scan raises
// a request carrying it to credentials, where the body policy stores nothing.
const recSecret = "password=hunter2-recorded-571"

// recordingConsumer is the rig's consumer with the switch set to on.
func recordingConsumer(on bool) *auth.ConsumerAuth {
	ca := urgentApp()
	ca.RecordBodies = on
	return ca
}

// releaseRows gives the rows' reservations back (the rig's sink is no writer).
func releaseRows(rows []llmlog.GatewayRow) {
	for _, r := range rows {
		llmlog.ReleaseRecorded(r.RecordedHeld)
	}
}

// answerIdentity drops what differs between two requests by construction:
// the answer's id and created stamp.
var answerIdentity = regexp.MustCompile(`"(id|created)":("[^"]*"|\d+)`)

func sameAnswer(a, b []byte) bool {
	return answerIdentity.ReplaceAllString(string(a), "") == answerIdentity.ReplaceAllString(string(b), "")
}

func TestRecordedChatKeepsWholeBodiesAndChangesNoAnswer(t *testing.T) {
	long := strings.Repeat("abcdefgh", 12<<10) // 96 KiB, past the 64 KiB cap
	answer := `{"id":"gen-1","object":"chat.completion","created":1700000000,"model":"M",` +
		`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"` + strings.Repeat("y", 80<<10) + `"}}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`
	body := `{"model":"pub","messages":[{"role":"user","content":"` + recSecret + " " + long + `"}]}`
	messages := `[{"role":"user","content":"` + recSecret + " " + long + `"}]`
	for _, pol := range []string{llmlog.BodyLoggingNone, llmlog.BodyLoggingErrors, llmlog.BodyLoggingFull} {
		t.Run(pol, func(t *testing.T) {
			u := newUpstream(t, answer)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			rig.setSettings(func(s *oai.Settings) { s.BodyLogging = pol })
			rig.ca.Store(recordingConsumer(false))
			off := rig.post(t, body)
			rig.ca.Store(recordingConsumer(true))
			on := rig.post(t, body)
			rows := rig.rows.all()
			defer releaseRows(rows)
			if off.status != 200 || on.status != 200 {
				t.Fatalf("status off %d on %d, want 200 both: %.200s / %.200s", off.status, on.status, off.body, on.body)
			}
			if !sameAnswer(off.body, on.body) {
				t.Errorf("the answer differs with the switch on")
			}
			if len(u.raws) != 2 || string(u.raws[0]) != string(u.raws[1]) {
				t.Errorf("the upstream bodies differ with the switch on (%d calls)", len(u.raws))
			}
			if len(rows) != 2 {
				t.Fatalf("%d rows, want 2", len(rows))
			}
			ro, rn := rows[0], rows[1]
			// The scan raised both to credentials: the switch changes no class.
			if ro.Telemetry.RequiredSensitivity != string(backends.SensCredentials) || rn.Telemetry.RequiredSensitivity != ro.Telemetry.RequiredSensitivity {
				t.Errorf("required sensitivity off %q on %q, want credentials both", ro.Telemetry.RequiredSensitivity, rn.Telemetry.RequiredSensitivity)
			}
			if ro.Recorded || ro.RecordedHeld != 0 {
				t.Errorf("the row of the consumer without the switch is marked recorded")
			}
			if stored := llmlog.GatewayEntry(ro); stored.RequestUser != "" || stored.ResponseContent != "" {
				t.Errorf("the unrecorded credentials row stores %d/%d bytes, want 0/0 (sealed)", len(stored.RequestUser), len(stored.ResponseContent))
			}
			if !rn.Recorded || rn.RecordLost != nil {
				t.Fatalf("the recording consumer's row: recorded %v lost %v, want recorded without a loss", rn.Recorded, rn.RecordLost)
			}
			stored := llmlog.RecordedGatewayEntry(rn)
			if stored.RequestUser != messages {
				t.Errorf("recorded request %d bytes, want the whole message array (%d bytes)", len(stored.RequestUser), len(messages))
			}
			if !strings.HasSuffix(string(on.body), "\n") || stored.ResponseContent != strings.TrimSuffix(string(on.body), "\n") {
				t.Errorf("recorded answer %d bytes, want the whole answer the client received (%d bytes)", len(stored.ResponseContent), len(on.body))
			}
			if want := int64(len(stored.RequestUser) + len(stored.ResponseContent)); rn.RecordedHeld != want {
				t.Errorf("the row holds %d bytes of the recording account, want %d", rn.RecordedHeld, want)
			}
		})
	}
}

// TestRecordedStreamKeepsTheRelayedLines: a stream's recorded answer is the
// data lines the relay handed to the client, in order — the client's body is
// exactly those lines and the closing [DONE] — and the client gets the same
// stream with and without the switch.
func TestRecordedStreamKeepsTheRelayedLines(t *testing.T) {
	sh := streamShapes[0]
	u := newUpstream(t, "")
	u.setRespond(sseRespond(sh.steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	srv := serveHTTP(t, rig.router)
	rig.ca.Store(recordingConsumer(false))
	off := postStream(context.Background(), t, srv, streamBody("chat"))
	rig.ca.Store(recordingConsumer(true))
	on := postStream(context.Background(), t, srv, streamBody("chat"))
	rows := rig.rows.all()
	defer releaseRows(rows)
	checkRelay(t, on)
	if !sameAnswer([]byte(off.raw), []byte(on.raw)) {
		t.Errorf("the stream differs with the switch on:\n off %q\n on  %q", off.raw, on.raw)
	}
	if len(rows) != 2 || rows[0].Recorded || rows[0].Response != "" || !rows[1].Recorded {
		t.Fatalf("rows %d: off recorded %v response %d bytes, on recorded %v", len(rows), rows[0].Recorded, len(rows[0].Response), rows[1].Recorded)
	}
	got := llmlog.RecordedGatewayEntry(rows[1]).ResponseContent
	if got == "" || got+"data: [DONE]\n\n" != on.raw {
		t.Errorf("recorded stream answer %q, want the client's body without its closing [DONE]: %q", got, on.raw)
	}
	if !strings.Contains(llmlog.RecordedGatewayEntry(rows[1]).RequestUser, `"content":"hi"`) {
		t.Errorf("recorded stream request %q, want the message array", rows[1].Request)
	}
}

func TestRecordedEmbeddingsKeepTheWholeInput(t *testing.T) {
	inputs := []string{recSecret + " " + strings.Repeat("e", 70<<10), "second " + strings.Repeat("f", 1000)}
	body := embedBody("emb1", inputs)
	for _, pol := range []string{llmlog.BodyLoggingNone, llmlog.BodyLoggingFull} {
		t.Run(pol, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1)
			rig.setSettings(func(s *oai.Settings) { s.BodyLogging = pol })
			rig.ca.Store(recordingConsumer(false))
			off := rig.postEmbed(t, body)
			rig.ca.Store(recordingConsumer(true))
			on := rig.postEmbed(t, body)
			rows := rig.rows.all()
			defer releaseRows(rows)
			if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
				t.Fatalf("status off %d on %d (answers equal %v): %.300s", off.status, on.status, sameAnswer(off.body, on.body), on.body)
			}
			if len(rows) != 2 || rows[0].Recorded || !rows[1].Recorded {
				t.Fatalf("rows %d, recorded off/on want false/true", len(rows))
			}
			in, _ := json.Marshal(inputs)
			stored := llmlog.RecordedGatewayEntry(rows[1])
			if stored.RequestUser != string(in) || stored.ResponseContent != "" {
				t.Errorf("recorded embeddings row: request %d bytes (want the whole input, %d), answer %d bytes (want none — the vectors are not kept)",
					len(stored.RequestUser), len(in), len(stored.ResponseContent))
			}
		})
	}
}

func TestRecordedRerankKeepsQueryAndDocuments(t *testing.T) {
	docs := []string{recSecret + " " + strings.Repeat("d", 70<<10), "other"}
	query := "which document? " + strings.Repeat("q", 100)
	body := rerankBody("rrpub", query, docs)
	e := newRerankEngine(t)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1)
	rig.ca.Store(recordingConsumer(false))
	off := rig.postRerank(t, "/v1/rerank", body)
	rig.ca.Store(recordingConsumer(true))
	on := rig.postRerank(t, "/v1/rerank", body)
	rows := rig.rows.all()
	defer releaseRows(rows)
	if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
		t.Fatalf("status off %d on %d (answers equal %v): %.300s", off.status, on.status, sameAnswer(off.body, on.body), on.body)
	}
	if len(rows) != 2 || rows[0].Recorded || !rows[1].Recorded {
		t.Fatalf("rows %d, recorded off/on want false/true", len(rows))
	}
	q, _ := json.Marshal(query)
	d, _ := json.Marshal(docs)
	stored := llmlog.RecordedGatewayEntry(rows[1])
	if want := string(q) + "\n" + string(d); stored.RequestUser != want {
		t.Errorf("recorded rerank request %d bytes, want query, line break and documents (%d bytes)", len(stored.RequestUser), len(want))
	}
	if stored.ResponseContent != string(on.body) || rows[0].Response != "" {
		t.Errorf("recorded rerank answer %q, want the answer the client received %q (the unrecorded row keeps none: %q)",
			stored.ResponseContent, on.body, rows[0].Response)
	}
}
