package llm

// The differential gate of FOLGE-S on the relay reader (RC-D S1, cluster
// C8): every SSE stream of the corpus hands onFrame the base's payloads and
// ends with the base's result and error, through sseRelay.run — the loop S2
// moves onto readSSELineInto/decodeChunkMeta and a []byte frame. Golden file
// written from the base (CTX_FOLGE_S_GOLDEN=update); a preservation pin,
// green on the S1 stubs by construction.

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const folgeSRelayGolden = "testdata/folge_s_relay_trace.json"

type folgeSRelayCase struct {
	Name     string   `json:"name"`
	Frames   []string `json:"frames"`
	Count    int      `json:"count"`
	Prompt   int      `json:"prompt"`
	Complete int      `json:"completion"`
	Finish   string   `json:"finish"`
	Model    string   `json:"model"`
	ID       string   `json:"id"`
	Err      string   `json:"err,omitempty"`
}

func folgeSRelayCorpus() [][2]string {
	c := func(s string) string {
		return `{"id":"I","model":"M","choices":[{"index":0,"delta":{"content":"` + s + `"},"finish_reason":null}]}`
	}
	fin := `{"id":"I2","model":"M2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`
	return [][2]string{
		{"plain", "data: " + c("a") + "\n\ndata: " + fin + "\n\ndata: [DONE]\n\n"},
		{"crlf", "data: " + c("a") + "\r\n\r\ndata: " + fin + "\r\n\r\ndata: [DONE]\r\n\r\n"},
		{"no space", "data:" + c("a") + "\n\ndata:" + fin + "\n\n"},
		{"comments and events", ": OPENROUTER PROCESSING\n\nevent: message\ndata: " + c("b") + "\nid: 7\n\n: keepalive\n\ndata: " + fin + "\n\ndata: [DONE]\n\n"},
		{"multi-line data", "data: {\"id\":\"I\",\"model\":\"M\",\ndata: \"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"},
		{"unterminated final frame", "data: " + c("a") + "\n\ndata: " + fin},
		{"timings only", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t\"},\"finish_reason\":\"stop\"}],\"timings\":{\"prompt_n\":9,\"predicted_n\":2}}\n\ndata: [DONE]\n\n"},
		{"error object", "data: " + c("a") + "\n\ndata: {\"error\":{\"code\":502,\"message\":\"upstream died\"}}\n\n"},
		{"finish error", "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"error\"}]}\n\n"},
		{"no end", "data: " + c("a") + "\n\n"},
		{"malformed", "data: {\"choices\":[\n\n"},
		{"empty data lines", "data:\ndata:\n\ndata: " + fin + "\n\ndata: [DONE]\n\n"},
		{"done with spaces", "data: " + fin + "\n\ndata:  [DONE]  \n\n"},
		{"over the cap", "data: " + c(strings.Repeat("z", 300)) + "\n\n"},
		{"unicode", "data: " + c(`\u00e4 ä 😀 <&>`) + "\n\ndata: " + fin + "\n\ndata: [DONE]\n\n"},
	}
}

func folgeSRelayRun() []folgeSRelayCase {
	var out []folgeSRelayCase
	for _, s := range folgeSRelayCorpus() {
		rc := folgeSRelayCase{Name: s[0], Frames: []string{}}
		rl := &sseRelay{onFrame: func(p []byte) error { rc.Frames = append(rc.Frames, string(p)); return nil }, data: sseFrame{limit: 256}}
		err := rl.run(strings.NewReader(s[1]), 256)
		res := rl.result()
		rc.Count, rc.Prompt, rc.Complete, rc.Finish, rc.Model, rc.ID = res.Frames, res.PromptTokens, res.CompletionTokens, res.FinishReason, res.ServedModel, res.ProviderRequestID
		if err != nil {
			rc.Err = err.Error()
		}
		out = append(out, rc)
	}
	return out
}

func TestSSERelayTraceIsTheBase(t *testing.T) {
	got := folgeSRelayRun()
	if os.Getenv("CTX_FOLGE_S_GOLDEN") == "update" {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(got); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(folgeSRelayGolden, b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d streams)", folgeSRelayGolden, len(got))
		return
	}
	raw, err := os.ReadFile(folgeSRelayGolden)
	if err != nil {
		t.Fatal(err)
	}
	var want []folgeSRelayCase
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d streams, golden %d", len(got), len(want))
	}
	for i := range got {
		g, _ := json.Marshal(got[i])
		w, _ := json.Marshal(want[i])
		if !bytes.Equal(g, w) {
			t.Errorf("stream %q:\n got %s\nbase %s", got[i].Name, g, w)
		}
	}
}
