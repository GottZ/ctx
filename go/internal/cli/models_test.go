package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// W05-19 S1 — the red tests of `ctx models`. Every test here runs the real
// command tree against an httptest server and states the contract of
// docs/cli.md "Model API: consumers & aliases": reveal-once of a minted key,
// a projection (never the raw body) for lists, TTY vs. pipe, exit 1 on
// success:false, client-side validation before any request, and the exact
// manage-action wire bodies. Against the S1 stubs (errModelsNotImplemented,
// no request) each one fails with a message naming its gate.

const (
	modelsTestConsumerID = "0197bbbb-0000-7000-8000-000000000001"
	modelsTestKeyID      = "0197bbbb-0000-7000-8000-000000000002"
	modelsTestTenantID   = "0197aaaa-0000-7000-8000-000000000001"
	modelsTestAliasID    = "0197cccc-0000-7000-8000-000000000001"
	// The plaintexts a minted answer carries. The key hint the server lists
	// ("ctxm_…" + tail, modelkey.go Hint) legitimately starts with ctxm_ as
	// well, so every reveal count below counts the FULL plaintext.
	modelsTestCreatedKey = "ctxm_TESTSECRETcreate0123456789abcdefABCDEF"
	modelsTestRotatedKey = "ctxm_TESTSECRETrotate0123456789abcdefABCDEF"
	modelsTestLeakKey    = "ctxm_LEAKplaintext0123456789abcdefABCDEF"
	modelsTestKeyHash    = "key_hash"
	modelsSaveWarning    = "Save the key now — it cannot be retrieved later."
)

// modelsKeyJSON is one keyView; extra is spliced in (leak injection).
func modelsKeyJSON(extra string) string {
	return `{"id":"` + modelsTestKeyID + `","hint":"ctxm_…CDEF","status":"active",` +
		`"created_at":"2026-11-07T10:00:00Z","expires_at":"2027-11-07T10:00:00Z",` +
		`"revoked_at":null,"rotated_from":null,"last_used_at":null` + extra + `}`
}

// modelsConsumerJSON is one consumerView; extra is spliced into the consumer
// and its key (leak injection).
func modelsConsumerJSON(label, extra string) string {
	return `{"id":"` + modelsTestConsumerID + `","tenant_id":"` + modelsTestTenantID + `",` +
		`"tenant_slug":"acme","label":"` + label + `","description":"","kind":"app",` +
		`"tenant_scope":"acme:main","all_models":false,"models":["` + modelsTestAliasID + `"],` +
		`"prompt_sensitivity":"credentials","fair_weight":1,"admission_lane":"bulk",` +
		`"allowed_cidrs":[],"allow_plaintext":false,"active":true,"status":"active",` +
		`"last_used_at":null,"created_at":"2026-11-07T10:00:00Z","created_by_label":"owner",` +
		`"keys":[` + modelsKeyJSON(extra) + `],"keys_total":1,"peer":null` + extra + `}`
}

func modelsAliasesJSON() string {
	return `{"success":true,"model_aliases":[{"id":"` + modelsTestAliasID + `","scope":"_global",` +
		`"name":"chat-default","kind":"chat","role":"chat","sensitivity":"credentials","policy":{},` +
		`"description":"","enabled":true,"created_at":"2026-11-07T10:00:00Z",` +
		`"updated_at":"2026-11-07T10:00:00Z","writable":false}],"count":1}`
}

func modelsCreateJSON() string {
	return `{"success":true,"consumer":` + modelsConsumerJSON("ci-bot", "") + `,"api_key":"` + modelsTestCreatedKey + `"}`
}

func modelsRotateJSON() string {
	return `{"success":true,"key":` + modelsKeyJSON("") + `,"api_key":"` + modelsTestRotatedKey + `"}`
}

// modelsFixture is one httptest server plus the request log.
type modelsFixture struct {
	t      *testing.T
	mu     sync.Mutex
	bodies [][]byte
	status int
	resp   string
}

