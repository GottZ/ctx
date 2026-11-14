package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #201: `ctx get --include-archived <id>` is the operator's read of an
// archived block (the rollback path docs/operations.md names). The flag rides
// to manage get as include_archived; without it the request body keeps its
// exact pre-flag bytes.
func TestGetCmdIncludeArchived(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"without the flag", []string{"0197aaaa-0000-7000-8000-000000000001"},
			`{"action":"get","id":"0197aaaa-0000-7000-8000-000000000001"}`},
		{"with the flag", []string{"--include-archived", "0197aaaa-0000-7000-8000-000000000001"},
			`{"action":"get","id":"0197aaaa-0000-7000-8000-000000000001","include_archived":true}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = w.Write([]byte(`{"action":"get","success":true,"block":{"id":"x"}}`))
			}))
			defer srv.Close()

			cmd := getCmd(func() (*Client, error) {
				return &Client{BaseURL: srv.URL + "/", Key: "k", HTTPClient: srv.Client()}, nil
			})
			cmd.SetArgs(c.args)
			cmd.SetOut(io.Discard)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if gotPath != "/api/manage" {
				t.Errorf("path = %q, want /api/manage", gotPath)
			}
			// Compare as JSON documents: key order is the encoder's business.
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
