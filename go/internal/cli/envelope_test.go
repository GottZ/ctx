package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── the checker itself ────────────────────────────────────────────────────────.

// The cases the two predecessors pinned (TestCheckSettingsEnvelope in
// settings_test.go, TestCheckAPIEnvelope in project_test.go) plus the cross
// cases the split made impossible to state: the SAME body under both modes.
func TestCheckEnvelope(t *testing.T) {
	cases := []struct {
		name, body string
		required   string // "" = no error expected
		optional   string
	}{
		{"success", `{"success":true,"settings":[]}`, "", ""},
		{"validation 422", `{"success":false,"error":"validation: rerank.blend_weight must be in [0,1]"}`,
			"must be in [0,1]", "must be in [0,1]"},
		{"restart 409", `{"success":false,"error":"dream.parallelism is restart-only; set CTX_DREAM_PARALLELISM and restart"}`,
			"restart-only", "restart-only"},
		{"admin 403", `{"success":false,"error":"admin key required"}`, "admin key required", "admin key required"},
		{"error without message", `{"success":false}`, "request failed", "request failed"},
		// The 401 the auth middleware sends: no success field, but a reason.
		// The typed commands have always shown that reason verbatim — reporting
		// the raw body instead would be a silent regression on every command
		// that meets an expired key (handler/middleware.go:362/375/392,
		// query.go:574/580/593/844, oauth_register.go:79/83, whoami.go:142).
		{"401 without a frame", `{"error":"unauthorized"}`, "unauthorized", ""},
		{"500 without a frame", `{"error":"authentication error"}`, "authentication error", ""},
		// The one axis the flag controls: a body that is not a ctx envelope.
		{"unparseable", `<html>proxy error</html>`, "unparseable response", ""},
		{"no success field, no reason", `{"status":"ok"}`, "request failed", ""},
		// A bare JSON array does not unmarshal into the frame at all, so it lands
		// in the same class as HTML: unparseable for a typed command, fine for a
		// raw passthrough that asked for exactly that route.
		{"bare array", `[{"id":"b1"}]`, "unparseable response", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertEnvelope(t, "required", checkEnvelope([]byte(c.body), envelopeRequired), c.required)
			assertEnvelope(t, "optional", checkEnvelope([]byte(c.body), envelopeOptional), c.optional)
		})
	}
}

func assertEnvelope(t *testing.T, mode string, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("%s: err = %v, want nil", mode, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: err = %v, want it to contain %q", mode, err, want)
	}
}

