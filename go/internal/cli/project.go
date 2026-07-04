// ctx project — the project register CLI (workflow W5, design/03-workflow-api-cli.md
// §4.3/§4.7). A project binds one repo corpus to exactly one tenant scope
// (Model C); the identity survives clones and moves.
//
//	ctx project                       # = show (detect in CWD, then look it up)
//	ctx project detect                # resolve the identity locally, no server call
//	ctx project init [--identity ID | --repo URL] [--scope NAME]
//	ctx project show                  # detect, then show the registered project
//	ctx project list                  # every project your key can read
//
// Identity precedence (§4.3): a GitHub `origin` remote → github:owner/repo; else
// a single-root git repo → git-root:<sha> (survives clones); else a manual slug
// (interactive on a TTY, an error when piped). The `.ctx-project` file is a
// DETECT SHORTCUT, not a trust anchor: for github:/git-root: identities it is
// honored ONLY when it matches the independent git detection — a mismatch (a
// cloned foreign repo with a planted file that would redirect issue writes into
// someone else's corpus) forces confirmation on a TTY and ERRORS when piped
// (confused-deputy defense). Only manual: identities, which have no independent
// source, are authoritative from the file.
//
// Every server call parses the {success,…} envelope: success:false reaches
// stderr with exit code 1 (the checkSettingsEnvelope contract) — these commands
// must not inherit the PrintJSON-and-exit-0 trap, `ctx project` output feeds
// scripts.

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// ctxProjectFile is the repo-root detect-shortcut file (identity only, no secret).
const ctxProjectFile = ".ctx-project"

// projectRow mirrors the server's store.ProjectRow wire shape (handler/project.go).
// forge/sync_cursor/metadata stay raw for pass-through printing.
type projectRow struct {
	ID               string          `json:"id"`
	TenantID         string          `json:"tenant_id"`
	Scope            string          `json:"scope"`
	Identity         string          `json:"identity"`
	DisplayName      string          `json:"display_name"`
	Forge            json.RawMessage `json:"forge"`
	WebhookSecretRef *string         `json:"webhook_secret_ref"`
	SyncStatus       string          `json:"sync_status"`
	LastSyncAt       *string         `json:"last_sync_at"`
	SyncCursor       json.RawMessage `json:"sync_cursor"`
	CreatedAt        string          `json:"created_at"`
	Metadata         json.RawMessage `json:"metadata"`
}

// resolvedIdentity is the outcome of local detection. The field order is the
// pipe-mode JSON golden shape — do not reorder without updating the golden test.
type resolvedIdentity struct {
	Identity string `json:"identity"`
	Source   string `json:"source"`
}

// identityKinds is the closed prefix set the server also enforces (§3.1).
var identityKinds = []string{"github:", "git-root:", "manual:"}

// validCLIIdentity mirrors handler.validIdentity: non-empty, one closed prefix,
// something after the prefix.
func validCLIIdentity(id string) bool {
	for _, p := range identityKinds {
		if strings.HasPrefix(id, p) && len(id) > len(p) {
			return true
		}
	}
	return false
}

// ── local git / file detection ───────────────────────────────────────────────.

// gitOutput runs `git <args…>` in dir and returns trimmed stdout; ok is false on
// any non-zero exit (git absent, not a repo, no such ref). stderr is discarded —
// callers treat failure as "signal absent", never as a hard error.
func gitOutput(dir string, args ...string) (string, bool) {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: args are fixed literals from this package (detection reads), never user input
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return strings.TrimSpace(out.String()), true
}

// githubRe matches the GitHub remote URL forms (scp-style, https, ssh) and
// captures owner + repo. The optional `.git` suffix and a trailing slash are
// stripped by the pattern itself.
var githubRe = regexp.MustCompile(`^(?:git@github\.com:|(?:https?|ssh)://(?:git@)?github\.com/)([^/]+)/([^/]+?)(?:\.git)?/?$`)

// githubSlug parses a remote URL into "owner/repo" when it points at github.com.
func githubSlug(remote string) (string, bool) {
	m := githubRe.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return "", false
	}
	return m[1] + "/" + m[2], true
}

