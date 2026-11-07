package store

import (
	"regexp"
	"testing"
)

// TestAuditViaPin pins the write channels a context_settings_audit row may
// name in metadata.via (W06-6, design/06 §3.3, B-06-14): the six literals the
// migration-160 triggers and backfill emit plus the gateway's (W06-18, the
// usageevents recorder's consumer_usage rows), pairwise distinct, each within
// the 32 characters audit_actor_meta keeps, and every one of them accepted by
// the stamp while anything else is refused.
func TestAuditViaPin(t *testing.T) {
	want := map[AuditVia]string{
		ViaAPI:          "api",
		ViaBootstrap:    "bootstrap",
		ViaProvision:    "provision",
		ViaOffboarding:  "offboarding",
		ViaMigration:    "migration",
		ViaSQL:          "sql",
		AuditViaGateway: "gateway",
	}
	if len(want) != 7 {
		t.Fatalf("via constants are not pairwise distinct: %d distinct of 7", len(want))
	}
	if AuditViaGateway != "gateway" {
		t.Errorf("AuditViaGateway = %q, want \"gateway\" (usageevents.Via, W06-18)", AuditViaGateway)
	}
	if len(auditVias) != len(want) {
		t.Fatalf("auditVias lists %d values, want %d", len(auditVias), len(want))
	}
	shape := regexp.MustCompile(`^[a-z]{1,32}$`)
	seen := map[AuditVia]bool{}
	for _, v := range auditVias {
		lit, ok := want[v]
		if !ok || string(v) != lit {
			t.Errorf("auditVias holds %q, which is not one of the pinned literals", v)
		}
		if seen[v] {
			t.Errorf("auditVias lists %q twice", v)
		}
		seen[v] = true
		if !shape.MatchString(string(v)) {
			t.Errorf("via %q does not match %s", v, shape)
		}
		if !v.valid() {
			t.Errorf("via %q is not valid", v)
		}
	}
	for _, bad := range []AuditVia{"API", "psql", "api ", "x"} {
		if bad.valid() {
			t.Errorf("via %q is valid, want refused", bad)
		}
	}
}
