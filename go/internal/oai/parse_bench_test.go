package oai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
)

// parseBenchBody is the request of the O4 measurement (bau report W02-07
// deviation 12): msgs messages of size bytes, 17 tools, two stops — 3.5 KB,
// 55 KB, 828 KB at the three sizes below.
func parseBenchBody(msgs, size int) []byte {
	type m struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var ms []m
	word := "the quick brown fox jumps over the lazy dog and keeps running through the field "
	for i := 0; i < msgs; i++ {
		ms = append(ms, m{Role: []string{"user", "assistant"}[i%2], Content: strings.Repeat(word, size/len(word)+1)[:size]})
	}
	tools := `[` + strings.Repeat(`{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string","enum":["a","b","c"]}}}}},`, 16) + `{"type":"function","function":{"name":"x"}}]`
	b, _ := json.Marshal(map[string]any{"model": "chat", "messages": ms, "tools": json.RawMessage(tools), "stop": []string{"a", "b"}})
	return b
}

// BenchmarkChatParse is the parse of one request (top level + policy half)
// — the cost H-2 adds is measured against the parse without it.
func BenchmarkChatParse(b *testing.B) {
	a := modelalias.Alias{Name: "chat", Scope: backends.GlobalScope}
	s := Settings{MaxRequestBytes: 8 << 20}
	for _, c := range []struct{ msgs, size int }{{4, 256}, {50, 1024}, {200, 4096}} {
		body := parseBenchBody(c.msgs, c.size)
		b.Run(fmt.Sprintf("bytes=%d", len(body)), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				req, perr := parseTopLevel(body)
				if perr != nil {
					b.Fatal(perr.message)
				}
				if perr := req.parsePolicy(limitsOf(a, s), nil); perr != nil {
					b.Fatal(perr.message)
				}
			}
		})
	}
}
