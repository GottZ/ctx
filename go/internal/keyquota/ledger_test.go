package keyquota_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

const consumerA = "0195f0a0-0000-7000-8000-00000000000a"

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// TestLedgerUnresolvedAliasIsOneCell is gate (c), model half, at the
// ledger: 10 000 refusals before the resolution — the hooks hand the
// record's alias, which is "" until the route resolved one — make ONE cell
// under the sentinel. Red against an aggregator on the client's string is
// the route pin (oai, TestRefusalRecordKeepsTheRawModelNameOut); here the
// sentinel and the bound of a non-resolved name.
func TestLedgerUnresolvedAliasIsOneCell(t *testing.T) {
	l := keyquota.NewLedger(fixedClock(time.Date(2026, 11, 7, 18, 30, 0, 0, time.UTC)))
	for range 10_000 {
		l.Note(keyquota.Rejection{ConsumerID: consumerA, Op: "chat", Reason: keyquota.RejectModel, Detail: keyquota.DetailNotFound})
	}
	l.Note(keyquota.Rejection{ConsumerID: consumerA, Op: "chat", Alias: strings.Repeat("x", 129), Reason: keyquota.RejectModel, Detail: keyquota.DetailNotFound})
	d := l.Drain()
	if len(d) != 1 || d[0].Alias != keyquota.UnresolvedAlias || d[0].N != 10_001 {
		t.Fatalf("drain %+v, want one cell %q n=10001", d, keyquota.UnresolvedAlias)
	}
	if !d[0].Bucket.Equal(time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("bucket %s, want the UTC hour 18:00", d[0].Bucket)
	}
}

// TestLedgerCardinality is B-06-7 at the ledger: 10 000 refusals over three
// reasons of one consumer drain as three cells, whatever the rate.
func TestLedgerCardinality(t *testing.T) {
	l := keyquota.NewLedger(fixedClock(time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)))
	kinds := []keyquota.Rejection{
		{Reason: keyquota.RejectRate, Detail: keyquota.DetailRateTokens},
		{Reason: keyquota.RejectConcurrency},
		{Reason: keyquota.RejectCapacity, Detail: keyquota.DetailServerBusy, Class: "XU"},
	}
	for i := range 10_000 {
		r := kinds[i%3]
		r.ConsumerID, r.Op, r.Alias = consumerA, "chat", "qwen"
		l.Note(r)
	}
	d := l.Drain()
	var sum int64
	for _, c := range d {
		sum += c.N
	}
	if len(d) != 3 || sum != 10_000 {
		t.Fatalf("drain %d cell(s) summing %d, want 3 summing 10000: %+v", len(d), sum, d)
	}
	if again := l.Drain(); len(again) != 0 {
		t.Fatalf("second drain %+v, want empty", again)
	}
}

// TestLedgerNormalises pins what Note keeps: class and target only on
// capacity, an unknown detail under its reason, an unknown op as "", a
// reason outside the vocabulary and a consumer that is no UUID not at all.
func TestLedgerNormalises(t *testing.T) {
	l := keyquota.NewLedger(fixedClock(time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)))
	l.Note(keyquota.Rejection{ConsumerID: consumerA, Op: "chat", Alias: "qwen", Class: "XU", Target: "http://a", Reason: keyquota.RejectRate, Detail: keyquota.DetailRateRequests})
	l.Note(keyquota.Rejection{ConsumerID: consumerA, Op: "chat", Alias: "qwen", Class: "XU", Target: "http://a", Reason: keyquota.RejectCapacity, Detail: keyquota.DetailServerBusy})
	l.Note(keyquota.Rejection{Op: "images", Reason: keyquota.RejectAuth, Detail: "free text from somewhere"})
	l.Note(keyquota.Rejection{Reason: "throttled"})
	l.Note(keyquota.Rejection{ConsumerID: "not-a-uuid", Reason: keyquota.RejectAuth})
	d := l.Drain()
	want := []string{
		"||_unresolved|||auth|",
		consumerA + "|chat|qwen|XU|http://a|capacity|server_busy",
		consumerA + "|chat|qwen|||rate|rate_requests",
	}
	var got []string
	for _, c := range d {
		got = append(got, strings.Join([]string{c.ConsumerID, c.Op, c.Alias, c.Class, c.Target, c.Reason, c.Detail}, "|"))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("cells\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := l.Dropped(); n != 2 {
		t.Fatalf("dropped %d, want 2 (unknown reason, non-UUID consumer)", n)
	}
}

