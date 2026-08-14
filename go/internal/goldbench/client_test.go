package goldbench

import (
	"encoding/json"
	"testing"
)

// extra-body wird gemerged, Struct-Felder gewinnen bei Kollision — sonst
// könnte ein Extra-Feld still das Modell oder die Sampler überschreiben.
func TestExtraBodyMerge(t *testing.T) {
	c := &Client{model: "m", seed: 1}
	if err := c.SetExtraBody(`{"chat_template_kwargs":{"enable_thinking":false},"model":"HIJACK","top_k":20}`); err != nil {
		t.Fatal(err)
	}
	body := wireRequest{Model: "m", Temperature: 0.2}
	payload, _ := json.Marshal(body)
	merged := map[string]json.RawMessage{}
	_ = json.Unmarshal(payload, &merged)
	for k, v := range c.extraBody {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	out, _ := json.Marshal(merged)
	var final map[string]any
	_ = json.Unmarshal(out, &final)
	if final["model"] != "m" {
		t.Errorf("model überschrieben: %v", final["model"])
	}
	if final["top_k"] != float64(20) {
		t.Errorf("top_k fehlt: %v", final["top_k"])
	}
	if _, ok := final["chat_template_kwargs"]; !ok {
		t.Error("chat_template_kwargs fehlt")
	}
	if err := c.SetExtraBody(`kein json`); err == nil {
		t.Error("invalides JSON nicht abgelehnt")
	}
}

// Think-Stripping: Blöcke raus, Antwort bleibt; unterminierter Block
// (Truncation mitten im Denken) hinterlässt leeren Content statt Denk-Müll.
func TestStripThink(t *testing.T) {
	cases := []struct{ in, want string; stripped bool }{
		{`{"label": "ok"}`, `{"label": "ok"}`, false},
		{"<think>hmm, also...</think>\n{\"label\": \"ok\"}", `{"label": "ok"}`, true},
		{"<think>abgeschnitten mitten im Denk", "", true},
		{"vorspann <think>a</think> mitte <think>b</think> ende", "vorspann  mitte  ende", true},
	}
	for _, c := range cases {
		got, stripped := stripThink(c.in)
		if got != c.want || stripped != c.stripped {
			t.Errorf("stripThink(%q) = (%q, %v), erwartet (%q, %v)", c.in, got, stripped, c.want, c.stripped)
		}
	}
}

// Die Fail-Metrik hängt an dieser Klassifikation: eine Context-Ablehnung,
// die als Transport-Fehler durchgeht, macht das Serving-Limit unsichtbar.
func TestIsContextOverflowBody(t *testing.T) {
	overflow := []string{
		// llama.cpp (server.cpp, mehrere Versionen)
		`{"error":{"code":400,"message":"the request exceeds the available context size. try increasing the context size or enable context shift","type":"invalid_request_error"}}`,
		`{"error":{"code":400,"message":"request exceeds context size limit","type":"exceed_context_size_error"}}`,
		`{"error":{"message":"prompt is too long (5000 tokens > n_ctx 4096)"}}`,
		// OpenAI-kompatibel
		`{"error":{"message":"This model's maximum context length is 8192 tokens.","code":"context_length_exceeded"}}`,
	}
	for _, body := range overflow {
		if !isContextOverflowBody(body) {
			t.Errorf("nicht als Context-Overflow erkannt: %s", body)
		}
	}
	other := []string{
		`{"error":{"message":"model is loading"}}`,
		`upstream connect error or disconnect/reset before headers`,
		`{"error":{"message":"invalid api key"}}`,
		``,
	}
	for _, body := range other {
		if isContextOverflowBody(body) {
			t.Errorf("fälschlich als Context-Overflow erkannt: %s", body)
		}
	}
}
