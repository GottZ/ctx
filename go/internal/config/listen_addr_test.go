package config

import (
	"os"
	"strings"
	"testing"
)

// TestListenAddrLoopbackDefaultAndContainerWiring pins the three halves of one
// decision (#49 Punkt 4) together, because each is harmless alone and only the
// set is correct:
//
//   - the registry default is loopback-only, so a bare `ctxd` started outside a
//     container does not listen on every interface of its host;
//   - the image (go/Dockerfile, final stage) sets LISTEN_ADDR=:8080, so
//     `docker run -p … ghcr.io/gottz/ctx` stays reachable — inside a container
//     127.0.0.1 is the container's own loopback, a published port could never
//     reach it;
//   - the compose ctx service sets LISTEN_ADDR ":8080" for the same reason (the
//     reverse proxy and the e2e stack connect over the compose network).
//
// Dropping the image line is the failure this test exists for: every unit test
// would stay green, and the published image would start, pass its own
// in-container healthcheck (it dials localhost) and refuse every connection
// from outside.
func TestListenAddrLoopbackDefaultAndContainerWiring(t *testing.T) {
	var def string
	for _, e := range registry() {
		if e.EnvVar == "LISTEN_ADDR" {
			def = e.defRaw
		}
	}
	if def != "127.0.0.1:8080" {
		t.Errorf("registry default for LISTEN_ADDR = %q, want the loopback-only 127.0.0.1:8080", def)
	}

	const dockerfile = "../../Dockerfile" // go/internal/config -> go/Dockerfile
	raw, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", dockerfile, err)
	}
	lines := strings.Split(string(raw), "\n")
	lastFrom := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(l)), "FROM ") {
			lastFrom = i
		}
	}
	if lastFrom < 0 {
		t.Fatalf("%s has no FROM line", dockerfile)
	}
	found := false
	for _, l := range lines[lastFrom+1:] {
		if strings.TrimSpace(l) == "ENV LISTEN_ADDR=:8080" {
			found = true
		}
	}
	if !found {
		t.Errorf("%s: the final stage must set `ENV LISTEN_ADDR=:8080` — without it the image binds the loopback default and a published port reaches nothing", dockerfile)
	}

	if got := ctxServiceEnvValues(t)["LISTEN_ADDR"]; got != `":8080"` {
		t.Errorf("docker-compose.yml ctx service: LISTEN_ADDR = %s, want \":8080\" — the proxy and the e2e stack reach ctx over the compose network", got)
	}
}
