//go:build integration

// §F-§6: CTX_OAUTH_DCR_MODE is read per request (handler.dcrMode), so
// without a boot line an operator never sees that open registration is on.
// The probe observes the production NewRouter (where the OAuth routes are
// mounted), not the handler helper alone: what is pinned is that the boot
// path actually emits the warning.
//
//	go test -tags=integration ./cmd/ctxd/ -run TestNewRouterWarnsOnOpenDCR -count=1 -v
package main

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/handler"
)

func TestNewRouterWarnsOnOpenDCR(t *testing.T) {
	const marker = "dynamic client registration is open"
	for _, tc := range []struct {
		name     string
		mode     string
		wantWarn bool
	}{
		{"Open", "open", true},
		{"Admin", "admin", false},
		{"Unset", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(handler.EnvDCRMode, tc.mode)
			buf := captureBootLog(t)
			mcpBodyCapRouter(t)

			var lines []string
			for _, line := range strings.Split(buf.String(), "\n") {
				if strings.Contains(line, marker) {
					lines = append(lines, line)
				}
			}
			if !tc.wantWarn {
				if len(lines) != 0 {
					t.Fatalf("mode %q logged the open-DCR warning: %q", tc.mode, lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("mode %q: %d open-DCR warning lines, want exactly 1:\n%s", tc.mode, len(lines), buf.String())
			}
			for _, want := range []string{"level=WARN", "env=" + handler.EnvDCRMode} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("warning line lacks %q: %s", want, lines[0])
				}
			}
		})
	}
}
