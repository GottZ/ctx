// Angriffs-Sonden aus der adversarialen Review von Welle C6-B (Auflage 1). Sie greifen genau die
// Verlustpfade an, die die Autor-Suite offen lässt: ein Write, der WÄHREND der
// Filter-Abfrage eintrifft, ein Write zwischen zwei Waits (also während eines
// laufenden Ticks), und die Behauptung, das Settle-Fenster sei fix ab dem ersten
// Signal statt re-armed.
package events

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// revGateFilter blockiert den ERSTEN Filter-Aufruf, bis der Test ihn freigibt.
// Das ist das Fenster, in dem der Arm weder auf dem Kanal lauscht noch tickt.
type revGateFilter struct {
	mu      sync.Mutex
	calls   int
	ids     [][]string
	entered chan struct{}
	release chan struct{}
}

func (f *revGateFilter) fn(_ context.Context, ids []string) (bool, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.ids = append(f.ids, append([]string(nil), ids...))
	f.mu.Unlock()
	if n == 1 {
		f.entered <- struct{}{}
		<-f.release
	}
	return true, nil
}

func (f *revGateFilter) snapshot() (int, [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([][]string(nil), f.ids...)
}

// TestReviewWriteDuringFilterIsNotLost: ein Checkpoint-Write, der eintrifft,
// während der Arm gerade die Filter-Abfrage stellt, darf nicht bis zum Fallback
// verschwinden.
func TestReviewWriteDuringFilterIsNotLost(t *testing.T) {
	c6bShortDebounce(t, 100*time.Millisecond)
	f := &revGateFilter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	s := c6bArm(t, f.fn)

	s.NotifyBlockInsert("part-1")
	done := make(chan bool, 1)
	go func() { done <- s.distillAwait(context.Background(), time.Hour) }()

	select {
	case <-f.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("filter wurde nicht aufgerufen")
	}
	// Der späte Write fällt genau in die Filter-Abfrage.
	s.NotifyBlockInsert("part-2-während-filter")
	close(f.release)

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("distillAwait meldete Shutdown auf lebendem Kontext")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("distillAwait kehrte nicht zurück")
	}

	// Zweiter Wait mit unerreichbarem Fallback: nur ein überlebendes Signal
	// kann ihn zurückbringen.
	start := time.Now()
	if !s.distillAwait(context.Background(), time.Hour) {
		t.Fatal("distillAwait meldete Shutdown")
	}
	took := time.Since(start)
	if took > 2*time.Second {
		t.Fatalf("zweiter Wait brauchte %v — der Write während der Filter-Abfrage ging bis zum Fallback verloren", took)
	}
	calls, ids := f.snapshot()
	if calls != 2 {
		t.Fatalf("Filter-Aufrufe = %d, erwartet 2", calls)
	}
	if len(ids[1]) != 1 || ids[1][0] != "part-2-während-filter" {
		t.Fatalf("zweites Fenster trug %v, erwartet den späten Write", ids[1])
	}
}

// TestReviewWriteBetweenWaitsIsNotLost: ein Write, der eintrifft, während der
// Arm in distillOnce steckt (also niemand auf dem Kanal lauscht), muss den
// nächsten Wait sofort zurückbringen.
func TestReviewWriteBetweenWaitsIsNotLost(t *testing.T) {
	c6bShortDebounce(t, 100*time.Millisecond)
	f := &c6bCountingFilter{hit: true}
	s := c6bArm(t, f.fn)

	// Kein Wait aktiv — genau der Zustand während eines minutenlangen Ticks.
	s.NotifyBlockInsert("ckpt-während-tick")
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	if !s.distillAwait(context.Background(), time.Hour) {
		t.Fatal("distillAwait meldete Shutdown")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Wait brauchte %v — ein Write während des Ticks ging bis zum Fallback verloren", took)
	}
}

// TestReviewSettleIsFixedNotReArmed: unter Dauerschreibstrom mit Abständen
// UNTER dem Debounce muss der Arm trotzdem nach einem Debounce ticken. Ein
// re-armed Fenster würde hier nie schließen.
func TestReviewSettleIsFixedNotReArmed(t *testing.T) {
	const debounce = 200 * time.Millisecond
	c6bShortDebounce(t, debounce)
	f := &c6bCountingFilter{hit: true}
	s := c6bArm(t, f.fn)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.NotifyBlockInsert(fmt.Sprintf("dauerstrom-%d", i))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	start := time.Now()
	ok := s.distillAwait(context.Background(), 30*time.Second)
	took := time.Since(start)
	close(stop)
	wg.Wait()

	if !ok {
		t.Fatal("distillAwait meldete Shutdown")
	}
	if took > 3*debounce {
		t.Fatalf("Wait brauchte %v bei Debounce %v — das Fenster wurde re-armed und ist unter Dauerstrom unbeschränkt", took, debounce)
	}
}

// TestReviewOverflowKeepsWaking: jenseits der Kappe darf kein Wake verloren
// gehen — der Überlauf muss bedingungslos wecken.
func TestReviewOverflowKeepsWaking(t *testing.T) {
	s := c6bArm(t, nil)
	for i := 0; i < distillWakeIDCap+50; i++ {
		s.NotifyBlockInsert(fmt.Sprintf("id-%d", i))
	}
	ids, overflow := s.drainDistillWake()
	if len(ids) != distillWakeIDCap {
		t.Fatalf("Fenster trug %d ids, Kappe ist %d", len(ids), distillWakeIDCap)
	}
	if !overflow {
		t.Fatal("Überlauf setzte das Overflow-Flag nicht — 50 Writes wären still verloren")
	}
}
