package oai

import (
	"strings"
	"testing"
)

// BenchmarkChatParseFolgeP is the cost line of FOLGE-P: the parse (both
// halves, and for FOLGE-P the route's nesting count before it) of a typical
// request — four messages, 2 KiB — and of 8 MiB of text,
// FOLGE-P against the base parse of 4839a958 (parse_oracle_test.go) in one
// binary.
func BenchmarkChatParseFolgeP(b *testing.B) {
	l := diffLimits[0]
	var typical strings.Builder
	typical.WriteString(`{"model":"chat","temperature":0.7,"max_tokens":512,"messages":[`)
	for i, role := range []string{"system", "user", "assistant", "user"} {
		if i > 0 {
			typical.WriteByte(',')
		}
		typical.WriteString(`{"role":"` + role + `","content":"` + strings.Repeat("lorem ipsum dolor sit amet ", 18) + `"}`)
	}
	typical.WriteString(`]}`)
	text := `{"model":"chat","messages":[{"role":"user","content":"` + strings.Repeat("a", 8<<20-64) + `"}]}`
	for _, c := range []struct{ name, body string }{{"typical_2KiB", typical.String()}, {"text_8MiB", text}} {
		body := []byte(c.body)
		b.Run(c.name+"/folgep", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				_ = jsonStateBytes(body) // the route counts the nesting before the parse
				if perr := pinParse(body, l); perr != nil {
					b.Fatal(perr.message)
				}
			}
		})
		b.Run(c.name+"/base", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				req, perr := baseParseTopLevel(body)
				if perr == nil {
					perr = req.parsePolicy(l)
				}
				if perr != nil {
					b.Fatal(perr.message)
				}
			}
		})
	}
}