// TestLedgerWaitColumns is B-06-25 at the ledger: a refusal that waited
// adds its wait and one to wait_n; one that did not wait adds neither.
func TestLedgerWaitColumns(t *testing.T) {
	l := keyquota.NewLedger(fixedClock(time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)))
	for range 100 {
		l.Note(keyquota.Rejection{ConsumerID: consumerA, Op: "chat", Alias: "qwen", Reason: keyquota.RejectCapacity, Detail: keyquota.DetailWaitExpired, Wait: 101 * time.Millisecond, Waited: true})
	}
	l.Note(keyquota.Rejection{ConsumerID: consumerA, Op: "chat", Alias: "qwen", Reason: keyquota.RejectCapacity, Detail: keyquota.DetailWaitExpired, Wait: time.Hour})
	d := l.Drain()
	if len(d) != 1 || d[0].N != 101 || d[0].WaitN != 100 || d[0].WaitMsSum != 10_100 {
		t.Fatalf("drain %+v, want n=101 wait_n=100 wait_ms_sum=10100", d)
	}
}

// TestLedgerRestore: the deltas of a failed flush come back and add up with
// what was noted since.
func TestLedgerRestore(t *testing.T) {
	l := keyquota.NewLedger(fixedClock(time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)))
	l.Note(keyquota.Rejection{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey})
	failed := l.Drain()
	l.Note(keyquota.Rejection{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey})
	if lost := l.Restore(failed); lost != 0 {
		t.Fatalf("restore lost %d", lost)
	}
	if d := l.Drain(); len(d) != 1 || d[0].N != 2 {
		t.Fatalf("after restore %+v, want one cell n=2", d)
	}
}

// TestLedgerHourBuckets: refusals of two hours stay two cells.
func TestLedgerHourBuckets(t *testing.T) {
	at := time.Date(2026, 11, 7, 18, 59, 59, 0, time.UTC)
	l := keyquota.NewLedger(func() time.Time { return at })
	l.Note(keyquota.Rejection{Reason: keyquota.RejectAuth})
	at = at.Add(2 * time.Second)
	l.Note(keyquota.Rejection{Reason: keyquota.RejectAuth})
	if d := l.Drain(); len(d) != 2 || d[0].Bucket.Hour() != 18 || d[1].Bucket.Hour() != 19 {
		t.Fatalf("drain %+v, want the hours 18 and 19", d)
	}
}

// TestLedgerConcurrentNotes: Note from many goroutines loses no count
// (run under -race).
func TestLedgerConcurrentNotes(t *testing.T) {
	l := keyquota.NewLedger(time.Now)
	var wg sync.WaitGroup
	var drained int64
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, c := range l.Drain() {
				mu.Lock()
				drained += c.N
				mu.Unlock()
			}
		}
	}()
	for g := range 8 {
		wg.Go(func() {
			for range 1000 {
				l.Note(keyquota.Rejection{ConsumerID: consumerA, Reason: keyquota.RejectRate, Detail: fmt.Sprint(g)})
			}
		})
	}
	wg.Wait()
	close(stop)
	<-done
	for _, c := range l.Drain() {
		drained += c.N
	}
	if drained != 8000 {
		t.Fatalf("drained %d, want 8000", drained)
	}
}

// TestDecisionLedgerReason pins the meter's refusals onto the ledger
// vocabulary (design/06 §4.1 steps 7–9); an invalid subject is no refusal.
func TestDecisionLedgerReason(t *testing.T) {
	for _, c := range []struct {
		r              keyquota.Reason
		reason, detail string
	}{
		{keyquota.ReasonRequests, keyquota.RejectRate, keyquota.DetailRateRequests},
		{keyquota.ReasonTokens, keyquota.RejectRate, keyquota.DetailRateTokens},
		{keyquota.ReasonConcurrency, keyquota.RejectConcurrency, ""},
		{keyquota.ReasonWarming, keyquota.RejectCapacity, keyquota.DetailWarming},
		{keyquota.ReasonInvalid, "", ""},
		{"", "", ""},
	} {
		if reason, detail := (keyquota.Decision{Reason: c.r}).LedgerReason(); reason != c.reason || detail != c.detail {
			t.Errorf("%q: (%q, %q), want (%q, %q)", c.r, reason, detail, c.reason, c.detail)
		}
	}
}
