package config

import "testing"

// TestDistillDryRunDirIsValidated is V31, the wave A02-6 review's second major
// finding: `distill.dryrun_dir` shipped as the ONLY key of the distill.* group
// without any validation, on the (measurably wrong) assumption that a validator
// covers the /api/settings path alone. It does not — cmd/ctxd/main.go:335-336
// runs FromEnv + Validate and exits on SeverityError, build.go:186 validates the
// assembled config (env + default + admitted settings), and store.go:208 the
// settings write. All three writers pass here.
//
// The rule is deliberately SYNTACTIC: empty (the documented "no plaintext dump"
// setting) and any absolute path pass, a relative one is refused. Whether the
// target lies inside a git working copy is a question about the FILE SYSTEM at
// the moment of use — a symlink, a mount or a fresh `git init` changes the
// answer without the value changing — so that half stays at the point of use in
// the arm (events.distillDumpDir). Two layers, one question each.
func TestDistillDryRunDirIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  Severity
	}{
		{"the default passes", "/var/lib/ctx/distill-dryrun", -1},
		{"empty is the documented dump-off setting", "", -1},
		{"whitespace reads as empty", "   ", -1},
		{"a relative path is refused", "distill-dryrun", SeverityError},
		{"a nested relative path is refused", "relativ/pfad", SeverityError},
		{"a dot-relative path is refused", "./dump", SeverityError},
		{"another absolute path passes", "/srv/ctx/dryrun", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(validCfg(t, map[string]string{"distill.dryrun_dir": tc.value}))
			if got := severityFor(issues, "distill.dryrun_dir"); got != tc.want {
				t.Errorf("distill.dryrun_dir = %q severity = %v, want %v: %v",
					tc.value, got, tc.want, issuesOn(issues, "distill.dryrun_dir"))
			}
		})
	}
}
