package testgoroutine

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// spawn starts a goroutine that runs body and then blocks until stop
// closes, and returns its id once it runs.
func spawn(stop <-chan struct{}, body func()) int64 {
	id := make(chan int64)
	go func() {
		id <- Current()
		if body != nil {
			body()
		}
		<-stop
	}()
	return <-id
}

func ids(gs []G) []int64 {
	out := make([]int64, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.ID)
	}
	slices.Sort(out)
	return out
}

// waitGone polls until no goroutine of the dump has id, or a second.
func waitGone(t *testing.T, id int64) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if !slices.ContainsFunc(Dump(), func(g G) bool { return g.ID == id }) {
			return
		}
	}
	t.Fatalf("goroutine %d still in the dump after 1 s", id)
}

// TestBaselineOwn: a goroutine the root started before the baseline is the
// test's; one whose creator had ended by then is foreign, and so is what it
// starts later; a goroutine the root starts later is the test's, and so is
// one whose short-lived creator has ended — the leak a pin is for. An own
// goroutine that ends leaves the count.
func TestBaselineOwn(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)

	own := spawn(stop, nil)
	// foreign: started by a goroutine that has ended before the baseline.
	var foreign int64
	var startForeignChild = make(chan chan int64)
	var parentDone sync.WaitGroup
	parentDone.Add(1)
	parent := make(chan int64)
	go func() {
		defer parentDone.Done()
		parent <- Current()
		foreign = spawn(stop, func() {
			for reply := range startForeignChild {
				reply <- spawn(stop, nil)
			}
		})
	}()
	parentID := <-parent
	parentDone.Wait()
	waitGone(t, parentID)

	b := Take()
	if got, want := ids(b.Own(Dump())), []int64{own}; !slices.Equal(got, want) {
		t.Fatalf("at the baseline the test owns %v, want %v (foreign %d)", got, want, foreign)
	}

	reply := make(chan int64)
	startForeignChild <- reply
	foreignChild := <-reply
	later := spawn(stop, nil)
	// orphan: started after the baseline by a goroutine that has ended.
	var orphan int64
	creator := make(chan int64)
	go func() {
		creator <- Current()
		orphan = spawn(stop, nil)
		close(creator)
	}()
	creatorID := <-creator
	<-creator
	waitGone(t, creatorID)

	// timer: a goroutine the runtime starts for a time.AfterFunc callback.
	timer := make(chan int64)
	time.AfterFunc(time.Millisecond, func() {
		timer <- Current()
		<-stop
	})
	timerID := <-timer

	if n := b.Len(); n != 1 {
		t.Fatalf("Len = %d, want the one goroutine owned at the baseline", n)
	}
	got := ids(b.Own(Dump()))
	want := []int64{own, later, orphan, timerID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("after the baseline the test owns %v, want %v (foreign %d and its child %d excluded)", got, want, foreign, foreignChild)
	}
	newer := []int64{later, orphan, timerID}
	slices.Sort(newer)
	if got := ids(b.New(Dump())); !slices.Equal(got, newer) {
		t.Fatalf("New = %v, want %v (the owned goroutines started after the baseline)", got, newer)
	}
	close(startForeignChild)
}

// TestTakeSettledAndSettle: a goroutine of the test on its way out when the
// baseline is asked for ends before TakeSettled takes it; Settle waits for
// one started after the baseline to end, and reports one that stays.
func TestTakeSettledAndSettle(t *testing.T) {
	go func() { time.Sleep(30 * time.Millisecond) }()
	b := TakeSettled(time.Second)
	if n := b.Len(); n != 0 {
		t.Fatalf("TakeSettled owns %d goroutines, want 0 — the leaving one was taken in", n)
	}
	go func() { time.Sleep(30 * time.Millisecond) }()
	if own := b.Settle(time.Second); len(own) != 0 {
		t.Fatalf("Settle left %d goroutines, want 0 after the short one ended", len(own))
	}
	stop := make(chan struct{})
	defer close(stop)
	stays := spawn(stop, nil)
	if own := b.Settle(50 * time.Millisecond); len(own) != 1 || own[0].ID != stays {
		t.Fatalf("Settle with one goroutine that stays: %v, want just %d", ids(own), stays)
	}
}
