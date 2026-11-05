package config

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestComposeGatewayHeaderCountsItsBlock pins the head of the gateway.*
// block in the repo-root docker-compose.yml ("# ── gateway.* (N) ──") to
// the number of CTX_GATEWAY_* lines the block declares, and that number to
// the gateway.* registry keys with an env name. The head drifted once (W06-10
// added eight keys under a head that still said 36 — CodeRabbit on PR #148);
// no other gate reads it. Like the other compose gates: run with -count=1
// after touching the file (it lives outside the module).
func TestComposeGatewayHeaderCountsItsBlock(t *testing.T) {
	const path = "../../../docker-compose.yml" // go/internal/config -> repo root
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	head := regexp.MustCompile(`^\s*# ── gateway\.\* \((\d+)\) ──`)
	anyHead := regexp.MustCompile(`^\s*# ── `)
	want, lines, inBlock, seen := -1, 0, false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if m := head.FindStringSubmatch(line); m != nil {
			want, _ = strconv.Atoi(m[1])
			inBlock, seen = true, true
			continue
		}
		if inBlock && anyHead.MatchString(line) {
			inBlock = false
		}
		if inBlock && strings.HasPrefix(strings.TrimSpace(line), "CTX_GATEWAY_") {
			lines++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatalf("%s has no \"# ── gateway.* (N) ──\" head", path)
	}
	keys := 0
	for _, e := range registry() {
		if strings.HasPrefix(e.Key, "gateway.") && e.EnvVar != "" && e.EnvVar != "-" {
			keys++
		}
	}
	if want != lines || lines != keys {
		t.Errorf("gateway.* block head says %d, the block declares %d CTX_GATEWAY_* lines, the registry has %d gateway.* env keys — keep all three equal",
			want, lines, keys)
	}
}
