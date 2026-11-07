package oai_test

// The route side of the embeddings class gate (FOLGE-E (g)): every
// combination of oai.EmbedClassGateCombos with both kinds of alias — "embsec"
// at credentials (no sensitivity scan) and "embpub" below it (the scan runs,
// with its nesting state) — at every size of oai.EmbedClassGateSizes (the
// chat gate's three and the cap) and every concurrency of ecgRouteNs, each
// and with body logging off and full (ecgRouteBodyLogs, a fixed part of the
// matrix), each run TestEmbedInvalidBytesHeapGate in a process of its own
// (this test binary run again with the form and the body logging in its
// environment; GOMEMLIMIT, 200MiB when unset, passes through). Every verdict
// must be ok. A measurement test outside the suite (load-sensitive, 3 072
// processes):
//
//	CTX_EMBED_CLASSGATE_ROUTE=1 [CTX_EMBED_CLASSGATE_ROUTE_OUT=<file>] \
//	  go test -run '^TestEmbedClassGateOverTheRoute$' -timeout 3h ./internal/oai/
//
// CTX_EMBED_CLASSGATE_ROUTE=<file> runs the combinations named in the file
// (one per line) instead of all.

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// ecgRouteNs are the concurrencies of the route side, ecgRouteBodyLogs its
// body-logging modes (off, and full: the log row's text is booked too).
var (
	ecgRouteNs       = []int{1, 10}
	ecgRouteBodyLogs = []string{llmlog.BodyLoggingNone, llmlog.BodyLoggingFull}
)

type ecgRouteRun struct {
	form    string
	n       int
	bytes   int
	bodylog string
}

// ecgRouteRuns is the matrix: every combination with both aliases at every
// size, concurrency and body-logging mode.
func ecgRouteRuns(combos []string) []ecgRouteRun {
	var runs []ecgRouteRun
	for _, c := range combos {
		for _, alias := range []string{"", "@pub"} {
			for _, size := range oai.EmbedClassGateSizes {
				for _, n := range ecgRouteNs {
					for _, bl := range ecgRouteBodyLogs {
						runs = append(runs, ecgRouteRun{"ecg:" + c + alias, n, size, bl})
					}
				}
			}
		}
	}
	return runs
}

// TestEmbedClassGateRouteRunsBothAliases pins the matrix: every combination
// runs at credentials and below it, at every size, concurrency and
// body-logging mode (off and full), and the two forms reach those two
// aliases.
func TestEmbedClassGateRouteRunsBothAliases(t *testing.T) {
	combos := oai.EmbedClassGateCombos()
	runs := ecgRouteRuns(combos)
	per := len(oai.EmbedClassGateSizes) * len(ecgRouteNs) * len(ecgRouteBodyLogs)
	logs := map[string]int{}
	for _, r := range runs {
		logs[r.bodylog]++
	}
	if len(logs) != 2 || logs[llmlog.BodyLoggingNone] != len(runs)/2 || logs[llmlog.BodyLoggingFull] != len(runs)/2 {
		t.Errorf("body logging of the route gate: %v, want none and full in half the runs each", logs)
	}
	sens := map[string]backends.Sensitivity{}
	for _, a := range embedScanAliases() {
		sens[a.name] = a.sens
	}
	forms := map[string]int{}
	for _, r := range runs {
		forms[r.form]++
	}
	count := map[backends.Sensitivity]int{}
	for f, k := range forms {
		if k != per {
			t.Errorf("form %s runs %d times, want %d (every size, concurrency and body logging)", f, k, per)
		}
		model := "embsec"
		if strings.HasSuffix(f, "@pub") {
			model = "embpub"
		}
		combo := strings.TrimSuffix(strings.TrimPrefix(f, "ecg:"), "@pub")
		if body, ok := oai.EmbedClassGateBody(combo, model, 1024); !ok || !strings.Contains(string(body), model) && !strings.HasPrefix(combo, "embedKeys.model:") {
			t.Errorf("form %s: no body for %s", f, model)
		}
		count[sens[model]]++
	}
	if len(runs) != 2*len(combos)*per || count[backends.SensCredentials] != len(combos) || count[backends.SensPublic] != len(combos) {
		t.Errorf("the route gate runs %d times (want %d): %d combinations at credentials, %d below (want %d each)",
			len(runs), 2*len(combos)*per, count[backends.SensCredentials], count[backends.SensPublic], len(combos))
	}
}

func TestEmbedClassGateOverTheRoute(t *testing.T) {
	sel := os.Getenv("CTX_EMBED_CLASSGATE_ROUTE")
	if sel == "" {
		t.Skip("CTX_EMBED_CLASSGATE_ROUTE not set (measurement test, see the comment)")
	}
	combos := oai.EmbedClassGateCombos()
	if sel != "1" {
		raw, err := os.ReadFile(sel)
		if err != nil {
			t.Fatal(err)
		}
		combos = strings.Fields(string(raw))
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CTX_EMBED_CLASSGATE_ROUTE") && !strings.HasPrefix(kv, "CTX_FOLGEE_FORM=") &&
			!strings.HasPrefix(kv, "CTX_FOLGEE_N=") && !strings.HasPrefix(kv, "CTX_FOLGEE_BYTES=") && !strings.HasPrefix(kv, "CTX_FOLGEE_BODYLOG=") {
			env = append(env, kv)
		}
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		env = append(env, "GOMEMLIMIT=200MiB")
	}
	var lines strings.Builder
	bad := map[string]int{}
	runs := ecgRouteRuns(combos)
	for _, r := range runs {
		cmd := exec.Command(exe, "-test.run", "^TestEmbedInvalidBytesHeapGate$", "-test.count=1", "-test.v")
		cmd.Env = append(slices.Clone(env), "CTX_FOLGEE_FORM="+r.form, "CTX_FOLGEE_N="+strconv.Itoa(r.n), "CTX_FOLGEE_BYTES="+strconv.Itoa(r.bytes),
			"CTX_FOLGEE_BODYLOG="+r.bodylog)
		out, _ := cmd.CombinedOutput()
		line := ""
		for _, l := range strings.Split(string(out), "\n") {
			if i := strings.Index(l, "FOLGEE form="); i >= 0 {
				line = l[i:]
			}
		}
		alias := "credentials"
		if strings.HasSuffix(r.form, "@pub") {
			alias = "public"
		}
		if line == "" {
			line = fmt.Sprintf("NOLINE form=%s n=%d bytes=%d bodylog=%s %.300s", r.form, r.n, r.bytes, r.bodylog, out)
		}
		fmt.Fprintf(&lines, "size=%d %s\n", r.bytes, line)
		if !strings.HasSuffix(line, "verdict=ok") {
			bad[alias]++
			t.Errorf("%s n=%d at %d B, body logging %s: %s", r.form, r.n, r.bytes, r.bodylog, line)
		}
	}
	if p := os.Getenv("CTX_EMBED_CLASSGATE_ROUTE_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(lines.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("FOLGEE class gate route: combos=%d runs=%d (each alias %d, each body logging %d) not ok: credentials %d, public %d",
		len(combos), len(runs), len(runs)/2, len(runs)/2, bad["credentials"], bad["public"])
}
