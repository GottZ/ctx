// ctx project issues — the project-scoped issue CLI family (workflow W8,
// design/03-workflow-api-cli.md §4.6/§4.7). It rides the W6 read + W7 write REST
// surface (/api/project/{id}/issues*) and resolves the project the same way W5
// does: detect the repo's identity in the CWD (github: / git-root: / manual:),
// then look it up — an explicit --project (a project id OR an identity) overrides
// the detection.
//
//	ctx project issues [list]  [--state s] [--label l]... [--q text] [--after cur]
//	ctx project issues show    <block-id>                 # detail + comment thread
//	ctx project issues create  <title> | --title t        # body via --body OR stdin
//	ctx project issues comment <block-id>                 # body via --body OR stdin
//	ctx project issues status  <block-id> <new-status>    # verb is `status` (K15)
//
// The verb for a state change is `status` (masterplan K15), NOT `state`/`move`.
//
// Every server call parses the {success,…} envelope: success:false reaches
// stderr with exit code 1 (the checkSettingsEnvelope contract) — these commands
// feed scripts, so they must not inherit the PrintJSON-and-exit-0 trap. TTY:
// tables / human lines; pipe: the raw server JSON (stable shapes, golden-pinned).
//
// SECURITY (§5.4): issue/comment TITLES, BODIES and LABELS are attacker-controlled
// text (any GitHub user can open an issue in a mirrored repo). Every such value
// printed to a TTY runs through sanitizeTerminal (allowlist, sanitize.go) so a
// crafted title cannot drive escape sequences into the terminal / agent log / CI
// output. The pipe (JSON) path is the machine contract and is NOT rewritten — the
// server already JSON-escapes C0 controls, and rewriting would break the golden.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// issueUUIDRe recognizes a canonical UUID so --project can accept a project id
// directly (used verbatim as the {id} path segment) OR an identity string.
var issueUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// issueListRow mirrors store.WorkflowBlockRow (the W6 list/board wire shape).
type issueListRow struct {
	ID             string `json:"id"`
	TypeName       string `json:"type_name"`
	Title          string `json:"title"`
	WorkflowStatus string `json:"workflow_status"`
	UpdatedAt      string `json:"updated_at"`
}

// issueBlock mirrors the fields of store.Block the issue detail + comment thread
// carry (the full block shape has more; these are what the CLI renders).
type issueBlock struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	Content        string         `json:"content"`
	Tags           []string       `json:"tags"`
	Scope          string         `json:"scope"`
	WorkflowStatus string         `json:"workflow_status"`
	UpdatedAt      string         `json:"updated_at"`
	Metadata       map[string]any `json:"metadata"`
}

// ── command tree ──────────────────────────────────────────────────────────────.

func issuesCmd(getClient func() (*Client, error)) *cobra.Command {
	var project string
	// The persistent --project override is shared by every subcommand (and the
	// default list run). project == "" ⇒ detect the identity in the CWD (§4.3).
	cmd := &cobra.Command{
		Use:     "issues",
		Aliases: []string{"issue", "i"},
		Short:   "List, read, create, comment on and re-status a project's issues",
		Long: "Work with the issues of a project. The project is detected from the current\n" +
			"repo (like `ctx project`), or named explicitly with --project (a project id or\n" +
			"an identity). `ctx project issues` with no subcommand = `list`. On a TTY the\n" +
			"output is a table / human lines; piped, it is the raw server JSON.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIssuesList(cmd, getClient, project)
		},
	}
	cmd.PersistentFlags().StringVar(&project, "project", "",
		"project override: a project id (UUID) or an identity (github:o/r | git-root:sha | manual:slug); default = detect in the current repo")

	cmd.AddCommand(issuesListCmd(getClient, &project))
	cmd.AddCommand(issuesShowCmd(getClient, &project))
	cmd.AddCommand(issuesCreateCmd(getClient, &project))
	cmd.AddCommand(issuesCommentCmd(getClient, &project))
	cmd.AddCommand(issuesStatusCmd(getClient, &project))
	cmd.AddCommand(issuesSyncCmd(getClient, &project))
	return cmd
}

