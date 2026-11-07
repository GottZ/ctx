package oai_test

// The bound of every grant read of the alias resolution (W02-08 hardening
// after Fremd-Review 2, H-1): the entry's, a watcher tick's, the check right
// before a non-stream write.

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/oai"
)

// TestGrantReadDeadlinePin: every grant read of the alias resolution is
// bounded by the door's own database deadline, 1 s (handler
// modelAuthDBDeadline; lead decision of the hardening: a named constant,
// not a setting). The watch tests use the same value (grantDeadline).
func TestGrantReadDeadlinePin(t *testing.T) {
	if oai.GrantReadDeadline != time.Second || grantDeadline != oai.GrantReadDeadline {
		t.Fatalf("grant read deadline %v (tests %v), want the door's 1 s", oai.GrantReadDeadline, grantDeadline)
	}
}
