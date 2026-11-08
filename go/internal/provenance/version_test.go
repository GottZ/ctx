package provenance

import (
	"runtime/debug"
	"testing"
)

// TestResolveVersion pins the order of the build-version sources (FOLGE-X,
// LOOP Queue 72): the release stamp (-ldflags -X …/provenance.version=vX.Y.Z,
// the Docker build-arg CTX_VERSION) wins; without it the module version Go
// stamped (go install of a tag); without that the VCS revision with its
// dirty flag; without any of them "dev" — never "", so /health always
// names what runs.
func TestResolveVersion(t *testing.T) {
	vcs := func(rev, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: modified}}
	}
	info := func(main string, s []debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/GottZ/ctx", Version: main}, Settings: s}
	}
	const rev = "8ef839f8c0ffee00112233445566778899aabbcc"
	for _, tc := range []struct {
		name  string
		stamp string
		info  *debug.BuildInfo
		ok    bool
		want  string
	}{
		{"stamp_wins", "v5.35.0", info("v5.34.0", vcs(rev, "false")), true, "v5.35.0"},
		{"stamp_trimmed", " v5.35.0\n", nil, false, "v5.35.0"},
		{"stamp_dev_is_unset", "dev", info("v5.34.0", nil), true, "v5.34.0"},
		{"module_version", "", info("v5.34.0", vcs(rev, "false")), true, "v5.34.0"},
		{"devel_falls_to_vcs", "", info("(devel)", vcs(rev, "false")), true, rev},
		{"vcs_dirty", "", info("(devel)", vcs(rev, "true")), true, rev + "-dirty"},
		{"empty_main_falls_to_vcs", "", info("", vcs(rev, "false")), true, rev},
		{"nothing_is_dev", "", info("(devel)", nil), true, "dev"},
		{"no_build_info_is_dev", "", nil, false, "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVersion(tc.stamp, tc.info, tc.ok); got != tc.want {
				t.Errorf("FOLGE-X gate (version source): resolveVersion(%q, …) = %q, want %q", tc.stamp, got, tc.want)
			}
		})
	}
}

// TestBuildVersionNeverEmpty: the process value is resolved once and is
// never empty (a test binary carries no stamp ⇒ "dev").
func TestBuildVersionNeverEmpty(t *testing.T) {
	v := BuildVersion()
	if v == "" || v != BuildVersion() {
		t.Errorf("FOLGE-X gate (version): BuildVersion() = %q, want a stable non-empty value", v)
	}
	t.Logf("BuildVersion() = %q (stamp %q)", v, version)
}