func issuesListCmd(getClient func() (*Client, error), project *string) *cobra.Command {
	var state, q, sort, after string
	var labels []string
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List issues (keyset paginated); filter by --state/--label/--q",
		Long: "List a project's issues, newest first. Filters: --state (one workflow status),\n" +
			"--label (repeatable), --q (full-text). Pages are keyset-based: the response\n" +
			"carries an opaque cursor; pass it back with --after to fetch the next page (the\n" +
			"list scales to 10k+ issues, so there is no offset paging). --sort created gives\n" +
			"a gap-free immutable traversal for export.",
		Example: `  ctx project issues list --state open
  ctx project issues list --label bug --label p1
  ctx project issues list --q "timeout" --after <cursor>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIssuesListFiltered(cmd, getClient, *project, issueListFilter{
				state: state, q: q, sort: sort, after: after, labels: labels, limit: limit,
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter to one workflow status (e.g. open, closed)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "filter by label (repeatable; all must match)")
	cmd.Flags().StringVar(&q, "q", "", "full-text query over title/body")
	cmd.Flags().StringVar(&sort, "sort", "", "sort order: updated (default) or created (immutable, gap-free)")
	cmd.Flags().StringVar(&after, "after", "", "opaque cursor from a previous page (fetch the next page)")
	cmd.Flags().IntVar(&limit, "limit", 0, "page size (server default when 0)")
	return cmd
}

func issuesShowCmd(getClient func() (*Client, error), project *string) *cobra.Command {
	return &cobra.Command{
		Use:   "show <block-id>",
		Short: "Show one issue in full plus its comment thread",
		Long: "Show an issue's full fields and the first page of its comment thread. The\n" +
			"block id is a full issue block id (a foreign or unknown id reads as not found).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIssuesShow(getClient, *project, args[0])
		},
	}
}

func issuesCreateCmd(getClient func() (*Client, error), project *string) *cobra.Command {
	var title, body, status string
	var tags []string
	cmd := &cobra.Command{
		Use:   "create [title]",
		Short: "Create an issue (title as arg or --title; body via --body or stdin)",
		Long: "Create an issue in the project's scope. The title is the positional arg or\n" +
			"--title; the body is --body or, if neither, read from stdin (so\n" +
			"`echo body | ctx project issues create --title t` works). --status must be a\n" +
			"valid entry state for the type policy (else the server refuses it).",
		Example: `  ctx project issues create "Login button misaligned"
  echo "steps to reproduce…" | ctx project issues create --title "Crash on save"
  ctx project issues create --title "Flaky test" --label ci --body "see run #42"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			t := strings.TrimSpace(strings.Join(args, " "))
			if t == "" {
				t = title
			}
			if t == "" {
				return fmt.Errorf("title required (positional arg or --title)")
			}
			content := ""
			if cmd.Flags().Changed("body") {
				content = body
			} else if stdin, ok := ReadStdin(); ok {
				content = stdin
			}
			return runIssuesCreate(getClient, *project, t, content, status, tags)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "issue title (alternative to the positional arg)")
	cmd.Flags().StringVar(&body, "body", "", "issue body (alternative to stdin)")
	cmd.Flags().StringVar(&status, "status", "", "initial workflow status (must be a valid entry state; default from type policy)")
	cmd.Flags().StringArrayVar(&tags, "label", nil, "label to attach (repeatable)")
	return cmd
}

