package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #219: `ctx search tags_all:a,b` posts POST /api/search's tags_all (the
// blocks that carry all of the tags) beside tags (at least one). Before the
// argument existed the CLI took it for the positional category. A search
// without it posts the bytes it always posted.
func TestSearchCmdTagsAll(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"tags_all", []string{"tags_all:acme,invoice"},
			`{"compact":true,"limit":10,"tags_all":["acme","invoice"]}`},
		{"tags and tags_all", []string{"notes", "tags:signed", "tags_all:acme,invoice"},
			`{"category":"notes","compact":true,"limit":10,"tags":["signed"],"tags_all":["acme","invoice"]}`},
		{"without tags_all", []string{"notes", "tags:signed"},
			`{"category":"notes","compact":true,"limit":10,"tags":["signed"]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = w.Write([]byte(`{"success":true,"count":0,"results":[]}`))
			}))
			defer srv.Close()

			cmd := searchCmd(func() (*Client, error) {
				return &Client{BaseURL: srv.URL + "/", Key: "k", HTTPClient: srv.Client()}, nil
			})
			cmd.SetArgs(c.args)
			cmd.SetOut(io.Discard)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if gotPath != "/api/search" {
				t.Errorf("path = %q, want /api/search", gotPath)
			}
			var got, want map[string]any
			if err := json.Unmarshal(gotBody, &got); err != nil {
				t.Fatalf("decode body %s: %v", gotBody, err)
			}
			_ = json.Unmarshal([]byte(c.want), &want)
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) != string(wb) {
				t.Errorf("body = %s, want %s", gb, wb)
			}
		})
	}
}
