package config

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Store publishes the current Config generation via atomic pointer swap.
// Snapshot is a single load instruction — no lock, no cache-line bouncing on
// the hot path — and a snapshot is frozen for the whole operation by
// construction: a torn read mixing host of generation A with the API key of
// generation B is impossible (unlike the package-var tuples it replaces).
type Store struct {
	p atomic.Pointer[Config]
}

// NewStore publishes the initial generation. c must have passed Validate
// (main aborts on HasErrors before constructing the store).
func NewStore(c *Config) *Store {
	s := &Store{}
	s.p.Store(c)
	return s
}

// Snapshot returns the current generation. Callers take ONE snapshot per
// operation (request, dream cycle, daily iteration, digest run) and pass
// values down as parameters. The returned Config is immutable — updates go
// through copy-on-write + Replace.
func (s *Store) Snapshot() *Config {
	return s.p.Load()
}

// Replace validates and atomically publishes a new generation. Any
// SeverityError rejects the swap and returns the findings as the error;
// warnings pass. F1: test-only — consumers adopt the store wave by wave
// (G09–G14), and no CLI/API path calls this before the F2 settings API.
func (s *Store) Replace(c *Config) error {
	issues := Validate(c)
	if HasErrors(issues) {
		var b strings.Builder
		for _, is := range issues {
			if is.Severity != SeverityError {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("; ")
			}
			fmt.Fprintf(&b, "%s: %s", is.Field, is.Msg)
		}
		return fmt.Errorf("config rejected: %s", b.String())
	}
	s.p.Store(c)
	return nil
}
