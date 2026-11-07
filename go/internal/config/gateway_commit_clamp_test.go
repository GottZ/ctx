package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestGatewayCommitClampMirrors is W02-08 Z7 (lead decision 13): the stream
// route's grace clamp (internal/oai cannot import config) mirrors the
// validation constants — the commit distance above the wait budget (V43)
// and the grace ceiling (V39). A drift on either side would clamp to a
// bound the settings write no longer enforces. Red: a mirror of 51 s.
func TestGatewayCommitClampMirrors(t *testing.T) {
	if oai.StreamCommitDistance != gatewayCommitDistance {
		t.Errorf("oai.StreamCommitDistance %v != gatewayCommitDistance %v", oai.StreamCommitDistance, gatewayCommitDistance)
	}
	if oai.StreamCommitGraceCeiling != gatewayCommitGraceCeiling {
		t.Errorf("oai.StreamCommitGraceCeiling %v != gatewayCommitGraceCeiling %v", oai.StreamCommitGraceCeiling, gatewayCommitGraceCeiling)
	}
}
