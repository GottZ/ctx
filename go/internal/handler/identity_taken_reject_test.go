package handler

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// TestIdentityTakenReject pins the one mapping of the issue #227 refusal onto
// the write vocabulary: 409 / constraint, the holder named when the store found
// it and left out when it did not, nil for every other error — the provenance
// sentinel included, which provenanceRejectOr answers on its own.
func TestIdentityTakenReject(t *testing.T) {
	const remedy = " — choose another category or title"

	rej := identityTakenReject(fmt.Errorf("outer: %w", &store.BlockIdentityTakenError{ConflictID: "0199-holder"}))
	if rej == nil {
		t.Fatal("wrapped identity error: nil reject")
	}
	if rej.Status != http.StatusConflict || rej.Code != "constraint" {
		t.Errorf("status/code = %d/%q, want 409/constraint", rej.Status, rej.Code)
	}
	if want := blockIdentityTakenMsg + " (block 0199-holder)" + remedy; rej.Msg != want {
		t.Errorf("msg = %q, want %q", rej.Msg, want)
	}

	rej = identityTakenReject(&store.BlockIdentityTakenError{})
	if rej == nil {
		t.Fatal("identity error without holder: nil reject")
	}
	if want := blockIdentityTakenMsg + remedy; rej.Msg != want {
		t.Errorf("msg without holder = %q, want %q", rej.Msg, want)
	}

	for _, err := range []error{nil, errors.New("store: update block: boom"), store.ErrProvenanceProtected} {
		if got := identityTakenReject(err); got != nil {
			t.Errorf("identityTakenReject(%v) = %+v, want nil", err, got)
		}
	}
}
