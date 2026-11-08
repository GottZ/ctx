//go:build windows

package oai_test

// dimRusageCPU has no rusage on windows; the harness records -1 (unknown),
// as it does when the kernel refuses on unix. The measurement itself runs
// on the Linux host only (CTX_BUDGET_DIM=1).
func dimRusageCPU() float64 { return -1 }
