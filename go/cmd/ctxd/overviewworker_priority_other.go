//go:build !linux

// Non-Linux stub for the E-B worker self-deprioritization: nice/ionice are
// Linux semantics. Elsewhere (dev hosts) the worker simply runs at normal
// priority — production ctxd is Linux-only by deployment (container image).

package main

// deprioritizeSelf is a no-op off Linux: no deprioritization, no error.
func deprioritizeSelf() (niceErr, ioErr error) { return nil, nil }
