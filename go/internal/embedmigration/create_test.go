package embedmigration

import (
	"errors"
	"testing"
)

// EstimateDiskBytes is pure arithmetic (§6.1) — no DB needed, runs under
// `go test -short`.

func TestEstimateDiskBytes_Zero(t *testing.T) {
	if got := EstimateDiskBytes(0); got != 0 {
		t.Errorf("EstimateDiskBytes(0) = %d, want 0", got)
	}
	if got := EstimateDiskBytes(-5); got != 0 {
		t.Errorf("EstimateDiskBytes(-5) = %d, want 0 (negative is nonsensical, treated as none)", got)
	}
}

func TestEstimateDiskBytes_ScalesLinearlyWithMargin(t *testing.T) {
	one := EstimateDiskBytes(1)
	thousand := EstimateDiskBytes(1000)
	// Per-vector cost is (4100+2800)=6900 bytes, +20% margin = 8280.
	if one != 8280 {
		t.Errorf("EstimateDiskBytes(1) = %d, want 8280 (6900 * 1.2)", one)
	}
	if thousand != one*1000 {
		t.Errorf("EstimateDiskBytes(1000) = %d, want %d (linear in block count)", thousand, one*1000)
	}
}

func TestDiskEstimate_ErrorUnwrapsToSentinel(t *testing.T) {
	var err error = &DiskEstimate{NeededBytes: 100, FreeBytes: 10}
	if !errors.Is(err, ErrDiskInsufficient) {
		t.Errorf("errors.Is(err, ErrDiskInsufficient) = false, want true (Unwrap must expose the sentinel)")
	}
}
