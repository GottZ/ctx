package goldbench

import "testing"

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
