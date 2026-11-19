package store

import (
	"context"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// Test hooks for the derived-identity probe of migration 187.
//
// The probe reads idx_context_derived_identity's state once per
// derivedIdentityProbeInterval and database; the gates in package store_test
// (internal/testdb imports internal/store, so an internal test opening a
// database would be an import cycle) build and drop the index within one
// test and must see the change without waiting the interval out.
// Production never calls them.

// ForgetDerivedIdentityReadingsForTest drops every reading, as a process
// that has not probed yet.
func ForgetDerivedIdentityReadingsForTest() {
	derivedIdentityReadings.Lock()
	derivedIdentityReadings.m = map[string]derivedIdentityReading{}
	derivedIdentityReadings.Unlock()
}

// AgeDerivedIdentityReadingsForTest moves every reading's time back by d, as
// if d had passed since it was taken.
func AgeDerivedIdentityReadingsForTest(d time.Duration) {
	derivedIdentityReadings.Lock()
	for k, r := range derivedIdentityReadings.m {
		r.at = r.at.Add(-d)
		derivedIdentityReadings.m[k] = r
	}
	derivedIdentityReadings.Unlock()
}

// DerivedIdentityProbeIntervalForTest is derivedIdentityProbeInterval.
const DerivedIdentityProbeIntervalForTest = derivedIdentityProbeInterval

// ForeignProvenanceClaimForTest runs foreignProvenanceClaim for a writer of
// the given arm and stratum.
func ForeignProvenanceClaimForTest(ctx context.Context, q pgxdb.Rower, category, title, scope, arm, stratum string) (bool, error) {
	return foreignProvenanceClaim(ctx, q, category, title, scope, provenanceIdentity{arm: arm, stratum: stratum})
}