// detectFromGit computes the INDEPENDENT project identity from git state in dir.
// Precedence (§4.3): a GitHub origin remote → github:owner/repo; else a
// single-root git repo → git-root:<sha>. ok=false means "no independent identity
// here" — not a git repo, or several root commits without a GitHub remote — and
// the caller falls to the manual path.
func detectFromGit(dir string) (identity, source string, ok bool) {
	if remote, got := gitOutput(dir, "remote", "get-url", "origin"); got {
		if slug, isGH := githubSlug(remote); isGH {
			return "github:" + slug, "github-remote", true
		}
	}
	if _, inRepo := gitOutput(dir, "rev-parse", "--is-inside-work-tree"); !inRepo {
		return "", "", false
	}
	roots, got := gitOutput(dir, "rev-list", "--max-parents=0", "HEAD")
	if !got || roots == "" {
		return "", "", false
	}
	// Several root commits (merged histories) without a GitHub remote have no
	// unambiguous identity → manual (§4.3 step 3).
	if fields := strings.Fields(roots); len(fields) == 1 {
		return "git-root:" + fields[0], "git-root", true
	}
	return "", "", false
}

// readCtxProjectFile reads the identity from the repo-root .ctx-project file.
// Format: an `identity=<value>` line (comments with '#' ignored); a bare line
// containing ':' is tolerated for hand-written files. ok=false when the file is
// absent.
func readCtxProjectFile(dir string) (identity string, ok bool, err error) {
	data, e := os.ReadFile(filepath.Join(dir, ctxProjectFile))
	if e != nil {
		if os.IsNotExist(e) {
			return "", false, nil
		}
		return "", false, e
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if v, found := strings.CutPrefix(line, "identity="); found {
			return strings.TrimSpace(v), true, nil
		}
		if strings.Contains(line, ":") {
			return line, true, nil
		}
	}
	return "", false, nil
}

// writeCtxProjectFile writes the identity (only — never a secret) to the repo
// root. The comment documents that the file is a re-verified shortcut, so a
// reader who greps it understands it is not trusted blindly.
func writeCtxProjectFile(dir, identity string) error {
	content := "# ctx project identity — a detect shortcut, re-verified against git on read\n" +
		"# (design/03 §4.3). NOT a trust anchor: for github:/git-root: identities detect\n" +
		"# honors this file ONLY when it matches independent git detection; a mismatch\n" +
		"# forces confirmation on a TTY and errors when piped.\n" +
		"identity=" + identity + "\n"
	//nolint:gosec // G306: .ctx-project holds only the (non-secret) identity and is meant to be committed — world-readable 0644 is correct, unlike the 0600 secret files
	return os.WriteFile(filepath.Join(dir, ctxProjectFile), []byte(content), 0o644)
}

// ── precedence resolver (the §4.3 confused-deputy gate) ───────────────────────.

// prompter abstracts the interactive channel so the resolver is unit-testable.
// piped() true forces errors instead of blocking reads (§4.3 pipe rule).
type prompter interface {
	piped() bool
	confirm(question string) (bool, error)
	askSlug() (string, error)
}

// resolveIdentity applies the §4.3 precedence with the .ctx-project mismatch
// gate. The file is a shortcut, never a trust anchor: a github:/git-root: file
// identity is accepted ONLY when it equals the independent git detection; a
// mismatch (or a non-manual file with no independent source to confirm it)
// forces confirmation on a TTY and errors when piped. A manual: file is
// authoritative (no independent source exists).
func resolveIdentity(dir string, p prompter) (resolvedIdentity, error) {
	indID, indSource, indOK := detectFromGit(dir)
	fileID, hasFile, err := readCtxProjectFile(dir)
	if err != nil {
		return resolvedIdentity{}, fmt.Errorf("reading %s: %w", ctxProjectFile, err)
	}

	if hasFile {
		// manual: no independent source can confirm or deny it → authoritative.
		if strings.HasPrefix(fileID, "manual:") {
			return resolvedIdentity{Identity: fileID, Source: "ctx-project-file"}, nil
		}
		// github:/git-root:: the file must AGREE with independent detection.
		if indOK && indID == fileID {
			return resolvedIdentity{Identity: fileID, Source: "ctx-project-file"}, nil
		}
		detected := indID
		if !indOK {
			detected = "(none — no verifiable git identity here)"
		}
		msg := fmt.Sprintf("%s claims %q but independent git detection says %s", ctxProjectFile, fileID, detected)
		if p.piped() {
			return resolvedIdentity{}, fmt.Errorf("%s; refusing in pipe mode (run `ctx project init` to rewrite the file, or run on a TTY to confirm)", msg)
		}
		ok, cErr := p.confirm(msg + "\nUse the .ctx-project identity anyway?")
		if cErr != nil {
			return resolvedIdentity{}, cErr
		}
		if !ok {
			return resolvedIdentity{}, fmt.Errorf("aborted: %s", msg)
		}
		return resolvedIdentity{Identity: fileID, Source: "ctx-project-file-confirmed"}, nil
	}

	// No file: independent detection, else the manual fallback.
	if indOK {
		return resolvedIdentity{Identity: indID, Source: indSource}, nil
	}
	if p.piped() {
		return resolvedIdentity{}, fmt.Errorf("no GitHub remote and no single-root git repo here, and no %s file; cannot prompt for a manual slug in pipe mode", ctxProjectFile)
	}
	slug, aErr := p.askSlug()
	if aErr != nil {
		return resolvedIdentity{}, aErr
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return resolvedIdentity{}, fmt.Errorf("empty project slug")
	}
	return resolvedIdentity{Identity: "manual:" + slug, Source: "manual"}, nil
}

