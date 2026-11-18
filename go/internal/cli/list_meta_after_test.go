package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #212: a list-meta answer cut at the cap carries next_after, and
// `ctx list-meta --after '<next_after>'` posts it back as `after`. Without the
// flag the request keeps its exact pre-flag bytes; a value that is not JSON is
// refused before any request.
func TestListMetaCmdAfter(t *testing.T) {
	const cursor = `{"after_category":"b-same","after_title":"twin","after_scope":"private"}`
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"without the flag", nil, `{"action":"list-meta"}`, false},
		{"with the flag", []string{"--after", cursor},
			`{"action":"list-meta","after":` + cursor + `}`, false},
		{"not JSON", []string{"--after", "b-same/twin"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotBody []byte
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = w.Write([]byte(`{"action":"list-meta","success":true,"blocks":[]}`))
			}))
			defer srv.Close()

			cmd := listMetaCmd(func() (*Client, error) {
				return &Client{BaseURL: srv.URL + "/", Key: "k", HTTPClient: srv.Client()}, nil
			})
			cmd.SetArgs(c.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if c.wantErr {
				if err == nil || calls != 0 {
					t.Fatalf("err = %v, requests = %d; want a refusal before any request", err, calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("execute: %v", err)
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
