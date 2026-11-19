// Package testgoroutine tells a test's own goroutines apart from the
// process's, so a leak pin counts only what the test answers for.
//
// runtime.NumGoroutine counts every goroutine of the process: a loop of an
// earlier test's dispatcher still leaving after its Close, a handler of an
// earlier test's server, a timer callback an earlier test armed. Such a
// goroutine ending — or starting — between a pin's two counts moves a
// process-wide count without any change to the code the pin is about
// (issues #362 and #369). The stack dump names, for every goroutine, the
// goroutine whose go statement created it; this package follows those links.
//
// Test helper only: no production code imports it.
package testgoroutine

import (
	"bytes"
	"maps"
	"regexp"
	"runtime"
	"strconv"
	"time"
)

// G is one goroutine of a stack dump.
type G struct {
	ID      int64  // goroutine id
	Creator int64  // the id of the goroutine whose go statement created it (0: none)
	Stack   string // its stack as the dump prints it
}

var (
	header  = regexp.MustCompile(`(?m)^goroutine (\d+)(?: [^\n\[]*)? \[`)
	creator = regexp.MustCompile(`(?m)^created by \S+ in goroutine (\d+)$`)
)

// Dump reads every goroutine of the process from one stack dump.
func Dump() []G {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []G
	for _, block := range bytes.Split(buf, []byte("\n\n")) {
		h := header.FindSubmatch(block)
		if h == nil {
			continue
		}
		g := G{Stack: string(block)}
		g.ID, _ = strconv.ParseInt(string(h[1]), 10, 64)
		if c := creator.FindSubmatch(block); c != nil {
			g.Creator, _ = strconv.ParseInt(string(c[1]), 10, 64)
		}
		out = append(out, g)
	}
	return out
}

// Current is the id of the calling goroutine.
func Current() int64 {
	buf := make([]byte, 64)
	h := header.FindSubmatch(buf[:runtime.Stack(buf, false)])
	if h == nil {
		panic("testgoroutine: no goroutine header in the stack dump")
	}
	id, _ := strconv.ParseInt(string(h[1]), 10, 64)
	return id
}

// Own is a test's own goroutines as of one dump: the goroutines root (the
// test's goroutine) created, or one of them created, at any depth the dump
// still links. Root itself is not one of them.
func Own(dump []G, root int64) []G {
	in := map[int64]bool{root: true}
	for grew := true; grew; {
		grew = false
		for _, g := range dump {
			if !in[g.ID] && in[g.Creator] {
				in[g.ID], grew = true, true
			}
		}
	}
	var own []G
	for _, g := range dump {
		if in[g.ID] && g.ID != root {
			own = append(own, g)
		}
	}
	return own
}

// Baseline is a test's view of the process at one moment: which goroutines
// were its own and which belonged to the rest of the process.
type Baseline struct {
	root    int64
	own     map[int64]bool
	foreign map[int64]bool
}

// Take records, for the calling goroutine as the test's root, which of the
// process's goroutines are the test's own (Own) and which are foreign.
func Take() Baseline {
	root := Current()
	dump := Dump()
	b := Baseline{root: root, own: map[int64]bool{}, foreign: map[int64]bool{}}
	for _, g := range Own(dump, root) {
		b.own[g.ID] = true
	}
	for _, g := range dump {
		if g.ID != root && !b.own[g.ID] {
			b.foreign[g.ID] = true
		}
	}
	return b
}

// TakeSettled is Take once the test's own goroutines have stopped
// changing: the same goroutines in three samples 10 ms apart, at most max —
// goroutines of the test's earlier steps still on their way out (a served
// request's watcher, a closed connection's loops) end before the baseline
// is taken instead of leaving it later.
func TakeSettled(max time.Duration) Baseline {
	b := Take()
	for stable, deadline := 0, time.Now().Add(max); stable < 3 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		next := Take()
		if maps.Equal(next.own, b.own) {
			stable++
		} else {
			b, stable = next, 0
		}
	}
	return b
}

// Settle polls the test's own goroutines (Own) until they are the
// baseline's again — as many, none of them new — or until max has passed,
// and returns the last set: a goroutine of the test that ends within max
// is on its way out, one that outlives it is a leak.
func (b Baseline) Settle(max time.Duration) []G {
	own := b.Own(Dump())
	for deadline := time.Now().Add(max); !b.settled(own) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
		own = b.Own(Dump())
	}
	return own
}

// Settled reports whether own (a result of Own) is the baseline's set
// again: as many goroutines, none of them new since the baseline.
func (b Baseline) Settled(own []G) bool { return b.settled(own) }

func (b Baseline) settled(own []G) bool {
	if len(own) != len(b.own) {
		return false
	}
	for _, g := range own {
		if !b.own[g.ID] {
			return false
		}
	}
	return true
}

// New is the test's own goroutines in dump (Own) that it did not own at the
// baseline: what the test's work since then started and left running.
func (b Baseline) New(dump []G) []G {
	var out []G
	for _, g := range b.Own(dump) {
		if !b.own[g.ID] {
			out = append(out, g)
		}
	}
	return out
}

// Len is how many goroutines the test owned at the baseline.
func (b Baseline) Len() int { return len(b.own) }

// Own is the test's own goroutines in dump, against the baseline: those of
// the baseline's own that are still there, and every goroutine that is new
// since the baseline unless a foreign goroutine of the baseline created it,
// directly or through new goroutines the dump links. A new goroutine whose
// creator has already ended — or that the runtime started for a timer
// (time.AfterFunc: no creating goroutine) — counts as the test's: between
// the baseline and the dump only the test's own work starts goroutines that
// its pins answer for (the tests that use this run without t.Parallel), and
// a goroutine that outlives its short-lived creator is exactly the leak such
// a pin is for. A timer an earlier test armed can fire in that window too;
// its goroutine counts until it ends, which a pin's settle waits for.
// Foreign goroutines of the baseline — an earlier test's, still leaving —
// never count, whether they end or not.
func (b Baseline) Own(dump []G) []G {
	byID := make(map[int64]G, len(dump))
	for _, g := range dump {
		byID[g.ID] = g
	}
	// foreignBorn: a new goroutine whose creator chain, within the dump,
	// reaches a foreign goroutine of the baseline.
	memo := map[int64]bool{}
	var foreignBorn func(id int64, depth int) bool
	foreignBorn = func(id int64, depth int) bool {
		if v, ok := memo[id]; ok {
			return v
		}
		g := byID[id]
		v := false
		switch {
		case depth > len(dump):
		case b.foreign[g.Creator]:
			v = true
		case g.Creator != 0 && !b.own[g.Creator] && g.Creator != b.root:
			if c, ok := byID[g.Creator]; ok && !b.foreign[c.ID] {
				v = foreignBorn(c.ID, depth+1)
			}
		}
		memo[id] = v
		return v
	}
	var own []G
	for _, g := range dump {
		switch {
		case g.ID == b.root || b.foreign[g.ID]:
		case b.own[g.ID]:
			own = append(own, g)
		case !foreignBorn(g.ID, 0):
			own = append(own, g)
		}
	}
	return own
}