func issuesCommentCmd(getClient func() (*Client, error), project *string) *cobra.Command {
	var body, author string
	cmd := &cobra.Command{
		Use:   "comment <block-id>",
		Short: "Add a comment to an issue (body via --body or stdin)",
		Long: "Append a comment to an issue's thread. The body is --body or, if not given,\n" +
			"read from stdin. --author labels the comment (default: anon).",
		Example: `  ctx project issues comment <id> --body "fixed in v1.2"
  echo "reproduced on staging" | ctx project issues comment <id>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			content := ""
			if cmd.Flags().Changed("body") {
				content = body
			} else if stdin, ok := ReadStdin(); ok {
				content = stdin
			}
			if content == "" {
				return fmt.Errorf("comment body required (--body or stdin)")
			}
			return runIssuesComment(getClient, *project, args[0], content, author)
		},
	}
	cmd.Flags().StringVar(&body, "body", "", "comment body (alternative to stdin)")
	cmd.Flags().StringVar(&author, "author", "", "comment author label (default: anon)")
	return cmd
}

func issuesStatusCmd(getClient func() (*Client, error), project *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status <block-id> <new-status>",
		Short: "Change an issue's workflow status (verb is `status`, not state/move)",
		Long: "Move an issue to a new workflow status. The transition is validated against the\n" +
			"type's policy data server-side; an out-of-policy target is refused (exit 1 with\n" +
			"the server's reason).",
		Example: `  ctx project issues status <id> closed`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIssuesStatus(getClient, *project, args[0], args[1])
		},
	}
}

func issuesSyncCmd(getClient func() (*Client, error), project *string) *cobra.Command {
	var status bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Trigger a forge (GitHub) pull sync for the project, or poll its status",
		Long: "Start an on-demand forge pull sync (issues + comments) for the project, or,\n" +
			"with --status, poll the current/last run without starting one. Starting needs\n" +
			"WRITE access to the project's scope (whoever may write its issues may sync it).\n" +
			"A second start of the SAME project while one is in flight exits 1 (409); the\n" +
			"per-project rate limit and the daemon-wide concurrency cap also exit 1 (429/409)\n" +
			"with the server's reason. On a TTY the output is human lines; piped, raw JSON.",
		Example: `  ctx project issues sync
  ctx project issues sync --status`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIssuesSync(getClient, *project, status)
		},
	}
	cmd.Flags().BoolVar(&status, "status", false, "poll the sync status instead of starting a run")
	return cmd
}

// runIssuesSync starts a sync (POST) or polls status (GET). Both parse the
// {success,…} envelope (success:false ⇒ stderr + exit 1) so a 409/429 refusal is
// scriptable, not a silent exit 0.
func runIssuesSync(getClient func() (*Client, error), project string, status bool) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	pid, err := resolveProjectID(c, project)
	if err != nil {
		return err
	}
	method := http.MethodPost
	if status {
		method = http.MethodGet
	}
	resp, _, err := c.Do(method, "/api/project/"+pid+"/sync", nil)
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
	if status {
		printSyncStatus(resp)
	} else {
		printSyncStarted(resp)
	}
	return nil
}

// syncRun mirrors the run-state fields the CLI renders (store.SyncRunRow / the
// engine's in-memory SyncStatus overlap on these names).
type syncRun struct {
	Running   bool   `json:"running"`
	Fetched   int    `json:"fetched"`
	Applied   int    `json:"applied"`
	Conflicts int    `json:"conflicts"`
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
}

// printSyncStarted renders the POST response (a launched run).
func printSyncStarted(resp []byte) {
	var payload struct {
		Run syncRun `json:"run"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return
	}
	fmt.Printf("sync started (run %s)\n", shortID(payload.Run.RunID))
}