func newModelsFixture(t *testing.T, status int, resp string) (*modelsFixture, func() (*Client, error)) {
	t.Helper()
	f := &modelsFixture{t: t, status: status, resp: resp}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, b)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.resp))
	}))
	t.Cleanup(srv.Close)
	return f, func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}
}

func (f *modelsFixture) requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
}

// modelsRun is what one command left behind.
type modelsRun struct {
	stdout, stderr string
	err            error
}

// runModels executes `ctx models <args>` with the TTY seam set to tty and
// captures stdout and stderr SEPARATELY (reveal-once is a per-stream claim).
func runModels(t *testing.T, getClient func() (*Client, error), tty bool, args ...string) modelsRun {
	t.Helper()
	oldTTY := modelsStdoutIsTTY
	modelsStdoutIsTTY = func() bool { return tty }
	defer func() { modelsStdoutIsTTY = oldTTY }()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	cmd := modelsCmd(getClient)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	runErr := cmd.Execute()
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	wg.Wait()
	_ = outR.Close()
	_ = errR.Close()
	return modelsRun{stdout: outBuf.String(), stderr: errBuf.String(), err: runErr}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// canonJSON re-marshals a JSON document with sorted keys, so two bodies
// compare by content, not by field order.
func canonJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %v: %q", err, raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

// topLevelKeys parses stdout as one JSON object and returns its sorted keys.
func topLevelKeys(t *testing.T, gate, stdout string) (map[string]json.RawMessage, []string) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &obj); err != nil {
		t.Fatalf("%s: stdout is not one JSON object (%v): %q", gate, err, stdout)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return obj, keys
}

// ── 1–3, 5: reveal-once ──────────────────────────────────────────────────────.

func TestModelsConsumersCreateRevealOncePipe(t *testing.T) {
	_, getClient := newModelsFixture(t, http.StatusOK, modelsCreateJSON())
	r := runModels(t, getClient, false, "consumers", "create", "--label", "ci-bot")
	if r.err != nil {
		t.Fatalf("reveal-once (pipe): `ctx models consumers create` failed with %q, want success and the key on stdout", r.err)
	}
	if n := strings.Count(r.stdout, modelsTestCreatedKey); n != 1 {
		t.Fatalf("reveal-once (pipe): plaintext on stdout %d×, want exactly 1×; stdout=%q", n, r.stdout)
	}
	if strings.Contains(r.stderr, "ctxm_") {
		t.Fatalf("reveal-once (pipe): key material on stderr: %q", r.stderr)
	}
	obj, keys := topLevelKeys(t, "reveal-once (pipe)", r.stdout)
	if want := []string{"api_key", "consumer"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("reveal-once (pipe): JSON keys %v, want exactly %v (a projection, not the raw body)", keys, want)
	}
	var key string
	if err := json.Unmarshal(obj["api_key"], &key); err != nil || key != modelsTestCreatedKey {
		t.Fatalf("reveal-once (pipe): api_key = %s, want %q", obj["api_key"], modelsTestCreatedKey)
	}
	if !strings.Contains(string(obj["consumer"]), modelsTestConsumerID) {
		t.Fatalf("reveal-once (pipe): consumer = %s, want the created consumer", obj["consumer"])
	}
}

func TestModelsConsumersCreateRevealOnceTTY(t *testing.T) {
	_, getClient := newModelsFixture(t, http.StatusOK, modelsCreateJSON())
	r := runModels(t, getClient, true, "consumers", "create", "--label", "ci-bot")
	if r.err != nil {
		t.Fatalf("reveal-once (TTY): `ctx models consumers create` failed with %q, want success and the key on stdout", r.err)
	}
	if n := strings.Count(r.stdout, modelsTestCreatedKey); n != 1 {
		t.Fatalf("reveal-once (TTY): plaintext on stdout %d×, want exactly 1×; stdout=%q", n, r.stdout)
	}
	if !regexp.MustCompile(`(?m)^\s*api_key:\s+` + regexp.QuoteMeta(modelsTestCreatedKey) + `\s*$`).MatchString(r.stdout) {
		t.Fatalf("reveal-once (TTY): no line `api_key: <key>` on stdout: %q", r.stdout)
	}
	warned := false
	for _, line := range strings.Split(r.stdout, "\n") {
		if !strings.Contains(line, modelsSaveWarning) {
			continue
		}
		warned = true
		if strings.Contains(line, "ctxm_") {
			t.Fatalf("reveal-once (TTY): the warning line carries key material: %q", line)
		}
	}
	if !warned {
		t.Fatalf("reveal-once (TTY): warning %q missing from stdout: %q", modelsSaveWarning, r.stdout)
	}
	if strings.Contains(r.stderr, "ctxm_") {
		t.Fatalf("reveal-once (TTY): key material on stderr: %q", r.stderr)
	}
}