// A `strings.Contains` assertion cannot tell "unauthorized" from
// "request failed: {\"error\":\"unauthorized\"}", and that difference IS the
// contract: the message a command puts on stderr is the server's reason, not a
// wrapper around its raw body. Exact equality, both frame shapes.
func TestCheckEnvelopeReportsTheReasonVerbatim(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"framed failure", `{"success":false,"error":"admin key required"}`, "admin key required"},
		{"frameless 401", `{"error":"unauthorized"}`, "unauthorized"},
		{"frameless 500", `{"error":"authentication error"}`, "authentication error"},
		{"framed, no reason", `{"success":false}`, `request failed: {"success":false}`},
		{"frameless, no reason", `{"status":"ok"}`, `request failed: {"status":"ok"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkEnvelope([]byte(c.body), envelopeRequired)
			if err == nil {
				t.Fatalf("err = nil, want %q", c.want)
			}
			if err.Error() != c.want {
				t.Fatalf("err = %q, want exactly %q", err.Error(), c.want)
			}
		})
	}
}

// The error detail is bounded — a 10 MB response must not land on stderr.
func TestCheckEnvelopeTruncatesTheBody(t *testing.T) {
	body := `{"success":false,"error":""` + strings.Repeat(" ", 4000) + `}`
	err := checkEnvelope([]byte(body), envelopeRequired)
	if err == nil {
		t.Fatal("err = nil, want a failure")
	}
	if len(err.Error()) > 400 {
		t.Fatalf("error length = %d, want it truncated near 300", len(err.Error()))
	}
	if !strings.HasSuffix(err.Error(), "…") {
		t.Fatalf("error = %q, want the truncation marker", err.Error())
	}
}

// ── the command inventory (the contract's pin) ────────────────────────────────.

// envelopeContract is what ONE command does when the server answers
// {"success":false,…}. The table below is the inventory of every registered
// command; TestEnvelopeInventoryIsComplete makes it impossible to add a command
// without deciding its class, and TestEnvelopeContractPerCommand actually runs
// each one against a failing server instead of trusting the label.
type envelopeContract int

const (
	// contractExit1: the command reads a ctx envelope, so success:false is a
	// command error — stderr + exit 1, with the server's reason.
	contractExit1 envelopeContract = iota
	// contractExit0: no ctx envelope on this path (a local command, or
	// GET /health, which is not an /api route and carries no frame).
	contractExit0
	// contractSkip: not exercised here, with a reason. Either the command has
	// its own exit scale, or running it in-process would need an interactive
	// terminal, a real repo or a network host.
	contractSkip
)

// failStdout pins what a FAILED call leaves on stdout. The exit code is only
// half the contract: the whole point of T03-13 is that a script keeps the bytes
// it already parses and gains a non-zero exit — an implementation that returns
// the error before printing would pass an exit-code-only test and still break
// every `ctx stats | jq`.
type failStdout int

const (
	// stdoutUnchecked: this command renders instead of echoing the response,
	// and it never rendered a failed one. Covered by the fakesrv matrix.
	stdoutUnchecked failStdout = iota
	// stdoutCarriesBody: the response JSON must still reach stdout, failure or
	// not. These are the commands whose stdout WAS the machine contract.
	stdoutCarriesBody
	// stdoutSilentByDesign: this wave deliberately removed what stdout said on
	// failure, because what it said was false ("No API keys provisioned." for a
	// 403). Nothing on stdout, reason on stderr.
	stdoutSilentByDesign
)

// inventoryEntry pins one command. oldExit records what the command did with
// success:false BEFORE T03-13 — the table doubles as the migration note, so the
// exit-code change stays readable next to the code that made it.
type inventoryEntry struct {
	args     []string
	contract envelopeContract
	oldExit  int    // 0 or 1; -1 = not applicable (contractSkip)
	reason   string // required for contractSkip
	stdout   failStdout
}

const fixtureProject = "11111111-2222-3333-4444-555566667777"

// envelopeInventory maps a cobra command path (without the leading "ctx ") to
// its contract. args are the FULL argv for the run, path included.
func envelopeInventory() map[string]inventoryEntry {
	e1 := func(old int, args ...string) inventoryEntry {
		return inventoryEntry{args: args, contract: contractExit1, oldExit: old}
	}
	// echo = exit 1 AND the response body still on stdout (the pipe contract).
	echo := func(old int, args ...string) inventoryEntry {
		return inventoryEntry{args: args, contract: contractExit1, oldExit: old, stdout: stdoutCarriesBody}
	}
	// mute = exit 1 and nothing on stdout, deliberately (see stdoutSilentByDesign).
	mute := func(old int, args ...string) inventoryEntry {
		return inventoryEntry{args: args, contract: contractExit1, oldExit: old, stdout: stdoutSilentByDesign}
	}
	e0 := func(args ...string) inventoryEntry {
		return inventoryEntry{args: args, contract: contractExit0, oldExit: 0}
	}
	skip := func(reason string) inventoryEntry {
		return inventoryEntry{contract: contractSkip, oldExit: -1, reason: reason}
	}
	modelsStub := skip("W05-19 S1 stub: RunE returns errModelsNotImplemented without a request; S2 flips it to e1")
	return map[string]inventoryEntry{
		// ── the exit-code change of T03-13: 0 → 1 ──────────────────────────
		"query":              e1(1, "query", "hello"), // --json was the 0
		"save":               echo(0, "save", "reference", "t", "c"),
		"search":             echo(0, "search", "foo"),
		"stats":              echo(0, "stats"),
		"categories":         echo(0, "categories"),
		"get":                echo(0, "get", "b1"),
		"delete":             echo(0, "delete", "b1"),
		"list-meta":          echo(0, "list-meta"),
		"digest":             echo(0, "digest"),
		"manage":             echo(0, "manage", "stats"),
		"dream":              echo(0, "dream"),
		"dream stats":        echo(0, "dream", "stats"),
		"dream review":       echo(0, "dream", "review"),
		"dream enable":       echo(0, "dream", "enable"),
		"dream disable":      echo(0, "dream", "disable"),
		"dream throttle":     echo(0, "dream", "throttle", "30"),
		"mcp":                mute(0, "mcp"),
		"mcp list":           mute(0, "mcp", "list"),
		"mcp delete":         echo(0, "mcp", "delete", "cid-123"),
		"keys":               mute(0, "keys"),
		"keys list":          mute(0, "keys", "list"),
		"keys delete":        echo(0, "keys", "delete", "key-456"),
		"block-grant create": echo(0, "block-grant", "create", "b1", fixtureProject),
		"block-grant revoke": echo(0, "block-grant", "revoke", "b1", fixtureProject),
		"block-grant list":   echo(0, "block-grant", "list"),

		// ── already exit 1 before T03-13, now through the same checker ─────
		"guard":                          e1(1, "guard"),
		"guard list":                     e1(1, "guard", "list"),
		"guard stats":                    e1(1, "guard", "stats"),
		"guard resolve":                  e1(1, "guard", "resolve", "b1", "keep"),
		"dream resolve":                  e1(1, "dream", "resolve", "b1", "b2", "relates_to", "confirm"),
		"mcp add":                        e1(1, "mcp", "add", "label"),
		"keys create":                    e1(1, "keys", "create", "label", "--home", "private"),
		"settings":                       e1(1, "settings"),
		"settings list":                  e1(1, "settings", "list"),
		"settings get":                   e1(1, "settings", "get", "dream.enabled"),
		"settings set":                   e1(1, "settings", "set", "dream.enabled", "true"),
		"settings unset":                 e1(1, "settings", "unset", "dream.enabled"),
		"secrets":                        e1(1, "secrets"),
		"secrets list":                   e1(1, "secrets", "list"),
		"secrets rm":                     e1(1, "secrets", "rm", "openai"),
		"types":                          e1(1, "types"),
		"types list":                     e1(1, "types", "list"),
		"types get":                      e1(1, "types", "get", "note"),
		"types set":                      e1(1, "types", "set", "note", "--display", "Note"),
		"types rm":                       e1(1, "types", "rm", "note"),
		"backends":                       e1(1, "backends"),
		"backends list":                  e1(1, "backends", "list"),
		"backends create":                e1(1, "backends", "create", `{"name":"x","role":"chat","base_url":"http://h/v1"}`),
		"backends update":                e1(1, "backends", "update", "be1", `{"enabled":false}`),
		"backends delete":                e1(1, "backends", "delete", "be1"),
		"backends test":                  e1(1, "backends", "test", "be1"),
		"eject":                          e1(1, "eject"),
		"quota":                          e1(1, "quota"),
		"quota set":                      e1(1, "quota", "set", "acme", "--daily-calls", "10"),
		"blocks audit":                   e1(1, "blocks", "audit"),
		"blocks audit status":            e1(1, "blocks", "audit", "status"),
		"blocks audit sample":            e1(1, "blocks", "audit", "sample"),
		"blocks audit start":             e1(1, "blocks", "audit", "start"),
		"blocks classify":                e1(1, "blocks", "classify"),
		"blocks classify status":         e1(1, "blocks", "classify", "status"),
		"blocks classify dry-run":        e1(1, "blocks", "classify", "dry-run"),
		"blocks classify start":          e1(1, "blocks", "classify", "start"),
		"tenant":                         e1(1, "tenant"),
		"tenant list":                    e1(1, "tenant", "list"),
		"tenant get":                     e1(1, "tenant", "get", fixtureProject),
		"tenant create":                  e1(1, "tenant", "create", "acme", "Acme"),
		"tenant update":                  e1(1, "tenant", "update", fixtureProject, "--status", "active"),
		"tenant delete":                  e1(1, "tenant", "delete", fixtureProject),
		"tenant usage":                   e1(1, "tenant", "usage"),
		"tenant limit set":               e1(1, "tenant", "limit", "set", fixtureProject, "--max-scopes", "1", "--max-keys", "1"),
		"tenant grant create":            e1(1, "tenant", "grant", "create", fixtureProject, "work"),
		"tenant grant list":              e1(1, "tenant", "grant", "list"),
		"tenant grant delete":            e1(1, "tenant", "grant", "delete", "g1"),
		"project":                        e1(1, "project"),
		"project show":                   e1(1, "project", "show"),
		"project list":                   e1(1, "project", "list"),
		"project init":                   e1(1, "project", "init", "--identity", "manual:fixture"),
		"project issues":                 e1(1, "project", "issues", "--project", fixtureProject),
		"project issues list":            e1(1, "project", "issues", "list", "--project", fixtureProject),
		"project issues show":            e1(1, "project", "issues", "show", "i1", "--project", fixtureProject),
		"project issues create":          e1(1, "project", "issues", "create", "t", "--body", "b", "--project", fixtureProject),
		"project issues comment":         e1(1, "project", "issues", "comment", "i1", "--body", "b", "--project", fixtureProject),
		"project issues status":          e1(1, "project", "issues", "status", "i1", "done", "--project", fixtureProject),
		"project issues sync":            e1(1, "project", "issues", "sync", "--project", fixtureProject),
		"kanban":                         e1(1, "kanban", "--project", fixtureProject),
		"admin embed-migration":          e1(1, "admin", "embed-migration"),
		"admin embed-migration status":   e1(1, "admin", "embed-migration", "status"),
		"admin embed-migration create":   e1(1, "admin", "embed-migration", "create", "--to", "m2"),
		"admin embed-migration pause":    e1(1, "admin", "embed-migration", "pause"),
		"admin embed-migration resume":   e1(1, "admin", "embed-migration", "resume"),
		"admin embed-migration confirm":  e1(1, "admin", "embed-migration", "confirm"),
		"admin embed-migration cleanup":  e1(1, "admin", "embed-migration", "cleanup"),
		"admin embed-migration purge":    e1(1, "admin", "embed-migration", "purge"),
		"admin embed-migration abort":    e1(1, "admin", "embed-migration", "abort", "--reason", "r"),
		"admin embed-migration rollback": e1(1, "admin", "embed-migration", "rollback", "--reason", "r"),
		"admin embed-migration failures": e1(1, "admin", "embed-migration", "failures"),
		// api kept its exit codes and GAINED the printed body on failure — the
		// behaviour docs/cli.md has always described for this command.
		"api": echo(1, "api", "GET", "/api/project"),

		// ── W05-19 S1: registered stubs, no request yet ────────────────────
		// RunE answers errModelsNotImplemented before any request, so these
		// cannot meet the contract run. S2 replaces each with e1(-1, …) —
		// TestModelsInventoryFlipsToExit1 pins the args and stays red until then.
		"models consumers list":       modelsStub,
		"models consumers create":     modelsStub,
		"models consumers rotate":     modelsStub,
		"models consumers revoke":     modelsStub,
		"models consumers deactivate": modelsStub,
		"models aliases list":         modelsStub,

		// ── outside the contract: exit 0 stays exit 0 ──────────────────────
		"health":  e0("health"), // GET /health, no /api envelope at all
		"brief":   e0("brief"),  // hook surface: degrades silently by design
		"persist": e0("persist"),

		// ── not exercised in-process ───────────────────────────────────────
		"secrets set":    skip("value comes from stdin ONLY (an argv value is rejected); covered by the fakesrv matrix"),
		"secrets rotate": skip("same stdin-only contract as `secrets set`"),
		"backends seed":  skip("multi-step provisioning; seals a secret and writes pool rows"),
		"project detect": skip("purely local (git/.ctx-project), never reaches a server"),
		"ingest":         skip("walks a vault and streams batches on its own client (ingest.go:451)"),
		"statusline":     skip("reads the Claude Code JSON from stdin and must never fail a prompt"),
		"contract":       skip("own exit scale 0/1/2/3 (contract.go:119-131); pinned by contract_test.go"),
		"init":           skip("first-run wizard: builds its own client and probes api.github.com"),
	}
}

// The walker: a command that is registered but not listed has no decided
// contract. This is the mechanism — without it the inventory would be a
// snapshot that rots at the next added command.
func TestEnvelopeInventoryIsComplete(t *testing.T) {
	inv := envelopeInventory()
	root := &cobra.Command{Use: "ctx", Short: "Your AI's save game"}
	RegisterCommands(root)

	seen := map[string]bool{}
	var missing []string
	walkCommands(root, func(c *cobra.Command) {
		if c == root || !c.Runnable() {
			return // groups print help and exit 0; they read no envelope
		}
		path := strings.TrimPrefix(c.CommandPath(), "ctx ")
		seen[path] = true
		if _, ok := inv[path]; !ok {
			missing = append(missing, path)
		}
	})
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("command %q is registered but missing from envelopeInventory — decide its envelope contract", m)
	}
	for path, entry := range inv {
		if !seen[path] {
			t.Errorf("envelopeInventory lists %q, which is not a runnable registered command", path)
		}
		if entry.contract == contractSkip && entry.reason == "" {
			t.Errorf("%q is contractSkip without a reason", path)
		}
		if entry.contract != contractSkip && len(entry.args) == 0 {
			t.Errorf("%q has no args to run", path)
		}
	}
}

// The behavioural half: every command of the inventory actually runs against a
// server that answers success:false, and its exit class is checked. A command
// that loses the envelope check goes red HERE, not in a grep.
func TestEnvelopeContractPerCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"error":"gate failure"}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("CTX_BASE_URL", srv.URL)
	t.Setenv("CTX_KEY", "fixture-key")
	t.Setenv("CTX_ADMIN_KEY", "fixture-key")
	t.Setenv("NO_COLOR", "1")

	// A manual: identity is authoritative from the file (project.go:200-203),
	// so the project commands resolve without a git repo and without a TTY.
	work := t.TempDir()
	if err := os.WriteFile(work+"/.ctx-project", []byte("identity=manual:fixture\n"), 0o600); err != nil {
		t.Fatalf("writing .ctx-project: %v", err)
	}
	t.Chdir(work)

	inv := envelopeInventory()
	paths := make([]string, 0, len(inv))
	for p := range inv {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		entry := inv[path]
		if entry.contract == contractSkip {
			continue
		}
		t.Run(path, func(t *testing.T) {
			stdout, err := runCommandCaptured(t, entry.args)
			switch entry.contract {
			case contractExit1:
				if err == nil {
					t.Fatalf("`ctx %s` returned nil on success:false — exit 0 on a failed call is the trap T03-13 closed", path)
				}
				if !strings.Contains(err.Error(), "gate failure") {
					t.Fatalf("`ctx %s` failed with %q, want the server reason \"gate failure\" — "+
						"the command must map the envelope, not fail on something else", path, err)
				}
			case contractExit0:
				if err != nil {
					t.Fatalf("`ctx %s` returned %v, want nil: this path carries no ctx envelope", path, err)
				}
			case contractSkip:
			}
			// The other half of the contract: what the failure left on stdout.
			switch entry.stdout {
			case stdoutCarriesBody:
				if !strings.Contains(stdout, `"success": false`) {
					t.Fatalf("`ctx %s` printed %q on a failed call, want the response body — "+
						"checking BEFORE printing gives the right exit code and still breaks every `| jq`", path, stdout)
				}
			case stdoutSilentByDesign:
				if stdout != "" {
					t.Fatalf("`ctx %s` printed %q on a failed call, want nothing: the line it used to print "+
						"(\"No … registered/provisioned.\") was a false statement about a rejected request", path, stdout)
				}
			case stdoutUnchecked:
			}
		})
	}
}

// runCommandCaptured executes one argv on a fresh command tree and returns the
// command error plus what landed on stdout. stdout and stderr go to SEPARATE
// files on purpose: the point of this fixture is that a failure keeps stdout
// intact while the reason goes to stderr, and one merged sink could not tell
// those apart. A file also makes StdoutIsTTY() false, so every command takes
// its machine-readable branch — the one a script or a pipe sees.
func runCommandCaptured(t *testing.T, args []string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	outFile, err := os.Create(dir + "/stdout.txt")
	if err != nil {
		t.Fatalf("temp stdout: %v", err)
	}
	defer func() { _ = outFile.Close() }()
	errFile, err := os.Create(dir + "/stderr.txt")
	if err != nil {
		t.Fatalf("temp stderr: %v", err)
	}
	defer func() { _ = errFile.Close() }()

	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outFile, errFile
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()

	root := &cobra.Command{Use: "ctx", Short: "Your AI's save game"}
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetOut(outFile)
	root.SetErr(errFile)
	RegisterCommands(root)
	root.SetArgs(args)
	runErr := root.Execute()

	captured, readErr := os.ReadFile(dir + "/stdout.txt")
	if readErr != nil {
		t.Fatalf("reading captured stdout: %v", readErr)
	}
	return string(captured), runErr
}

// The negative probe, permanent: if checkEnvelope ever stops turning
// success:false into an error, THIS goes red — independently of any command.
// The per-command test above proves the wiring, this one proves the mechanism.
// `ctx query` is the one command whose two output branches disagree, so it is
// the one that can lose bytes quietly: --json echoes the response, the human
// form renders it, and BOTH have always dumped a body they could not decode to
// stdout with the decoder's complaint. The envelope error must not overtake
// that fallback — checking too early makes `ctx query` answer a proxy's HTML
// 502 with an empty stdout and a truncated echo on stderr.
func TestQueryKeepsItsRawFallback(t *testing.T) {
	const garbage = "not json at all"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(garbage))
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("CTX_BASE_URL", srv.URL)
	t.Setenv("CTX_KEY", "fixture-key")
	t.Chdir(t.TempDir())

	for _, args := range [][]string{
		{"query", "hello"},
		{"query", "--json", "hello"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, err := runCommandCaptured(t, args)
			if err == nil {
				t.Fatalf("an undecodable answer must not exit 0")
			}
			if !strings.Contains(stdout, garbage) {
				t.Fatalf("stdout = %q, want the raw body %q — the fallback has to run before the envelope error", stdout, garbage)
			}
		})
	}
}

func TestEnvelopeContractDetectsARemovedCheck(t *testing.T) {
	failing := []byte(`{"success":false,"error":"gate failure"}`)
	for _, mode := range []envelopeMode{envelopeRequired, envelopeOptional} {
		if err := checkEnvelope(failing, mode); err == nil {
			t.Errorf("mode %d: success:false produced no error", mode)
		} else if err.Error() != "gate failure" {
			t.Errorf("mode %d: error = %q, want the server reason verbatim", mode, err)
		}
	}
	// And the success frame must stay silent in both modes, or every command
	// would fail on its happy path.
	ok := []byte(`{"success":true}`)
	for _, mode := range []envelopeMode{envelopeRequired, envelopeOptional} {
		if err := checkEnvelope(ok, mode); err != nil {
			t.Errorf("mode %d: success:true produced %v", mode, err)
		}
	}
}
