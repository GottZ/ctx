package llmusage

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// W06-4: the window arithmetic with a fake clock, and what Window does with
// a row it is handed — DB-less. The parity of the SQL with the raw queries is
// window_integration_test.go.

func utc(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05.999999999", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestBounds(t *testing.T) {
	kolkata := time.FixedZone("IST", 5*3600+1800)
	cases := []struct {
		name          string
		now           time.Time
		span          time.Duration
		since, edgeTo time.Time
	}{
		{"mid-hour", utc("2026-11-03 10:53:34.123456"), 24 * time.Hour,
			utc("2026-11-02 10:53:34.123456"), utc("2026-11-02 11:00:00")},
		// since exactly on a full hour: the edge is that whole hour, not
		// empty — the bucket starting at since holds a row stamped exactly
		// at since, which "created_at > since" excludes.
		{"since on a full hour", utc("2026-11-03 10:00:00"), 24 * time.Hour,
			utc("2026-11-02 10:00:00"), utc("2026-11-02 11:00:00")},
		{"since 1µs past the hour", utc("2026-11-03 10:00:00.000001"), 24 * time.Hour,
			utc("2026-11-02 10:00:00.000001"), utc("2026-11-02 11:00:00")},
		{"since 1µs before the hour", utc("2026-11-03 09:59:59.999999"), 24 * time.Hour,
			utc("2026-11-02 09:59:59.999999"), utc("2026-11-02 10:00:00")},
		{"30 days are 720 hours", utc("2026-11-03 10:53:34"), 30 * 24 * time.Hour,
			utc("2026-10-04 10:53:34"), utc("2026-10-04 11:00:00")},
		{"nanoseconds cut to microseconds", utc("2026-11-03 10:53:34.123456999"), 24 * time.Hour,
			utc("2026-11-02 10:53:34.123456"), utc("2026-11-02 11:00:00")},
		// A clock in a half-hour zone still cuts at the full UTC hour.
		{"non-UTC clock", time.Date(2026, 11, 3, 16, 23, 34, 0, kolkata), 24 * time.Hour,
			utc("2026-11-02 10:53:34"), utc("2026-11-02 11:00:00")},
	}
	for _, c := range cases {
		since, edgeEnd := Bounds(c.now, c.span)
		if !since.Equal(c.since) || !edgeEnd.Equal(c.edgeTo) {
			t.Errorf("%s: Bounds = (%s, %s), want (%s, %s)", c.name, since, edgeEnd, c.since, c.edgeTo)
		}
		if since.Location() != time.UTC || edgeEnd.Location() != time.UTC {
			t.Errorf("%s: bounds not in UTC: %s, %s", c.name, since.Location(), edgeEnd.Location())
		}
	}
}

// TestBoundsProperty: for any clock reading and span the raw edge is non-empty,
// at most one hour, and ends on a full UTC hour.
func TestBoundsProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 6))
	base := utc("2026-01-01 00:00:00")
	spans := []time.Duration{time.Hour, 90 * time.Minute, 24 * time.Hour, 30 * 24 * time.Hour}
	for i := 0; i < 20000; i++ {
		now := base.Add(time.Duration(r.Int64N(int64(365 * 24 * time.Hour))))
		switch i % 4 {
		case 0:
			now = now.Truncate(time.Hour) // since on a full hour
		case 1:
			now = now.Truncate(time.Hour).Add(time.Microsecond)
		case 2:
			now = now.Truncate(time.Hour).Add(-time.Microsecond)
		}
		span := spans[i%len(spans)]
		since, edgeEnd := Bounds(now, span)
		if want := now.Truncate(time.Microsecond).Add(-span); !since.Equal(want) {
			t.Fatalf("now %s span %s: since %s, want %s", now, span, since, want)
		}
		if d := edgeEnd.Sub(since); d <= 0 || d > time.Hour {
			t.Fatalf("now %s span %s: edge (%s, %s) is %s long", now, span, since, edgeEnd, d)
		}
		if edgeEnd.UnixNano()%int64(time.Hour) != 0 {
			t.Fatalf("now %s span %s: edge end %s is not a full UTC hour", now, span, edgeEnd)
		}
	}
}

// fakeRow is the one row Window scans: calls, cost, two nullable oldest
// timestamps.
type fakeRow struct {
	calls      int64
	cost       float64
	oldestCall *time.Time
	oldestCost *time.Time
	err        error
}

func (f fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	*dest[0].(*int64) = f.calls
	*dest[1].(*float64) = f.cost
	*dest[2].(**time.Time) = f.oldestCall
	*dest[3].(**time.Time) = f.oldestCost
	return nil
}

// fakeRower records the statement and hands back row; a nil row fails the
// test (no query expected).
type fakeRower struct {
	t    *testing.T
	row  *fakeRow
	sql  string
	args []any
}

func (f *fakeRower) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if f.row == nil {
		f.t.Fatal("Window queried the database although nothing was to be counted")
	}
	f.sql, f.args = sql, args
	return *f.row
}