func TestModelsKeyRotateRevealOnce(t *testing.T) {
	cases := []struct {
		name string
		tty  bool
		args []string
	}{
		{"pipe", false, []string{"consumers", "rotate", modelsTestConsumerID, "--grace", "0s"}},
		{"json_on_tty", true, []string{"consumers", "rotate", modelsTestConsumerID, "--grace", "0s", "--json"}},
	}
	wantBody := `{"action":"model-key-rotate","data":{"consumer_id":"` + modelsTestConsumerID + `","grace":0}}`
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, getClient := newModelsFixture(t, http.StatusOK, modelsRotateJSON())
			r := runModels(t, getClient, c.tty, c.args...)
			reqs := f.requests()
			if len(reqs) != 1 {
				t.Fatalf("reveal-once (rotate): %d requests, want 1 (err=%q)", len(reqs), errText(r.err))
			}
			if got, want := canonJSON(t, reqs[0]), canonJSON(t, []byte(wantBody)); got != want {
				t.Fatalf("rotate wire body:\n got %s\nwant %s", got, want)
			}
			if r.err != nil {
				t.Fatalf("reveal-once (rotate): failed with %q", r.err)
			}
			if n := strings.Count(r.stdout, modelsTestRotatedKey); n != 1 {
				t.Fatalf("reveal-once (rotate): plaintext on stdout %d×, want exactly 1×; stdout=%q", n, r.stdout)
			}
			if strings.Contains(r.stderr, "ctxm_") {
				t.Fatalf("reveal-once (rotate): key material on stderr: %q", r.stderr)
			}
			if _, keys := topLevelKeys(t, "reveal-once (rotate)", r.stdout); !reflect.DeepEqual(keys, []string{"api_key", "key"}) {
				t.Fatalf("reveal-once (rotate): JSON keys %v, want exactly [api_key key]", keys)
			}
		})
	}
}

