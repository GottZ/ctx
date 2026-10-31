package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestCapBody: under and exactly at the cap the body reads through; one byte
// over is ErrResponseTooLarge, never a silent truncation.
func TestCapBody(t *testing.T) {
	for _, c := range []struct {
		body    string
		limit   int64
		wantErr bool
	}{
		{"abc", 4, false},
		{"abcd", 4, false},
		{"abcde", 4, true},
		{strings.Repeat("x", 1<<16), 1 << 10, true},
	} {
		got, err := io.ReadAll(CapBody(strings.NewReader(c.body), c.limit))
		if c.wantErr {
			if !errors.Is(err, ErrResponseTooLarge) {
				t.Errorf("len %d cap %d: err = %v, want ErrResponseTooLarge", len(c.body), c.limit, err)
			}
			if int64(len(got)) > c.limit {
				t.Errorf("len %d cap %d: read %d bytes past the cap", len(c.body), c.limit, len(got))
			}
			continue
		}
		if err != nil || string(got) != c.body {
			t.Errorf("len %d cap %d: got %q, %v", len(c.body), c.limit, got, err)
		}
	}
}

// TestCapBodyDecoder: json.Decoder surfaces the cap error itself, so the
// wire's "decode response: %w" wrap keeps errors.Is working.
func TestCapBodyDecoder(t *testing.T) {
	doc := `{"content":"` + strings.Repeat("a", 4096) + `"}`
	var v map[string]any
	err := json.NewDecoder(CapBody(strings.NewReader(doc), 1024)).Decode(&v)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("decode over the cap: err = %v, want ErrResponseTooLarge", err)
	}
}
