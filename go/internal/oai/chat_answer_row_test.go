package oai_test

// The row of a non-streamed answer carries what the client got (RC-D S2,
// cluster C8, FOLGE-S S4; condition of the main context for the outcome's
// []byte): since the route hands the rewrite's buffer to the outcome
// without a copy, nothing may touch the buffer after the write — a reuse, a
// pool or a zeroing would change the row's text. The pin compares the row's
// Response with the client's body (its newline cut) at a body under the log
// cap and its bodyLogTextMax prefix at a body over it.

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

func TestChatAnswerRowIsTheClientBody(t *testing.T) {
	for _, size := range []int{2 << 10, 200 << 10} {
		content := strings.Repeat("<ä>&", size/5)
		answer := `{"id":"u","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		up := newUpstream(t, answer)
		rig := newChatRig(t, []backends.Backend{up.row("good", backends.TrustFull, 10)}, 4, 1)
		rig.setSettings(func(s *oai.Settings) { s.BodyLogging = "full" })
		a := rig.post(t, chatBody("chat"))
		if a.status != http.StatusOK {
			t.Fatalf("answer %d: %s", a.status, a.body)
		}
		rows := rig.rows.all()
		if len(rows) != 1 {
			t.Fatalf("%d rows, want 1", len(rows))
		}
		client := bytes.TrimSuffix(a.body, []byte("\n"))
		want := client[:min(len(client), 64<<10+4)]
		if rows[0].Response != string(want) {
			n := min(len(rows[0].Response), 80)
			t.Errorf("size %d: the row's response (%d B, %q…) is not the client's body (%d B, %q…) — the answer's buffer changed after the write", size, len(rows[0].Response), rows[0].Response[:n], len(want), want[:min(len(want), 80)])
		}
	}
}