// TestModelsMintedKeyNeverLost: an answer that carries a minted key but is
// otherwise broken still hands the key over — exactly once, on stdout — and
// only then fails; the error names the defect, never the key.
func TestModelsMintedKeyNeverLost(t *testing.T) {
	cases := []struct {
		name, resp, key, defect string
		args                    []string
	}{
		{"create_consumer_missing", `{"success":true,"api_key":"` + modelsTestCreatedKey + `"}`,
			modelsTestCreatedKey, "consumer", []string{"consumers", "create", "--label", "ci-bot"}},
		{"create_consumer_not_an_object", `{"success":true,"consumer":"garbage","api_key":"` + modelsTestCreatedKey + `"}`,
			modelsTestCreatedKey, "consumer", []string{"consumers", "create", "--label", "ci-bot"}},
		{"create_consumer_without_id", `{"success":true,"consumer":{},"api_key":"` + modelsTestCreatedKey + `"}`,
			modelsTestCreatedKey, "consumer", []string{"consumers", "create", "--label", "ci-bot"}},
		{"rotate_key_missing", `{"success":true,"api_key":"` + modelsTestRotatedKey + `"}`,
			modelsTestRotatedKey, "key", []string{"consumers", "rotate", modelsTestConsumerID}},
		// No envelope at all: checkEnvelope's "request failed: <body>"
		// (truncateForError) would carry the key to stderr — the key is
		// taken out of the answer before the envelope check (lead decision
		// S1, sub-agent finding).
		{"create_without_envelope", `{"api_key":"` + modelsTestCreatedKey + `"}`,
			modelsTestCreatedKey, "success", []string{"consumers", "create", "--label", "ci-bot"}},
		{"rotate_without_envelope", `{"api_key":"` + modelsTestRotatedKey + `"}`,
			modelsTestRotatedKey, "success", []string{"consumers", "rotate", modelsTestConsumerID}},
	}
	for _, c := range cases {
		for _, tty := range []bool{false, true} {
			name := c.name + map[bool]string{false: "/pipe", true: "/tty"}[tty]
			t.Run(name, func(t *testing.T) {
				_, getClient := newModelsFixture(t, http.StatusOK, c.resp)
				r := runModels(t, getClient, tty, c.args...)
				if n := strings.Count(r.stdout, c.key); n != 1 {
					t.Fatalf("minted key lost: plaintext on stdout %d×, want exactly 1× even on a broken answer; stdout=%q err=%q",
						n, r.stdout, errText(r.err))
				}
				if r.err == nil {
					t.Fatal("minted key: a broken answer must still end with exit 1 after the key is out")
				}
				if strings.Contains(r.err.Error(), "ctxm_") || strings.Contains(r.stderr, "ctxm_") {
					t.Fatalf("minted key: key material in the error/stderr: err=%q stderr=%q", r.err, r.stderr)
				}
				if !strings.Contains(strings.ToLower(r.err.Error()), c.defect) {
					t.Fatalf("minted key: error %q does not name the defect (%q)", r.err, c.defect)
				}
			})
		}
	}
}

// ── 4: lists are projections ─────────────────────────────────────────────────.

func TestModelsConsumersListNeverPrintsPlaintext(t *testing.T) {
	leak := `,"api_key":"` + modelsTestLeakKey + `","` + modelsTestKeyHash + `":"5f1d0c0ffee5f1d0c0ffee"`
	resp := `{"success":true,"consumers":[` + modelsConsumerJSON("leaky-app", leak) + `],"next_cursor":null` + leak + `}`
	cases := []struct {
		name string
		tty  bool
		args []string
	}{
		{"pipe", false, []string{"consumers", "list"}},
		{"tty_table", true, []string{"consumers", "list"}},
		{"json_on_tty", true, []string{"consumers", "list", "--json"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, getClient := newModelsFixture(t, http.StatusOK, resp)
			r := runModels(t, getClient, c.tty, c.args...)
			if r.err != nil {
				t.Fatalf("list projection: `ctx models consumers list` failed with %q", r.err)
			}
			if !strings.Contains(r.stdout, "leaky-app") {
				t.Fatalf("list projection: the consumer was not printed at all: %q", r.stdout)
			}
			for _, banned := range []string{modelsTestLeakKey, "ctxm_LEAK", modelsTestKeyHash} {
				if strings.Contains(r.stdout, banned) || strings.Contains(r.stderr, banned) {
					t.Fatalf("list projection: %q reached the output — a field outside the allowlist was passed through:\n%s", banned, r.stdout)
				}
			}
			if c.tty && c.name == "tty_table" {
				return
			}
			if !strings.Contains(r.stdout, `ctxm_…CDEF`) {
				t.Fatalf("list projection: the key hint (an allowlisted field) is missing from the JSON: %q", r.stdout)
			}
		})
	}
}

// ── 6: exit 1 on success:false ───────────────────────────────────────────────.

func TestModelsExitOneOnSuccessFalse(t *testing.T) {
	cases := [][]string{
		{"consumers", "list"},
		{"consumers", "create", "--label", "x"},
		{"consumers", "rotate", modelsTestConsumerID},
		{"consumers", "revoke", modelsTestKeyID},
		{"consumers", "deactivate", modelsTestConsumerID},
		{"aliases", "list"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			f, getClient := newModelsFixture(t, http.StatusOK, `{"success":false,"error":"boom"}`)
			r := runModels(t, getClient, false, args...)
			if n := len(f.requests()); n != 1 {
				t.Fatalf("exit-1 gate: `ctx models %s` sent %d requests, want exactly 1 (err=%q)", strings.Join(args, " "), n, errText(r.err))
			}
			if r.err == nil || !strings.Contains(r.err.Error(), "boom") {
				t.Fatalf("exit-1 gate: err = %q, want the server reason \"boom\" as the command error", errText(r.err))
			}
		})
	}
}

