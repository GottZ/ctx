package handler

// A03-W3 gates on the handler side (design/03 §4.14, §5 X23; DECISIONS A-E5
// no. 3): the server-admin status carries the external fields, the tenant
// view never sees external load (Busy/Depth from internal sizes only, no
// external bucket — not by a key comparison but by class), and the
// foreign-load detector keeps reading the SUM of internal and external held
// leases (otherwise external load on a probed GPU would read as foreign).

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// externalSnapshot is one target under external load: two external leases
// (Held counts them, HeldInternal does not), five external waiters, and two
// external buckets — one of them deliberately keyed like the caller's own
// fair key, so only a class filter (never a key comparison) keeps it out of
// the tenant view.
func externalSnapshot() dispatch.Snapshot {
	return dispatch.Snapshot{
		Enabled: true,
		Targets: []dispatch.TargetSnapshot{{
			Origin:           "http://spark:30000",
			Slots:            4,
			Held:             2,
			Inflight:         2,
			HeldInternal:     0,
			HeldExternal:     2,
			InflightInternal: 0,
			InternalReserved: 2,
			ExternalOffered:  true,
			ExternalCapacity: 2,
			External:         dispatch.WaitStats{Waiting: 5, OldestWait: 700 * time.Millisecond, P95Wait: 300 * time.Millisecond, MaxWait: 800 * time.Millisecond, Samples: 9},
			ExternalRejects:  dispatch.ExternalRejectStats{Cap: 3, NotOffered: 4},
			Buckets: []dispatch.BucketSnapshot{
				{FairKey: "_consumer:foreign-app", TenantID: "tenant-other", Class: dispatch.ClassExternal, Waiting: 4, Inflight: 1},
				{FairKey: "private", TenantID: "tenant-1", Class: dispatch.ClassExternal, Waiting: 1, Inflight: 1},
			},
		}},
	}
}

