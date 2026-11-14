package main

// #246 — the command-line decision without a process: parseArgs classifies,
// answerArgs writes the version, help and usage answers. The child-process
// half (args_subprocess_test.go) proves that main routes every argument
// through this decision before anything is opened.

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/provenance"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args []string
		want argMode
	}{
		{nil, modeBoot},
		{[]string{}, modeBoot},
		{[]string{"--version"}, modeVersion},
		{[]string{"-version"}, modeVersion},
		{[]string{"version"}, modeVersion},
		{[]string{"--help"}, modeHelp},
		{[]string{"-help"}, modeHelp},
		{[]string{"-h"}, modeHelp},
		// The argv the repository's own callers pass, byte for byte: the
		// compose healthcheck, break-glass.sh and the scheduler's re-exec.
		{[]string{"-health"}, modeHealth},
		{[]string{"-secret-decrypt"}, modeSecretDecrypt},
		{[]string{overview.WorkerCommand}, modeOverviewWorker},
		// Everything else is refused, never ignored into a boot.
		{[]string{"--bogus"}, modeUsageError},
		{[]string{""}, modeUsageError},
		{[]string{"-v"}, modeUsageError},
		{[]string{"--Version"}, modeUsageError},
		{[]string{"--health"}, modeUsageError},
		{[]string{"--version", "extra"}, modeUsageError},
		{[]string{"-health", "extra"}, modeUsageError},
		{[]string{"-secret-decrypt", ""}, modeUsageError},
		{[]string{overview.WorkerCommand, "x"}, modeUsageError},
	}
	for _, tc := range cases {
		if got := parseArgs(tc.args); got != tc.want {
			t.Errorf("parseArgs(%q) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

func TestAnswerArgs(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		var out, errw bytes.Buffer
		code := answerArgs(modeVersion, []string{"--version"}, &out, &errw)
		if want := "ctxd " + provenance.BuildVersion() + "\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if code != 0 || errw.Len() != 0 {
			t.Errorf("code %d, stderr %q — want 0 and nothing", code, errw.String())
		}
	})
	t.Run("help", func(t *testing.T) {
		var out, errw bytes.Buffer
		code := answerArgs(modeHelp, []string{"--help"}, &out, &errw)
		if out.String() != usageText || code != 0 || errw.Len() != 0 {
			t.Errorf("code %d, stdout %q, stderr %q — want 0, the usage, nothing", code, out.String(), errw.String())
		}
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus"}, `ctxd: unknown argument "--bogus"`},
		{[]string{""}, `ctxd: unknown argument ""`},
		{[]string{"--version", "extra"}, `ctxd: --version takes no further arguments, got ["extra"]`},
	} {
		t.Run("usage error "+strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errw bytes.Buffer
			code := answerArgs(modeUsageError, tc.args, &out, &errw)
			if code != exitUsage || exitUsage != 2 {
				t.Errorf("code = %d (exitUsage %d), want 2", code, exitUsage)
			}
			if out.Len() != 0 {
				t.Errorf("a usage error wrote to stdout: %q", out.String())
			}
			if want := tc.want + "\n" + usageText; errw.String() != want {
				t.Errorf("stderr = %q, want %q", errw.String(), want)
			}
		})
	}
}

// TestUsageNamesEveryOperatorMode keeps the help honest: every argument an
// operator can pass is in the usage text. The overview worker is the one
// deliberate exception (internal IPC of the scheduler).
func TestUsageNamesEveryOperatorMode(t *testing.T) {
	names := make([]string, 0, len(argModes))
	for arg := range argModes {
		names = append(names, arg)
	}
	slices.Sort(names)
	for _, arg := range names {
		if arg == overview.WorkerCommand || arg == "-version" || arg == "version" || arg == "-help" || arg == "-h" {
			continue // the worker is hidden; the alternate spellings share their line
		}
		if !strings.Contains(usageText, " "+arg+" ") {
			t.Errorf("usage text does not name %q", arg)
		}
	}
}