// ── 7: TTY vs. pipe ──────────────────────────────────────────────────────────.

func TestModelsTTYvsPipe(t *testing.T) {
	listResp := `{"success":true,"consumers":[` + modelsConsumerJSON("ci-bot", "") + `],"next_cursor":null}`
	cases := []struct {
		name, resp, jsonKey string
		args, header        []string
	}{
		{"consumers_list", listResp, "consumers", []string{"consumers", "list"},
			[]string{"ID", "LABEL", "KIND", "STATUS", "LANE", "SENSITIVITY", "KEYS", "LAST", "USED"}},
		{"aliases_list", modelsAliasesJSON(), "model_aliases", []string{"aliases", "list"},
			[]string{"NAME", "KIND", "ROLE", "SCOPE", "SENSITIVITY", "ENABLED", "ID"}},
	}
	for _, c := range cases {
		t.Run(c.name+"/tty", func(t *testing.T) {
			_, getClient := newModelsFixture(t, http.StatusOK, c.resp)
			r := runModels(t, getClient, true, c.args...)
			if r.err != nil {
				t.Fatalf("TTY table: failed with %q", r.err)
			}
			first := strings.SplitN(strings.TrimLeft(r.stdout, "\n"), "\n", 2)[0]
			if got := strings.Fields(first); !reflect.DeepEqual(got, c.header) {
				t.Fatalf("TTY table: header %q, want columns %v; stdout=%q", first, c.header, r.stdout)
			}
		})
		for _, mode := range []struct {
			name  string
			tty   bool
			extra []string
		}{{"pipe", false, nil}, {"json_on_tty", true, []string{"--json"}}} {
			t.Run(c.name+"/"+mode.name, func(t *testing.T) {
				_, getClient := newModelsFixture(t, http.StatusOK, c.resp)
				r := runModels(t, getClient, mode.tty, append(append([]string{}, c.args...), mode.extra...)...)
				if r.err != nil {
					t.Fatalf("JSON mode: failed with %q", r.err)
				}
				obj, _ := topLevelKeys(t, "JSON mode", r.stdout)
				var items []json.RawMessage
				if err := json.Unmarshal(obj[c.jsonKey], &items); err != nil || len(items) != 1 {
					t.Fatalf("JSON mode: %q = %s, want one item", c.jsonKey, obj[c.jsonKey])
				}
			})
		}
	}
}

// ── 8: validation before any request ─────────────────────────────────────────.

