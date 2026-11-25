package cli

import (
	"testing"
)

// TestEmbedBackfillReleaseSendsTheLimitAsGiven: `ctx admin embed-backfill
// release --limit N` sends the limit it was given, so the server refuses a
// negative one (issue #536). Before, the CLI sent a limit only when it was
// above 0: --limit -1 and --limit 0 left the field out, and the server paged
// by its maximum. Without the flag the field stays out.
func TestEmbedBackfillReleaseSendsTheLimitAsGiven(t *testing.T) {
	rs := newRecordedServer(t, func(map[string]any) string {
		return `{"success":true,"dry_run":true,"counted":0,"confirm_memos":0,"as_of":"2026-11-25T00:00:00Z","held":0}`
	})
	for i, c := range []struct {
		args []string
		want any // nil: no limit field
	}{
		{[]string{"--limit", "-1"}, float64(-1)},
		{[]string{"--limit", "0"}, float64(0)},
		{[]string{"--limit", "5"}, float64(5)},
		{nil, nil},
	} {
		args := append([]string{"admin", "embed-backfill", "release", "--class", "oversize"}, c.args...)
		_, _, _ = runCLI(t, nil, args...)
		reqs := rs.requests()
		if len(reqs) != i+1 {
			t.Fatalf("ctx %v: %d requests in all, want %d", args, len(reqs), i+1)
		}
		data, _ := reqs[i]["data"].(map[string]any)
		got, ok := data["limit"]
		switch {
		case c.want == nil && ok:
			t.Errorf("ctx %v sent limit %v, want no limit field", args, got)
		case c.want != nil && (!ok || got != c.want):
			t.Errorf("ctx %v sent data %v, want limit %v", args, data, c.want)
		}
	}
}
