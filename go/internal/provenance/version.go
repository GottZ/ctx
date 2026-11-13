package provenance

import (
	"runtime/debug"
	"strings"
	"sync"
)

// version is the release stamp of the build, set by the linker:
//
//	go build -ldflags "-X github.com/GottZ/ctx/internal/provenance.version=v5.35.0" ./cmd/ctxd
//
// The ctxd image sets it from the build-arg CTX_VERSION (go/Dockerfile): the
// compose build passes CTX_VERSION from its environment (docker-compose.yml
// build.args), CI the tag on a tag ref and git-<commit> otherwise
// (.github/workflows/ci.yml). Empty or "dev" = not stamped.
var version string

// BuildVersion is the release this binary was built as (FOLGE-X, LOOP
// Queue 72): the one source GET /health (field version) and the boot line
// "starting HTTP server" both read. Never empty — in order: the release
// stamp; the module version Go stamped (a go install of a tag); the VCS
// revision with "-dirty" when the tree was modified (a local go build in a
// checkout); otherwise "dev". Resolved once per process: /health is public
// and polled, and debug.ReadBuildInfo parses the embedded module list on
// every call.
func BuildVersion() string { return buildVersion() }

var buildVersion = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	return resolveVersion(version, info, ok)
})

// resolveVersion is BuildVersion's order over its inputs (testable without a
// linker flag).
func resolveVersion(stamp string, info *debug.BuildInfo, ok bool) string {
	if s := strings.TrimSpace(stamp); s != "" && s != "dev" {
		return s
	}
	if !ok || info == nil {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	if rev := vcsRev(info); rev != "" {
		return rev
	}
	return "dev"
}