func TestModelsFlagValidation(t *testing.T) {
	cid := modelsTestConsumerID
	cases := []struct {
		name, want string
		args       []string
	}{
		{"create_without_label", "label", []string{"consumers", "create"}},
		{"create_empty_label", "label", []string{"consumers", "create", "--label", ""}},
		{"create_lane", "lane", []string{"consumers", "create", "--label", "x", "--lane", "fast"}},
		{"create_sensitivity", "sensitivity", []string{"consumers", "create", "--label", "x", "--sensitivity", "secret"}},
		{"create_fair_weight_0", "fair-weight", []string{"consumers", "create", "--label", "x", "--fair-weight", "0"}},
		{"create_fair_weight_101", "fair-weight", []string{"consumers", "create", "--label", "x", "--fair-weight", "101"}},
		{"create_expires", "expires", []string{"consumers", "create", "--label", "x", "--expires", "2026-13-01"}},
		{"rotate_grace_negative", "grace", []string{"consumers", "rotate", cid, "--grace", "-1s"}},
		{"rotate_expires", "expires", []string{"consumers", "rotate", cid, "--expires", "tomorrow"}},
		{"list_limit_0", "limit", []string{"consumers", "list", "--limit", "0"}},
		{"list_limit_negative", "limit", []string{"consumers", "list", "--limit", "-5"}},
		{"list_status", "status", []string{"consumers", "list", "--status", "bogus"}},
		{"list_status_mixed", "status", []string{"consumers", "list", "--status", "active,bogus"}},
		// Regression pins, green already against the S1 stubs (cobra.ExactArgs
		// fires before RunE): no gate, kept so S2 cannot drop the arity.
		{"rotate_no_arg", "accepts 1 arg", []string{"consumers", "rotate"}},
		{"revoke_no_arg", "accepts 1 arg", []string{"consumers", "revoke"}},
		{"deactivate_no_arg", "accepts 1 arg", []string{"consumers", "deactivate"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, getClient := newModelsFixture(t, http.StatusOK, `{"success":true}`)
			r := runModels(t, getClient, false, c.args...)
			if r.err == nil {
				t.Fatalf("validation: `ctx models %s` succeeded, want a client-side error", strings.Join(c.args, " "))
			}
			if !strings.Contains(r.err.Error(), c.want) {
				t.Fatalf("validation: error %q does not name %q", r.err, c.want)
			}
			if n := len(f.requests()); n != 0 {
				t.Fatalf("validation: %d requests sent, want 0 — validation runs before any HTTP call", n)
			}
		})
	}
}

// ── 9: wire bodies ───────────────────────────────────────────────────────────.

