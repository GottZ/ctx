package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestScanMaxRequestBytesDefault pins the default of
// gateway.scan_max_request_bytes (RC-D C8): 2 MiB, below the body cap of
// 8 MiB, and its way into the oai settings snapshot.
func TestScanMaxRequestBytesDefault(t *testing.T) {
	d := Defaults()
	if got := d.Gateway.ScanMaxRequestBytes; got != 2<<20 {
		t.Errorf("default gateway.scan_max_request_bytes = %d, want %d", got, 2<<20)
	}
	if got := d.GatewaySettings().ScanMaxRequestBytes; got != 2<<20 {
		t.Errorf("oai.Settings.ScanMaxRequestBytes = %d, want %d", got, 2<<20)
	}
}

// TestValidateScanMaxRequestBytes is V58: 0 is refused (every request below
// credentials would be 413); a value above gateway.max_request_bytes is
// legal and has no effect — the effective scan cap is the smaller of the
// two (oai.EffectiveScanCap), so lowering the body cap alone stays a valid
// write; 1 is legal.
func TestValidateScanMaxRequestBytes(t *testing.T) {
	c := Defaults()
	c.Gateway.ScanMaxRequestBytes = 0
	if hits := issuesOn(Validate(c), "gateway.scan_max_request_bytes"); len(hits) != 1 || hits[0].Severity != SeverityError {
		t.Errorf("gateway.scan_max_request_bytes = 0: want exactly one error, got %+v", hits)
	}
	for _, v := range []int{1, 8 << 20, 64 << 20} {
		c := Defaults()
		c.Gateway.ScanMaxRequestBytes = v
		if hits := issuesOn(Validate(c), "gateway.scan_max_request_bytes"); len(hits) != 0 {
			t.Errorf("gateway.scan_max_request_bytes = %d is legal, got %+v", v, hits)
		}
	}
	c = Defaults()
	c.Gateway.MaxRequestBytes, c.Gateway.ScanMaxRequestBytes = 1<<20, 2<<20
	if issues := Validate(c); len(issuesOn(issues, "gateway.scan_max_request_bytes"))+len(issuesOn(issues, "gateway.max_request_bytes")) != 0 {
		t.Errorf("a body cap below the scan cap is legal (the scan cap has no effect there), got %+v", issues)
	}
	if got := oai.EffectiveScanCap(int64(c.Gateway.ScanMaxRequestBytes), int64(c.Gateway.MaxRequestBytes)); got != 1<<20 {
		t.Errorf("effective scan cap = %d, want the body cap %d", got, 1<<20)
	}
}
