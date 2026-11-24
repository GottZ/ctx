package events

import "testing"

// TestDigestNotesDecideEveryOrder is the decision table of the background
// digest's notes (issue #525) for the orders a notification and a run can
// take: a notification of a topic map the digest wrote is no write; one that
// arrives while a run is under way is decided when the run ends, against the
// maps it reports; anything else is a write, and none is lost. The
// integration tests in digest_selftrigger_integration_test.go drive the same
// paths through PostgreSQL's notifications.
func TestDigestNotesDecideEveryOrder(t *testing.T) {
	type step struct {
		notify string   // an id to notify ("" = a write nobody knows)
		begin  int      // > 0: open a run over that many tenants
		end    []string // non-nil: end the run, reporting these maps
		panic  bool     // end the run as a panic does (no maps)
	}
	for _, c := range []struct {
		name    string
		known   []string // maps written by earlier runs
		steps   []step
		pending bool
	}{
		{"a known map, between runs", []string{"M"}, []step{{notify: "M"}}, false},
		{"a known map, during a run", []string{"M"}, []step{{begin: 1}, {notify: "M"}, {end: []string{"M"}}}, false},
		{"the run's first map before the run reports it", nil, []step{{begin: 1}, {notify: "M"}, {notify: "M"}, {end: []string{"M"}}}, false},
		{"the run's first map after the run reported it", nil, []step{{begin: 1}, {end: []string{"M"}}, {notify: "M"}}, false},
		{"another block during the run", nil, []step{{begin: 1}, {notify: "X"}, {end: []string{"M"}}}, true},
		{"the map, then another block", nil, []step{{begin: 1}, {notify: "M"}, {notify: "X"}, {end: []string{"M"}}}, true},
		{"another block, then the map", nil, []step{{begin: 1}, {notify: "X"}, {notify: "M"}, {end: []string{"M"}}}, true},
		{"two tenants, one map and another block", nil, []step{{begin: 2}, {notify: "M"}, {notify: "X"}, {end: []string{"M"}}}, true},
		{"two tenants, two maps", nil, []step{{begin: 2}, {notify: "M"}, {notify: "N"}, {end: []string{"M", "N"}}}, false},
		{"an unknown write during the run", nil, []step{{begin: 1}, {notify: ""}, {end: []string{"M"}}}, true},
		{"a run that panicked", nil, []step{{begin: 1}, {notify: "M"}, {panic: true}}, true},
		{"another block between runs", []string{"M"}, []step{{notify: "X"}}, true},
	} {
		s := &Scheduler{}
		for _, id := range c.known {
			if s.digestMaps == nil {
				s.digestMaps = map[string]struct{}{}
			}
			s.digestMaps[id] = struct{}{}
		}
		var notes *digestRunNotes
		for _, st := range c.steps {
			switch {
			case st.begin > 0:
				notes = s.beginDigestRun(st.begin)
			case st.end != nil:
				s.endDigestRun(notes, st.end)
			case st.panic:
				s.endDigestRun(notes, nil)
			default:
				s.NotifyBlockWrite(st.notify)
			}
		}
		if notes != nil {
			s.endDigestRun(notes, nil) // the deferred close: a no-op after the regular one
		}
		if s.digestPending != c.pending {
			t.Errorf("%s: digest pending %v, want %v", c.name, s.digestPending, c.pending)
		}
		if moved := s.writeGen != 0; moved != c.pending {
			t.Errorf("%s: write generation %d, want it moved only for a write (%v)", c.name, s.writeGen, c.pending)
		}
		if s.digestRun != nil {
			t.Errorf("%s: the run's notes are still open", c.name)
		}
	}
}