// TestBuildDispatchAdminExposesExternal is the positive exposure probe of the
// server-admin shape: every external field of §4.14 is mapped.
func TestBuildDispatchAdminExposesExternal(t *testing.T) {
	ds := buildDispatchAdmin(externalSnapshot(), false, nil, nil, nil, nil)
	tgt := ds.Targets[0]
	if tgt.Held != 2 || tgt.HeldInternal != 0 || tgt.HeldExternal != 2 || tgt.InflightInternal != 0 {
		t.Errorf("held split wrong: held=%d int=%d ext=%d inflight_int=%d", tgt.Held, tgt.HeldInternal, tgt.HeldExternal, tgt.InflightInternal)
	}
	if tgt.InternalReserved != 2 || !tgt.ExternalOffered || tgt.ExternalCapacity != 2 {
		t.Errorf("partition fields wrong: reserved=%d offered=%v capacity=%d", tgt.InternalReserved, tgt.ExternalOffered, tgt.ExternalCapacity)
	}
	if tgt.External.Waiting != 5 || tgt.External.P95WaitMs != 300 || tgt.External.MaxWaitMs != 800 || tgt.External.Samples != 9 || tgt.External.OldestWaitMs != 700 {
		t.Errorf("external wait stats wrong: %+v", tgt.External)
	}
	if tgt.ExternalRejects.Cap != 3 || tgt.ExternalRejects.NotOffered != 4 {
		t.Errorf("external rejects wrong: %+v", tgt.ExternalRejects)
	}
	if len(tgt.Buckets) != 2 || tgt.Buckets[0].Class != "external" || tgt.Buckets[0].TenantID != "tenant-other" || tgt.Buckets[0].FairKey != "_consumer:foreign-app" {
		t.Errorf("external buckets wrong: %+v", tgt.Buckets)
	}
	b, _ := json.Marshal(ds)
	for _, want := range []string{`"held_external":2`, `"external_capacity":2`, `"not_offered":4`, `"class":"external"`, `"tenant_id":"tenant-other"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("admin payload lacks %s: %s", want, b)
		}
	}
}

// TestBuildDispatchTenantExcludesExternal is the X23 negative probe: external
// leases and waiters change neither Busy nor Depth of the tenant view, and no
// external bucket reaches it — not even one whose key equals the caller's.
func TestBuildDispatchTenantExcludesExternal(t *testing.T) {
	ts := buildDispatchTenant(externalSnapshot(), map[string]bool{"http://spark:30000": true}, "private")
	tt := ts.Targets[0]
	if tt.Busy || tt.Depth != "leer" {
		t.Errorf("external load leaks into the tenant occupancy: busy=%v depth=%q, want false/leer", tt.Busy, tt.Depth)
	}
	if tt.OwnWaiting != 0 || tt.OwnInflight != 0 || tt.OwnOldestWaitMs != 0 {
		t.Errorf("an external bucket reached the tenant view as own detail: %+v", tt)
	}
	b, _ := json.Marshal(ts)
	for _, forbidden := range []string{"_consumer:", "foreign-app", "tenant-other", "held_external", "external"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("tenant payload contains %q: %s", forbidden, b)
		}
	}
}

// TestForeignLoadDetectorReadsInternalPlusExternal is the A-E5 detector
// harness with a REAL dispatcher and the production principal hook: two
// external and one background lease on a probed origin, the fake engine
// reports three requests running — three rounds stay clean with delta 0. A
// Held that dropped the external leases would read 1 and suspect foreign load.
func TestForeignLoadDetectorReadsInternalPlusExternal(t *testing.T) {
	installRequestPrincipalHook(t)
	fake := &flCounter{}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer srv.Close()
	origin, _ := dispatch.NormalizeOrigin(srv.URL)

	disp := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	disp.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 4, InternalReserved: 2}}})
	req := func(c dispatch.Class) dispatch.Request {
		return dispatch.Request{Target: dispatch.Target{Origin: srv.URL}, Class: c, Role: "chat"}
	}
	// Bounded: an acquire that wrongly queues fails the test instead of
	// hanging it (the 3 s deadline outlives the three rounds).
	bounded, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	consumer := func(id string) context.Context {
		return withConsumerAuth(bounded, &auth.ConsumerAuth{
			ModelKeyID: "mk-" + id, ConsumerID: id, TenantID: "tenant-1", TenantScope: "scope-1",
			Kind: auth.KindApp, FairWeight: 10, IsValid: true,
		})
	}
	var leases []*dispatch.Lease
	for _, ctx := range []context.Context{consumer("c1"), consumer("c2")} {
		l, _, err := disp.Acquire(ctx, req(dispatch.ClassExternal))
		if err != nil {
			t.Fatalf("external acquire through the production hook: %v", err)
		}
		leases = append(leases, l)
	}
	bl, _, err := disp.Acquire(bounded, req(dispatch.ClassBackground))
	if err != nil {
		t.Fatalf("background acquire: %v", err)
	}
	leases = append(leases, bl)
	defer func() {
		for _, l := range leases {
			l.Release()
		}
	}()

	d := newForeignLoadDetector(nil, disp)
	d.logger = slog.New(slog.DiscardHandler)
	targets := []foreignLoadTarget{{origin: origin, kind: backends.ForeignLoadProbeSGLang, names: []string{"spark-chat"}, rollupKeys: []string{"spark-chat"}}}
	d.mu.Lock()
	d.syncLocked(targets)
	d.mu.Unlock()
	set := foreignLoadSettings{timeout: 2 * time.Second, hysteresis: 2, minAbs: 5, minRatio: 0.01, grace: 7 * time.Second}
	fake.set(0, 3)
	for round := 1; round <= 3; round++ {
		d.round(context.Background(), targets, set)
		row := (*d.section())[0]
		if row.Held == nil || *row.Held != 3 || row.Delta == nil || *row.Delta != 0 || row.Suspected {
			held, delta := -1, int64(-1)
			if row.Held != nil {
				held = *row.Held
			}
			if row.Delta != nil {
				delta = *row.Delta
			}
			t.Fatalf("round %d: held=%d delta=%d suspected=%v, want 3/0/false (Held = internal + external)", round, held, delta, row.Suspected)
		}
	}
}
