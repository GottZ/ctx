package oai_test

// The lane over the chat route (FOLGE-M M4, W02-07 × A03-W9): a consumer on
// the bulk lane is served in the external bulk class and answered with
// service_tier "flex"; the walk accepts the XB lease because the planner's
// own AllowedClasses() carries XB (chat.go reads them for WalkRules).

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1census"
)

// TestChatBulkLaneIsServedAsFlex: lane bulk ⇒ 200, service_tier "flex",
// exactly one acquire — in the external bulk class — one wire contact and
// one row whose dispatch class is external_bulk; the census over the route
// with the planner's AllowedClasses() holds. The urgent lane on the same rig
// is the comparison: service_tier "default", class external.
func TestChatBulkLaneIsServedAsFlex(t *testing.T) {
	for _, tc := range []struct {
		lane, tier string
		class      dispatch.Class
	}{
		{handler.LaneBulk, "flex", dispatch.ClassExternalBulk},
		{handler.LaneUrgent, "default", dispatch.ClassExternal},
	} {
		t.Run(tc.lane, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withLane(tc.lane))
			allowed := rig.planner.AllowedClasses()
			_ = rig.recorder.Take()
			a := rig.post(t, chatBody("chat"))
			if a.status != http.StatusOK {
				t.Fatalf("lane %s: %d %s, want 200", tc.lane, a.status, a.body)
			}
			var out struct {
				ServiceTier string `json:"service_tier"`
			}
			if err := json.Unmarshal(a.body, &out); err != nil {
				t.Fatalf("answer %q: %v", a.body, err)
			}
			if out.ServiceTier != tc.tier {
				t.Errorf("lane %s: service_tier %q, want %q", tc.lane, out.ServiceTier, tc.tier)
			}
			var classes []dispatch.Class
			for _, e := range rig.recorder.Take() {
				if e.Kind == v1census.KindAcquire || e.Kind == v1census.KindTry {
					classes = append(classes, e.Class)
				}
			}
			if len(classes) != 1 || classes[0] != tc.class {
				t.Errorf("lane %s: acquires in classes %v, want exactly one in %s", tc.lane, classes, tc.class)
			}
			if !slices.Contains(allowed, tc.class) {
				t.Errorf("lane %s: the planner's AllowedClasses() %v lack %s — the walk refuses its lease", tc.lane, allowed, tc.class)
			}
			if n := u.hits.Load(); n != 1 {
				t.Errorf("lane %s: wire hits = %d, want 1", tc.lane, n)
			}
			rows := rig.rows.all()
			if len(rows) != 1 {
				t.Fatalf("lane %s: %d rows, want 1", tc.lane, len(rows))
			}
			if got, want := rows[0].Telemetry.DispatchClass, tc.class.String(); got != want || rows[0].ConsumerID != rig.ca.Load().ConsumerID {
				t.Errorf("lane %s: row dispatch_class %q consumer %q, want %q and %q", tc.lane, got, rows[0].ConsumerID, want, rig.ca.Load().ConsumerID)
			}
			v1census.Assert(t, rig.router, rig.recorder, v1census.Options{
				Allowed:   allowed,
				Hosts:     []string{u.srv.URL},
				Admitting: []string{"POST /v1/chat/completions"},
				Body:      func(string, string) string { return chatBody("chat") },
			})
		})
	}
}
