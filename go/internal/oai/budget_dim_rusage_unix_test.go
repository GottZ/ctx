//go:build !windows

package oai_test

import "syscall"

// dimRusageCPU is the process's CPU time so far (user + system) in seconds
// for the budget dimensioning harness (budget_dim_test.go); -1 when the
// kernel refuses. Unix only: the windows cross-compile vets the test files
// too (ci.yml), and syscall.Getrusage does not exist there.
func dimRusageCPU() float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return -1
	}
	return float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e9
}