// printSyncStatus renders the GET response (current/last run + recent history).
func printSyncStatus(resp []byte) {
	var payload struct {
		SyncStatus string  `json:"sync_status"`
		LastSyncAt *string `json:"last_sync_at"`
		LastError  *string `json:"last_error"`
		Conflicts  int     `json:"conflicts"`
		Run        syncRun `json:"run"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return
	}
	fmt.Printf("status:    %s%s\n", sanitizeTerminal(payload.SyncStatus), map[bool]string{true: " (running)", false: ""}[payload.Run.Running])
	if payload.LastSyncAt != nil && *payload.LastSyncAt != "" {
		fmt.Printf("last sync: %s\n", shortDate(*payload.LastSyncAt))
	}
	fmt.Printf("conflicts: %d\n", payload.Conflicts)
	if payload.LastError != nil && *payload.LastError != "" {
		fmt.Printf("last error: %s\n", sanitizeTerminal(*payload.LastError))
	}
}

// ── project resolution ──────────────────────────────────────────────────────.

// resolveProjectID turns the --project override (or CWD detection) into a project
// id. A UUID override is used verbatim (no server call); an identity override —
// and the detected identity — is looked up via GET /api/project?identity=….
func resolveProjectID(c *Client, projectFlag string) (string, error) {
	if projectFlag != "" {
		if issueUUIDRe.MatchString(projectFlag) {
			return projectFlag, nil
		}
		if !validCLIIdentity(projectFlag) {
			return "", fmt.Errorf("--project %q is neither a project id (UUID) nor an identity (github:o/r | git-root:sha | manual:slug)", projectFlag)
		}
		return projectIDByIdentity(c, projectFlag)
	}
	id, err := resolveIdentity(".", stdinPrompter{})
	if err != nil {
		return "", err
	}
	return projectIDByIdentity(c, id.Identity)
}

// projectIDByIdentity resolves exactly one project id for an identity. Zero ⇒ a
// "not registered" error; more than one (a key that reads the same identity
// across tenants) ⇒ an ambiguity error asking for an explicit --project id.
func projectIDByIdentity(c *Client, identity string) (string, error) {
	rows, err := lookupByIdentity(c, identity)
	if err != nil {
		return "", err
	}
	switch len(rows) {
	case 0:
		return "", fmt.Errorf("no project registered for %s — run: ctx project init", identity)
	case 1:
		return rows[0].ID, nil
	default:
		return "", fmt.Errorf("%s matches %d projects; pass --project <id>", identity, len(rows))
	}
}

// ── run functions ─────────────────────────────────────────────────────────────.

type issueListFilter struct {
	state, q, sort, after string
	labels                []string
	limit                 int
}

// runIssuesList is the default (no-subcommand) list with no filters.
func runIssuesList(_ *cobra.Command, getClient func() (*Client, error), project string) error {
	return runIssuesListFiltered(nil, getClient, project, issueListFilter{})
}

func runIssuesListFiltered(_ *cobra.Command, getClient func() (*Client, error), project string, f issueListFilter) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	pid, err := resolveProjectID(c, project)
	if err != nil {
		return err
	}
	qv := url.Values{}
	if f.state != "" {
		qv.Set("state", f.state)
	}
	for _, l := range f.labels {
		qv.Add("labels", l)
	}
	if f.q != "" {
		qv.Set("q", f.q)
	}
	if f.sort != "" {
		qv.Set("sort", f.sort)
	}
	if f.after != "" {
		qv.Set("after", f.after)
	}
	if f.limit > 0 {
		qv.Set("limit", fmt.Sprintf("%d", f.limit))
	}
	path := "/api/project/" + pid + "/issues"
	if enc := qv.Encode(); enc != "" {
		path += "?" + enc
	}
	resp, _, err := c.Do(http.MethodGet, path, nil)
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
		Issues []issueListRow `json:"issues"`
		Cursor *string        `json:"cursor"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	printIssueTable(payload.Issues)
	if payload.Cursor != nil && *payload.Cursor != "" {
		fmt.Printf("\nnext page: --after %s\n", *payload.Cursor)
	}
	return nil
}

func runIssuesShow(getClient func() (*Client, error), project, blockID string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	pid, err := resolveProjectID(c, project)
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodGet, "/api/project/"+pid+"/issues/"+url.PathEscape(blockID), nil)
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
		Issue    issueBlock   `json:"issue"`
		Comments []issueBlock `json:"comments"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	printIssueDetail(payload.Issue, payload.Comments)
	return nil
}

func runIssuesCreate(getClient func() (*Client, error), project, title, content, status string, tags []string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	pid, err := resolveProjectID(c, project)
	if err != nil {
		return err
	}
	body := map[string]any{"title": title}
	if content != "" {
		body["content"] = content
	}
	if status != "" {
		body["status"] = status
	}
	if len(tags) > 0 {
		body["tags"] = tags
	}
	resp, _, err := c.Do(http.MethodPost, "/api/project/"+pid+"/issues", body)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	return printCreatedIssue(resp, "issue")
}

func runIssuesComment(getClient func() (*Client, error), project, blockID, content, author string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	pid, err := resolveProjectID(c, project)
	if err != nil {
		return err
	}
	body := map[string]any{"content": content}
	if author != "" {
		body["author"] = author
	}
	resp, _, err := c.Do(http.MethodPost, "/api/project/"+pid+"/issues/"+url.PathEscape(blockID)+"/comments", body)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	return printCreatedIssue(resp, "comment")
}

func runIssuesStatus(getClient func() (*Client, error), project, blockID, status string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	pid, err := resolveProjectID(c, project)
	if err != nil {
		return err
	}
	// PATCH {status}: an out-of-policy transition is a 422 with a {success:false,
	// error} envelope — checkSettingsEnvelope maps it to exit 1 + the reason.
	resp, _, err := c.Do(http.MethodPatch, "/api/project/"+pid+"/issues/"+url.PathEscape(blockID),
		map[string]any{"status": status})
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
		Issue issueBlock `json:"issue"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	fmt.Printf("%s → %s\n", shortID(payload.Issue.ID), sanitizeTerminal(payload.Issue.WorkflowStatus))
	return nil
}