// stdinPrompter is the production prompter: prompts on stderr (stdout stays
// clean for JSON piping), reads answers from stdin. Its methods are only reached
// when piped()==false, so they never block a non-interactive run.
type stdinPrompter struct{}

func (stdinPrompter) piped() bool { return !StdinIsTTY() }

func (stdinPrompter) confirm(question string) (bool, error) {
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil //nolint:nilerr // EOF at the prompt = "no", not a failure
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

func (stdinPrompter) askSlug() (string, error) {
	fmt.Fprint(os.Stderr, "No git identity here. Enter a manual project slug: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no slug entered")
	}
	return strings.TrimSpace(line), nil
}

// ── scope-name derivation ─────────────────────────────────────────────────────.

var scopeCharRe = regexp.MustCompile(`[^a-z0-9-]+`)

// deriveScopeName suggests a scope NAME (the part after the tenant slug) from an
// identity: the repo name for github:, "repo-<sha12>" for git-root:, the slug for
// manual:. The result is sanitized to the server's scope charset and length; ""
// means "cannot derive, require --scope".
func deriveScopeName(identity string) string {
	var base string
	switch {
	case strings.HasPrefix(identity, "github:"):
		parts := strings.SplitN(strings.TrimPrefix(identity, "github:"), "/", 2)
		base = parts[len(parts)-1]
	case strings.HasPrefix(identity, "git-root:"):
		sha := strings.TrimPrefix(identity, "git-root:")
		if len(sha) > 12 {
			sha = sha[:12]
		}
		base = "repo-" + sha
	case strings.HasPrefix(identity, "manual:"):
		base = strings.TrimPrefix(identity, "manual:")
	}
	return sanitizeScopeName(base)
}

// sanitizeScopeName lowercases, maps invalid runs to '-', trims leading/trailing
// '-', and caps at 24 chars (the server rejects anything else, handler/project.go).
func sanitizeScopeName(s string) string {
	s = scopeCharRe.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 24 {
		s = strings.Trim(s[:24], "-")
	}
	return s
}

// ── output helpers ────────────────────────────────────────────────────────────.