func TestModelsRequestBodies(t *testing.T) {
	ok := `{"success":true,"consumer":` + modelsConsumerJSON("ci-bot", "") + `,"key":` + modelsKeyJSON("") +
		`,"api_key":"` + modelsTestCreatedKey + `","consumers":[],"next_cursor":null,"model_aliases":[],"count":0,"cancelled_inflight":0}`
	cid, kid, tid := modelsTestConsumerID, modelsTestKeyID, modelsTestTenantID
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"create_all_flags", []string{"consumers", "create", "--label", "app1", "--description", "d",
			"--tenant", tid, "--tenant-scope", "acme:main", "--model", "a1", "--model", "a2", "--all-models",
			"--sensitivity", "personal", "--lane", "urgent", "--fair-weight", "5",
			"--cidr", "10.0.0.0/8", "--cidr", "192.168.0.0/16", "--allow-plaintext", "--expires", "2026-12-31T23:59:59Z"},
			`{"action":"model-consumer-create","data":{"label":"app1","description":"d","tenant_id":"` + tid + `",` +
				`"tenant_scope":"acme:main","models":["a1","a2"],"all_models":true,"prompt_sensitivity":"personal",` +
				`"admission_lane":"urgent","fair_weight":5,"allowed_cidrs":["10.0.0.0/8","192.168.0.0/16"],` +
				`"allow_plaintext":true,"expires_at":"2026-12-31T23:59:59Z"}}`},
		{"create_only_label", []string{"consumers", "create", "--label", "app1"},
			`{"action":"model-consumer-create","data":{"label":"app1"}}`},
		{"list_no_flags", []string{"consumers", "list"},
			`{"action":"model-consumer-list","data":{}}`},
		{"list_status_all", []string{"consumers", "list", "--status", "all"},
			`{"action":"model-consumer-list","data":{"status":["all"]}}`},
		{"list_all_flags", []string{"consumers", "list", "--status", "active,expiring", "--q", "bot",
			"--tenant", tid, "--limit", "10", "--cursor", "c1"},
			`{"action":"model-consumer-list","data":{"status":["active","expiring"],"q":"bot","tenant_id":"` + tid +
				`","limit":10,"cursor":"c1"}}`},
		{"rotate_grace_24h", []string{"consumers", "rotate", cid, "--grace", "24h", "--expires", "2027-01-01T00:00:00Z"},
			`{"action":"model-key-rotate","data":{"consumer_id":"` + cid + `","grace":86400,"expires_at":"2027-01-01T00:00:00Z"}}`},
		{"rotate_no_flags", []string{"consumers", "rotate", cid},
			`{"action":"model-key-rotate","data":{"consumer_id":"` + cid + `"}}`},
		{"revoke", []string{"consumers", "revoke", kid},
			`{"action":"model-key-revoke","data":{"key_id":"` + kid + `"}}`},
		{"deactivate", []string{"consumers", "deactivate", cid},
			`{"action":"model-consumer-delete","data":{"id":"` + cid + `"}}`},
		{"aliases_list", []string{"aliases", "list"},
			`{"action":"model-alias-list"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, getClient := newModelsFixture(t, http.StatusOK, ok)
			r := runModels(t, getClient, false, c.args...)
			reqs := f.requests()
			if len(reqs) != 1 {
				t.Fatalf("wire body: `ctx models %s` sent %d requests, want 1 (err=%q)", strings.Join(c.args, " "), len(reqs), errText(r.err))
			}
			if got, want := canonJSON(t, reqs[0]), canonJSON(t, []byte(c.want)); got != want {
				t.Fatalf("wire body:\n got %s\nwant %s", got, want)
			}
			if r.err != nil {
				t.Fatalf("wire body: command failed with %q on a success answer", r.err)
			}
		})
	}
}

// ── 10: cancelled_inflight ───────────────────────────────────────────────────.

func TestModelsRevokeCancelledInflight(t *testing.T) {
	cases := []struct {
		name, resp, want string
		args             []string
	}{
		{"revoke_2", `{"success":true,"cancelled_inflight":2}`, "2", []string{"consumers", "revoke", modelsTestKeyID}},
		{"revoke_null", `{"success":true,"cancelled_inflight":null}`, "unknown", []string{"consumers", "revoke", modelsTestKeyID}},
		{"deactivate_2", `{"success":true,"cancelled_inflight":2}`, "2", []string{"consumers", "deactivate", modelsTestConsumerID}},
		{"deactivate_null", `{"success":true,"cancelled_inflight":null}`, "unknown", []string{"consumers", "deactivate", modelsTestConsumerID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, getClient := newModelsFixture(t, http.StatusOK, c.resp)
			r := runModels(t, getClient, true, c.args...)
			if r.err != nil {
				t.Fatalf("cancelled_inflight (TTY): failed with %q", r.err)
			}
			line := regexp.MustCompile(`(?m)^\s*cancelled_inflight:\s+` + c.want + `\s*$`)
			if !line.MatchString(r.stdout) {
				t.Fatalf("cancelled_inflight (TTY): want a line `cancelled_inflight: %s`, stdout=%q", c.want, r.stdout)
			}
		})
	}
}

// ── inventory flip ───────────────────────────────────────────────────────────.

// TestModelsInventoryFlipsToExit1 keeps the S1 skip entries of
// envelopeInventory from outliving the stubs: S2 must turn each into
// e1(-1, …) with these args so TestEnvelopeContractPerCommand runs it against
// a success:false server.
func TestModelsInventoryFlipsToExit1(t *testing.T) {
	want := map[string][]string{
		"models consumers list":       {"models", "consumers", "list"},
		"models consumers create":     {"models", "consumers", "create", "--label", "x"},
		"models consumers rotate":     {"models", "consumers", "rotate", fixtureProject},
		"models consumers revoke":     {"models", "consumers", "revoke", fixtureProject},
		"models consumers deactivate": {"models", "consumers", "deactivate", fixtureProject},
		"models aliases list":         {"models", "aliases", "list"},
	}
	inv := envelopeInventory()
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		e, ok := inv[path]
		switch {
		case !ok:
			t.Errorf("envelope inventory: %q missing", path)
		case e.contract != contractExit1:
			t.Errorf("envelope inventory: %q is not contractExit1 yet (S1 stub skip) — flip it to e1(-1, %q…)", path, strings.Join(want[path], " "))
		case !reflect.DeepEqual(e.args, want[path]):
			t.Errorf("envelope inventory: %q args %v, want %v", path, e.args, want[path])
		}
	}
}
