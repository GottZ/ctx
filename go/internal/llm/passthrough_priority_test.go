package llm

// Priority strip (wave A03-W8, design/03 §5 X4, design/04 I-H5): a client's
// body field priority — and every other engine scheduling field — never
// reaches the wire; ctx alone sets the engine priority, from its own band
// (A03-W13). Every other client PASS key passes byte for byte (golden).

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestBuildUpstreamBodyStripsPriority(t *testing.T) {
	b := ptRow("http://x")
	client := map[string]json.RawMessage{
		"priority":           json.RawMessage(`9000000000000000000`),
		"id_slot":            json.RawMessage(`3`),
		"bootstrap_host":     json.RawMessage(`"10.0.0.9"`),
		"bootstrap_port":     json.RawMessage(`8998`),
		"bootstrap_room":     json.RawMessage(`42`),
		"kv_transfer_params": json.RawMessage(`{"do_remote_prefill":true}`),
		// PASS keys — unchanged on the wire (golden below).
		"temperature":       json.RawMessage(`0.25`),
		"top_p":             json.RawMessage(`0.9`),
		"seed":              json.RawMessage(`9007199254740993`),
		"stop":              json.RawMessage(`["</s>","\n\n"]`),
		"logit_bias":        json.RawMessage(`{"50256":-100}`),
		"response_format":   json.RawMessage(`{"type":"json_object"}`),
		"tools":             json.RawMessage(`[{"type":"function","function":{"name":"f","parameters":{}}}]`),
		"tool_choice":       json.RawMessage(`"auto"`),
		"presence_penalty":  json.RawMessage(`0.5`),
		"frequency_penalty": json.RawMessage(`0.1`),
	}
	raw, err := BuildUpstreamBody(ptSpec(client), &b)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"priority", "id_slot", "bootstrap_host", "bootstrap_port", "bootstrap_room", "kv_transfer_params"} {
		if v, ok := got[k]; ok {
			t.Errorf("engine scheduling field %q reached the wire: %s", k, v)
		}
	}
	// Golden: every PASS key carries the client's value. Compared as JSON
	// values — the wire re-encodes RawMessage with HTML escaping ("</s>" ⇒
	// "\u003c/s\u003e"), the same string to every JSON parser.
	for _, k := range []string{"temperature", "top_p", "seed", "stop", "logit_bias", "response_format", "tools", "tool_choice", "presence_penalty", "frequency_penalty"} {
		var want, have any
		dw, dh := json.NewDecoder(bytesReader(client[k])), json.NewDecoder(bytesReader(got[k]))
		dw.UseNumber()
		dh.UseNumber()
		if err := dw.Decode(&want); err != nil {
			t.Fatalf("client %s: %v", k, err)
		}
		if err := dh.Decode(&have); err != nil || !reflect.DeepEqual(want, have) {
			t.Errorf("PASS key %q on the wire = %s, want the client's value %s", k, got[k], client[k])
		}
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