// emitIdentity prints a resolved identity: JSON when piped (the golden shape),
// two human lines on a TTY.
func emitIdentity(id resolvedIdentity) {
	if !StdoutIsTTY() {
		out, _ := json.MarshalIndent(id, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Printf("identity: %s\n", id.Identity)
	fmt.Printf("source:   %s\n", id.Source)
}

// printProjectTable renders the list view (TTY).
func printProjectTable(rows []projectRow) {
	if len(rows) == 0 {
		fmt.Println("No projects registered. Use: ctx project init")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tIDENTITY\tSCOPE\tSYNC\tDISPLAY")
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Identity, r.Scope, r.SyncStatus, r.DisplayName)
	}
	_ = w.Flush()
}

// printProjectDetail renders one project (TTY).
func printProjectDetail(r projectRow) {
	fmt.Printf("%s\n", r.Identity)
	fmt.Printf("  id:            %s\n", r.ID)
	fmt.Printf("  scope:         %s\n", r.Scope)
	fmt.Printf("  tenant_id:     %s\n", r.TenantID)
	fmt.Printf("  sync_status:   %s\n", r.SyncStatus)
	if r.DisplayName != "" {
		fmt.Printf("  display_name:  %s\n", r.DisplayName)
	}
	if r.LastSyncAt != nil {
		fmt.Printf("  last_sync_at:  %s\n", *r.LastSyncAt)
	}
	if len(r.Forge) > 0 && string(r.Forge) != "{}" && string(r.Forge) != "null" {
		fmt.Printf("  forge:         %s\n", compactRaw(r.Forge))
	}
	webhook := "none"
	if r.WebhookSecretRef != nil && *r.WebhookSecretRef != "" {
		webhook = "configured"
	}
	fmt.Printf("  webhook:       %s\n", webhook)
}

// ── command tree ──────────────────────────────────────────────────────────────.

func projectCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"proj"},
		Short:   "Project register: detect a repo's identity, register it, list projects",
		Long: "A project binds one repo corpus to exactly one tenant scope (Model C): the\n" +
			"scope is the isolation boundary, so a scope belongs to one tenant and each\n" +
			"project's issues live only in its own scope. `ctx project` with no subcommand\n" +
			"= `show` (detect in the current directory, then look the project up). Identity\n" +
			"precedence: a GitHub origin remote, else a single-root git repo (git-root:<sha>,\n" +
			"which survives clones), else a manual slug. A .ctx-project file is only a\n" +
			"shortcut — it is honored for github:/git-root: identities ONLY when it matches\n" +
			"the independent git detection.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProjectShow(getClient)
		},
	}
	cmd.AddCommand(projectDetectCmd())
	cmd.AddCommand(projectInitCmd(getClient))
	cmd.AddCommand(projectShowCmd(getClient))
	cmd.AddCommand(projectListCmd(getClient))
	cmd.AddCommand(issuesCmd(getClient))
	return cmd
}

func projectDetectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "detect",
		Short: "Resolve this repo's project identity locally (no server call)",
		Long: "Resolve the project identity of the current directory without contacting the\n" +
			"server: a GitHub origin remote → github:owner/repo, else a single-root git\n" +
			"repo → git-root:<sha>, else a manual slug (prompted on a TTY, an error when\n" +
			"piped). A .ctx-project file is a shortcut, honored only when it matches the\n" +
			"independent git detection; a mismatch forces confirmation (or errors when\n" +
			"piped). TTY: human lines; pipe: JSON {identity, source}.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveIdentity(".", stdinPrompter{})
			if err != nil {
				return err
			}
			emitIdentity(id)
			return nil
		},
	}
}

func projectListCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List every project your key can read (scope intersection)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProjectList(getClient)
		},
	}
}

func projectShowCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Detect this repo's identity, then show its registered project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProjectShow(getClient)
		},
	}
}

func projectInitCmd(getClient func() (*Client, error)) *cobra.Command {
	var identity, repo, scope string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Register this repo as a project (idempotent) and write .ctx-project",
		Long: "Register the current repo as a project: resolve its identity (or take\n" +
			"--identity / --repo), then create the project via the server (idempotent —\n" +
			"re-running with the same identity returns the existing project, never a\n" +
			"duplicate). On success .ctx-project is written to the repo root (the identity\n" +
			"only, never a secret). The scope defaults to a name derived from the identity;\n" +
			"override with --scope.",
		Example: `  ctx project init
  ctx project init --scope backend
  ctx project init --repo https://github.com/acme/api
  ctx project init --identity manual:internal-docs --scope docs`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProjectInit(getClient, identity, repo, scope)
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "explicit project identity (github:o/r | git-root:sha | manual:slug); skips detection")
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub repo URL to derive the identity from (overrides local detection)")
	// The --scope flag names a scope; a scope belongs to ONE tenant (the server
	// prefixes it with the tenant slug), so this help must reference tenants
	// (help_consistency rule b).
	cmd.Flags().StringVar(&scope, "scope", "", "scope NAME for the project corpus; a scope belongs to one tenant, so the server prefixes it with your tenant slug (default: derived from the identity)")
	return cmd
}

// ── run functions ─────────────────────────────────────────────────────────────.