// printCreatedIssue renders a create/comment response: raw JSON when piped, a
// one-line human confirmation on a TTY (id + sanitized title/status).
func printCreatedIssue(resp []byte, kind string) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Issue   *issueBlock `json:"issue"`
		Comment *issueBlock `json:"comment"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	b := payload.Issue
	if b == nil {
		b = payload.Comment
	}
	if b == nil {
		PrintJSON(resp)
		return nil
	}
	if b.WorkflowStatus != "" {
		fmt.Printf("created %s %s [%s] %s\n", kind, shortID(b.ID), sanitizeTerminal(b.WorkflowStatus), sanitizeTerminal(b.Title))
		return nil
	}
	fmt.Printf("created %s %s\n", kind, shortID(b.ID))
	return nil
}

// ── TTY rendering (all attacker-controlled text is sanitized, §5.4) ────────────.

// printIssueTable renders the list view. Titles are attacker-controlled ⇒
// sanitizeTerminal before printing.
func printIssueTable(rows []issueListRow) {
	if len(rows) == 0 {
		fmt.Println("No issues.")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATUS\tUPDATED\tTITLE")
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			shortID(r.ID), sanitizeTerminal(r.WorkflowStatus), shortDate(r.UpdatedAt), sanitizeTerminal(r.Title))
	}
	_ = w.Flush()
}

// printIssueDetail renders one issue + its comment thread. Title, body, tags and
// every comment (author + body) are attacker-controlled ⇒ sanitized.
func printIssueDetail(issue issueBlock, comments []issueBlock) {
	fmt.Printf("%s\n", sanitizeTerminal(issue.Title))
	fmt.Printf("  id:      %s\n", issue.ID)
	if issue.WorkflowStatus != "" {
		fmt.Printf("  status:  %s\n", sanitizeTerminal(issue.WorkflowStatus))
	}
	fmt.Printf("  scope:   %s\n", issue.Scope)
	if issue.UpdatedAt != "" {
		fmt.Printf("  updated: %s\n", shortDate(issue.UpdatedAt))
	}
	if len(issue.Tags) > 0 {
		safe := make([]string, len(issue.Tags))
		for i, t := range issue.Tags {
			safe[i] = sanitizeTerminal(t)
		}
		fmt.Printf("  labels:  %s\n", strings.Join(safe, ", "))
	}
	if issue.Content != "" {
		fmt.Printf("\n%s\n", sanitizeTerminal(issue.Content))
	}
	fmt.Printf("\nComments (%d):\n", len(comments))
	for _, c := range comments {
		author := "anon"
		if a, ok := c.Metadata["author"].(string); ok && a != "" {
			author = a
		}
		fmt.Printf("  [%s] %s\n", sanitizeTerminal(author), sanitizeTerminal(oneLine(c.Content)))
	}
}

// shortID trims a UUID to its first segment for compact tables (full id stays in
// the piped JSON). A non-UUID (or short) id is returned unchanged.
func shortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

// shortDate trims an RFC3339 timestamp to its date for compact tables.
func shortDate(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// oneLine collapses a body to its first line for the comment-thread summary.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
