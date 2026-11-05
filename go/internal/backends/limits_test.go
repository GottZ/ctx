package backends

import "testing"

func TestChatMaxTokens(t *testing.T) {
	cases := []struct {
		name   string
		limits map[string]any
		want   int
	}{
		{"absent", nil, 0},
		{"json float", map[string]any{"chat_max_tokens": float64(512)}, 512},
		{"int", map[string]any{"chat_max_tokens": 512}, 512},
		{"int64", map[string]any{"chat_max_tokens": int64(512)}, 512},
		{"other key ignored", map[string]any{"foo": 1.0}, 0},
		{"non-number ignored", map[string]any{"chat_max_tokens": "512"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := Backend{Limits: c.limits}
			if got := b.ChatMaxTokens(); got != c.want {
				t.Errorf("ChatMaxTokens = %d; want %d", got, c.want)
			}
		})
	}
}
