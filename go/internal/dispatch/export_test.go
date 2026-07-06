package dispatch

import "time"

// ReapForTest exposes one reaper pass to the external K1 wire test (the
// production cadence is the lazy reapLoop ticker).
func (d *Dispatcher) ReapForTest(now time.Time) { d.reapNow(now) }