func runProjectList(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodGet, "/api/project", nil)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Projects []projectRow `json:"projects"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	printProjectTable(payload.Projects)
	return nil
}

func runProjectShow(getClient func() (*Client, error)) error {
	id, err := resolveIdentity(".", stdinPrompter{})
	if err != nil {
		return err
	}
	c, err := getClient()
	if err != nil {
		return err
	}
	rows, err := lookupByIdentity(c, id.Identity)
	if err != nil {
		return err
	}
	if !StdoutIsTTY() {
		// Pipe: emit the raw server list for the identity (stable, scriptable).
		out, _ := json.MarshalIndent(map[string]any{"success": true, "identity": id.Identity, "source": id.Source, "projects": rows}, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	if len(rows) == 0 {
		fmt.Printf("%s (%s)\n  not registered — run: ctx project init\n", id.Identity, id.Source)
		return nil
	}
	printProjectDetail(rows[0])
	return nil
}

func runProjectInit(getClient func() (*Client, error), identity, repo, scope string) error {
	// 1. Determine the identity: explicit flag > --repo > local detection.
	var chosen resolvedIdentity
	switch {
	case identity != "":
		if !validCLIIdentity(identity) {
			return fmt.Errorf("--identity must start with github: | git-root: | manual: (got %q)", identity)
		}
		chosen = resolvedIdentity{Identity: identity, Source: "flag"}
	case repo != "":
		slug, ok := githubSlug(repo)
		if !ok {
			return fmt.Errorf("--repo %q is not a recognized github.com URL", repo)
		}
		chosen = resolvedIdentity{Identity: "github:" + slug, Source: "flag-repo"}
	default:
		id, err := resolveIdentity(".", stdinPrompter{})
		if err != nil {
			return err
		}
		chosen = id
	}

	c, err := getClient()
	if err != nil {
		return err
	}

	// 2. Existence probe (idempotency, W4 contract): if it already exists, show it
	//    and (re)write .ctx-project so a fresh clone gets the shortcut.
	rows, err := lookupByIdentity(c, chosen.Identity)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		if werr := maybeWriteMarker(chosen.Identity); werr != nil {
			Errorf("warning: could not write %s: %v", ctxProjectFile, werr)
		}
		if !StdoutIsTTY() {
			out, _ := json.MarshalIndent(map[string]any{"success": true, "already_registered": true, "project": rows[0]}, "", "  ")
			fmt.Println(string(out))
			return nil
		}
		fmt.Printf("already registered:\n")
		printProjectDetail(rows[0])
		return nil
	}

	// 3. Create. Derive the scope name if not given; the server validates + prefixes.
	scopeName := scope
	if scopeName == "" {
		scopeName = deriveScopeName(chosen.Identity)
		if scopeName == "" {
			return fmt.Errorf("could not derive a scope name from %q; pass --scope", chosen.Identity)
		}
	}
	body := map[string]any{"identity": chosen.Identity, "scope": scopeName}
	resp, _, err := c.Do(http.MethodPost, "/api/project", body)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}

	// 4. Write the .ctx-project marker (identity only).
	if werr := maybeWriteMarker(chosen.Identity); werr != nil {
		Errorf("warning: project created but could not write %s: %v", ctxProjectFile, werr)
	}

	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Project projectRow `json:"project"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	fmt.Printf("registered:\n")
	printProjectDetail(payload.Project)
	return nil
}

// maybeWriteMarker writes .ctx-project into the git repo root (or the CWD when
// not in a repo) — never a secret, only the identity.
func maybeWriteMarker(identity string) error {
	dir := "."
	if root, ok := gitOutput(".", "rev-parse", "--show-toplevel"); ok && root != "" {
		dir = root
	}
	return writeCtxProjectFile(dir, identity)
}

// lookupByIdentity fetches the projects matching an identity via GET
// /api/project?identity=… and returns the parsed rows (envelope-checked).
func lookupByIdentity(c *Client, identity string) ([]projectRow, error) {
	resp, _, err := c.Do(http.MethodGet, "/api/project?identity="+url.QueryEscape(identity), nil)
	if err != nil {
		return nil, err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return nil, err
	}
	var payload struct {
		Projects []projectRow `json:"projects"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return nil, fmt.Errorf("unparseable project response: %s", truncateForError(resp))
	}
	return payload.Projects, nil
}
