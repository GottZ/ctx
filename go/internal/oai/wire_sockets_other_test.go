//go:build !linux

package oai_test

import (
	"net"
	"testing"
)

// heldListener needs Linux socket semantics (shutdown of a listening socket,
// backlog 0 dropping SYNs); elsewhere the wire gates skip.
type heldListener struct{ net.Listener }

func newHeldListener(t testing.TB) *heldListener {
	t.Helper()
	t.Skip("the deterministic transport faults need Linux socket semantics")
	return nil
}

func (l *heldListener) stop()     {}
func (l *heldListener) dropSYNs() {}

func refusedAddr(t testing.TB) string {
	t.Helper()
	t.Skip("the deterministic transport faults need Linux socket semantics")
	return ""
}