func TestWindowRelief(t *testing.T) {
	now := utc("2026-11-03 10:20:00")
	oldest, oldestCost := utc("2026-11-02 11:05:00.000007"), utc("2026-11-02 13:00:00")
	q := &fakeRower{t: t, row: &fakeRow{calls: 7, cost: 0.25, oldestCall: &oldest, oldestCost: &oldestCost}}
	got, err := Window(context.Background(), q, Spec{Now: now, Span: 24 * time.Hour, APIKeyIDs: []string{"k"}, Locality: "external"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Calls != 7 || got.Cost != 0.25 {
		t.Errorf("figures = %d, %v", got.Calls, got.Cost)
	}
	if want := utc("2026-11-03 11:05:00.000007"); !got.CallsRelief.Equal(want) {
		t.Errorf("CallsRelief = %s, want %s (oldest counted row + 24 h)", got.CallsRelief, want)
	}
	if want := utc("2026-11-03 13:00:00"); !got.CostRelief.Equal(want) {
		t.Errorf("CostRelief = %s, want %s", got.CostRelief, want)
	}
	if q.sql != windowLocalitySQL || len(q.args) != 4 || q.args[3] != "external" {
		t.Errorf("locality window sent %d args, locality rendering %v", len(q.args), q.sql == windowLocalitySQL)
	}
	if !q.args[1].(time.Time).Equal(utc("2026-11-02 10:20:00")) || !q.args[2].(time.Time).Equal(utc("2026-11-02 11:00:00")) {
		t.Errorf("bounds sent = %v, %v", q.args[1], q.args[2])
	}

	// An empty window: no oldest row, no relief.
	q = &fakeRower{t: t, row: &fakeRow{}}
	got, err = Window(context.Background(), q, Spec{Now: now, Span: 24 * time.Hour, APIKeyIDs: []string{"k"}})
	if err != nil {
		t.Fatal(err)
	}
	if !got.CallsRelief.IsZero() || !got.CostRelief.IsZero() {
		t.Errorf("empty window reports relief %s / %s", got.CallsRelief, got.CostRelief)
	}
	if q.sql != windowAllSQL || len(q.args) != 3 {
		t.Errorf("all-locality window sent %d args, all rendering %v", len(q.args), q.sql == windowAllSQL)
	}
}

func TestWindowNoKeysNoQuery(t *testing.T) {
	got, err := Window(context.Background(), &fakeRower{t: t}, Spec{Now: utc("2026-11-03 10:00:00"), Span: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got.Calls != 0 || got.Cost != 0 || !got.Since.Equal(utc("2026-11-03 09:00:00")) {
		t.Errorf("no keys: %+v", got)
	}
}

func TestWindowRejectsSpan(t *testing.T) {
	for _, span := range []time.Duration{0, -time.Hour} {
		if _, err := Window(context.Background(), &fakeRower{t: t}, Spec{Span: span, APIKeyIDs: []string{"k"}}); err == nil {
			t.Errorf("span %s accepted", span)
		}
	}
}

func TestWindowScanError(t *testing.T) {
	boom := errors.New("boom")
	q := &fakeRower{t: t, row: &fakeRow{err: boom}}
	if _, err := Window(context.Background(), q, Spec{Now: time.Now(), Span: time.Hour, APIKeyIDs: []string{"k"}}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it wrapped", err)
	}
}

// TestWindowSQLRenderings pins the two renderings and the shape the plans
// depend on (TestQuotaSpendPlan measures the plans themselves): the locality
// condition fills every marker — four aggregates each on the edge and the
// rollup, the two oldest-row lookups — and never the WHERE of a probe; every
// relation is probed once per key (api_key_id = keys.id, never = ANY) behind
// an OFFSET 0 fence that keeps the planner from flattening the probe into a
// join over a time-leading index.
func TestWindowSQLRenderings(t *testing.T) {
	if strings.Contains(windowAllSQL, "/*loc*/") || strings.Contains(windowLocalitySQL, "/*loc*/") {
		t.Fatal("a marker survived the rendering")
	}
	if strings.Contains(windowAllSQL, "$4") {
		t.Error("the all-locality rendering references $4")
	}
	if n := strings.Count(windowLocalitySQL, "backend_locality = $4"); n != 10 {
		t.Errorf("locality condition rendered %d times, want 10", n)
	}
	for _, sql := range []string{windowAllSQL, windowLocalitySQL} {
		if n := strings.Count(sql, "WHERE api_key_id = keys.id"); n != 4 {
			t.Errorf("%d per-key probes, want 4", n)
		}
		if n := strings.Count(sql, "OFFSET 0)"); n != 4 {
			t.Errorf("%d fenced probes, want 4", n)
		}
		if strings.Contains(sql, "api_key_id = ANY") {
			t.Error("a relation is filtered with = ANY instead of probed per key")
		}
	}
	for _, where := range []string{
		"AND created_at < $3::timestamptz\n           OFFSET 0) l",
		"AND bucket_start >= $3::timestamptz\n           OFFSET 0) r",
	} {
		if !strings.Contains(windowLocalitySQL, where) {
			t.Errorf("the WHERE of a probe changed; expected %q unchanged", where)
		}
	}
}
