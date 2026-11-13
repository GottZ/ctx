package config

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
)

// composeVersionArg is the build argument of the ctx service that carries the
// release stamp into the image, as the compose file must declare it: taken
// from the environment of the build, "dev" when unset or empty — so a build
// from a tag checkout can stamp the tag without a hand-typed --build-arg
// (docs/operations.md, Deploy & migrations), and every other build keeps
// saying "dev" as before.
const composeVersionArg = "${CTX_VERSION:-dev}"

// TestComposeCtxBuildPassesVersionStamp pins #187 point 1 for the compose
// build: the ctx service hands CTX_VERSION from the environment to the image
// build, and go/Dockerfile consumes exactly that argument as the linker stamp
// GET /health version and the boot line read (internal/provenance.version).
// Before, the build declared no args, so `docker compose build ctx` always
// produced "dev" unless someone remembered --build-arg by hand. Same caveat as
// the other compose gates: docker-compose.yml lives outside the module, so run
// it with -count=1 after touching the file.
func TestComposeCtxBuildPassesVersionStamp(t *testing.T) {
	const composePath = "../../../docker-compose.yml" // go/internal/config -> repo root
	f, err := os.Open(composePath)
	if err != nil {
		t.Fatalf("open %s: %v (the compose gate needs the repo root checkout)", composePath, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	got, ok, err := scanCtxBuildArg(f, "CTX_VERSION")
	if err != nil {
		t.Fatalf("scan %s: %v", composePath, err)
	}
	if !ok || got != composeVersionArg {
		t.Errorf("ctx service build.args.CTX_VERSION = %q (declared: %v), want %q — without it a compose build of a release tag reports version \"dev\"",
			got, ok, composeVersionArg)
	}

	const dockerfile = "../../Dockerfile" // go/internal/config -> go/Dockerfile
	raw, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", dockerfile, err)
	}
	var declared, stamped bool
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "ARG CTX_VERSION=dev" {
			declared = true
		}
		if strings.HasPrefix(line, "RUN ") && strings.Contains(line, "./cmd/ctxd") &&
			strings.Contains(line, "-X github.com/GottZ/ctx/internal/provenance.version=${CTX_VERSION}") {
			stamped = true
		}
	}
	if !declared || !stamped {
		t.Errorf("%s: ARG CTX_VERSION=dev declared %v, ctxd build stamps provenance.version from it %v — the compose argument reaches nothing",
			dockerfile, declared, stamped)
	}
}

// ciImageVersionArg is the build argument the ci.yml docker job hands
// go/Dockerfile: the tag on a tag ref (a workflow_dispatch on refs/tags/vX.Y.Z
// at the tip of root), git-<commit> on every other ref.
const ciImageVersionArg = "CTX_VERSION=${{ github.ref_type == 'tag' && github.ref_name || format('git-{0}', github.sha) }}"

// TestCIImageStampsTagOnTagRefs pins #187 point 1 for the ghcr image: the
// docker job's only CTX_VERSION build argument names the tag on a tag ref.
// Before, it was git-<commit> on every ref, so no CI image could report a
// release tag. CI cannot be run here; this pins the expression, actionlint
// and CI judge its syntax.
func TestCIImageStampsTagOnTagRefs(t *testing.T) {
	const path = "../../../.github/workflows/ci.yml" // go/internal/config -> repo root
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "CTX_VERSION=") {
			got = append(got, line)
		}
	}
	if len(got) != 1 || got[0] != ciImageVersionArg {
		t.Errorf("%s: CTX_VERSION build arguments %q, want exactly [%q] — the ghcr image of a tag build reports git-<commit> instead of the tag",
			path, got, ciImageVersionArg)
	}
}

// scanCtxBuildArg returns the raw value of one entry of the `ctx` service's
// build.args map (indent 4 build:, 6 args:, 8 entries), line-scanned like
// scanCtxServiceKey. Only the ctx entry under the top-level `services:`
// counts, and only args under its build: key.
func scanCtxBuildArg(r io.Reader, name string) (string, bool, error) {
	section, inCtx, inBuild, inArgs := "", false, false, false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, value, isKey := yamlKeyValue(trimmed)
		switch {
		case indent == 0:
			section, inCtx = "", false
			if isKey {
				section = key
			}
		case indent == 2:
			inCtx = section == "services" && isKey && key == "ctx"
			inBuild, inArgs = false, false
		case indent == 4:
			inBuild, inArgs = inCtx && isKey && key == "build", false
		case indent == 6:
			inArgs = inBuild && isKey && key == "args"
		case indent == 8 && inArgs && isKey && key == name:
			return value, true, nil
		}
	}
	return "", false, sc.Err()
}

// TestCtxBuildArgScanner pins the scanner behind the version-stamp gate: an
// args entry of another service, or an args key outside build:, is not the
// ctx build argument.
func TestCtxBuildArgScanner(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       string
		found      bool
	}{
		{"ctx_build_args", "services:\n  ctx:\n    build:\n      context: ./go\n      args:\n        CTX_VERSION: ${CTX_VERSION:-dev}\n", "${CTX_VERSION:-dev}", true},
		{"other_service_only", "services:\n  db:\n    build:\n      args:\n        CTX_VERSION: x\n  ctx:\n    build:\n      context: ./go\n", "", false},
		{"args_outside_build", "services:\n  ctx:\n    build:\n      context: ./go\n    labels:\n      args:\n        CTX_VERSION: x\n", "", false},
		{"ctx_under_volumes_is_not_the_service", "services:\n  ctx:\n    image: x\nvolumes:\n  ctx:\n    build:\n      args:\n        CTX_VERSION: x\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := scanCtxBuildArg(strings.NewReader(tc.yaml), "CTX_VERSION")
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if found != tc.found || got != tc.want {
				t.Errorf("scan = (%q, %v), want (%q, %v)", got, found, tc.want, tc.found)
			}
		})
	}
}
