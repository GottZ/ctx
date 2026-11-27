package llmlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The rows of a consumer WITHOUT the recording switch (issue #571, promise 4)
// are byte-identical to the rows before it, under every gateway.body_logging
// value: testdata/gateway_unrecorded.golden was written by this test on the
// base commit 2cc39a21 (CTX_LLMLOG_GOLDEN_WRITE=<path>) and is compared here
// row by row — bodies as sha256 and length, every other column and
// metadata.gw as JSON. A change to the row of an unrecorded call shows as
// the first line that differs.

// goldenPolicies are the body_logging values a row can carry: the three
// policies, unset, and spellings the route never sends (GatewayEntry trims
// and lowers).
var goldenPolicies = []string{BodyLoggingNone, BodyLoggingErrors, BodyLoggingFull, "", " FULL ", "bogus"}

// goldenSensitivities are the required sensitivities of a row: every class,
// unset and an unknown word.
var goldenSensitivities = []string{"credentials", "personal", "internal", "public", "", "bogus"}

// goldenBodies are the request texts: short, a credentials pattern, past the
// 64 KiB cap, invalid UTF-8 and NUL.
func goldenBodies() map[string]string {
	return map[string]string{
		"short":  `[{"role":"user","content":"hi"}]`,
		"secret": `[{"role":"user","content":"password=hunter2-correct-horse"}]`,
		"70KiB":  `[{"role":"user","content":"` + strings.Repeat("abcdefgh", 70<<7) + `"}]`,
		"bytes":  "a\xffb\x00c" + strings.Repeat("é", 40000),
	}
}

// unrecordedMatrix is every row the golden holds, keyed by a stable name.
func unrecordedMatrix() (names []string, rows map[string]GatewayRow) {
	rows = map[string]GatewayRow{}
	bodies := goldenBodies()
	for _, pipe := range []string{PipelineGatewayChat, PipelineGatewayEmbeddings, PipelineGatewayRerank} {
		for _, pol := range goldenPolicies {
			for _, sens := range goldenSensitivities {
				for _, withErr := range []bool{false, true} {
					for _, bname := range []string{"short", "secret", "70KiB", "bytes"} {
						r := GatewayRow{
							Pipeline: pipe, ConsumerID: "0195a6f0-0000-7000-8000-00000000c571",
							Gateway: Gateway{Alias: "chat", Hop: 1, Usage: UsageBackend, Finish: FinishStop,
								ModelKeyID: "0195a6f0-0000-7000-8000-00000000a571", ClientReqID: "req-571"},
							Telemetry: Entry{Model: "m", Host: "h", PromptTokens: 7, RequiredSensitivity: sens,
								Metadata: map[string]any{"sub_batches": 1}},
							Attempts:    []WireAttempt{{Backend: "spark", Class: "ok"}},
							Request:     bodies[bname],
							Response:    `{"answer":"` + bname + `"}`,
							BodyLogging: pol,
						}
						if withErr {
							r.Err = &RedactedErr{Class: "server_fault", Status: 502, Code: "upstream_error"}
						}
						name := fmt.Sprintf("%s|%q|%q|err=%t|%s", pipe, pol, sens, withErr, bname)
						names = append(names, name)
						rows[name] = r
					}
				}
			}
		}
	}
	return names, rows
}

// goldenLine renders the row GatewayEntry builds: bodies as sha256:len, the
// rest as JSON.
func goldenLine(name string, e Entry) string {
	h := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return fmt.Sprintf("%s:%d", hex.EncodeToString(sum[:8]), len(s))
	}
	errText := ""
	if e.Err != nil {
		errText = e.Err.Error()
	}
	md, err := json.Marshal(e.Metadata)
	if err != nil {
		md = []byte("unmarshalable: " + err.Error())
	}
	return fmt.Sprintf("%s\tsys=%s user=%s resp=%s err=%q pipe=%s consumer=%s key=%q attempt=%d sens=%q md=%s",
		name, h(e.RequestSystem), h(e.RequestUser), h(e.ResponseContent), errText, e.Pipeline, e.ConsumerID,
		e.APIKeyID, e.Attempt, e.RequiredSensitivity, md)
}

func TestGatewayUnrecordedRowsMatchTheBase(t *testing.T) {
	names, rows := unrecordedMatrix()
	var b strings.Builder
	for _, n := range names {
		b.WriteString(goldenLine(n, GatewayEntry(rows[n])))
		b.WriteByte('\n')
	}
	if out := os.Getenv("CTX_LLMLOG_GOLDEN_WRITE"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %d rows to %s", len(names), out)
		return
	}
	raw, err := os.ReadFile("testdata/gateway_unrecorded.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	got := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("%d rows, the golden of the base holds %d", len(got), len(want))
	}
	diffs := 0
	for i := range want {
		if got[i] != want[i] {
			diffs++
			if diffs <= 5 {
				t.Errorf("row %d differs from the base:\n got  %s\n want %s", i, got[i], want[i])
			}
		}
	}
	if diffs > 0 {
		t.Fatalf("%d of %d unrecorded rows differ from the base", diffs, len(want))
	}
	t.Logf("%d unrecorded rows byte-identical to the base (2cc39a21)", len(want))
}
