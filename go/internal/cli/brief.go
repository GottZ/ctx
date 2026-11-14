package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// briefCategory is the dedicated store category for agent briefings.
const briefCategory = "agent-briefing"

const (
	// briefGlobalTag is the producer-set marker for project-neutral briefings.
	briefGlobalTag = "briefing:global"
	// briefMaxLimit is the REST search ceiling shared by both lookup paths.
	briefMaxLimit = 50
)

// briefBlock represents a single block from the search response.
type briefBlock struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// briefResponse is the search API response shape.
type briefResponse struct {
	Success bool         `json:"success"`
	Error   string       `json:"error"`
	Count   int          `json:"count"`
	Results []briefBlock `json:"results"`
}

// hookInput is the JSON passed on stdin by Claude Code hooks.
type hookInput struct {
	CWD       string `json:"cwd"`
	AgentType string `json:"agent_type"`
}

// hookOutput is the Claude Code SubagentStart hook JSON format.
type hookOutput struct {
	Continue           bool        `json:"continue"`
	SuppressOutput     bool        `json:"suppressOutput"`
	HookSpecificOutput hookPayload `json:"hookSpecificOutput"`
}

type hookPayload struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

func briefCmd(getClient func() (*Client, error)) *cobra.Command {
	var (
		hook    bool
		limit   int
		project string
	)

	cmd := &cobra.Command{
		Use:   "brief",
		Short: "Project briefing from agent-briefing blocks",
		Long: `Output a compact project briefing from agent-briefing blocks.
Scoped by project tag: the repository holding CWD (an ordinary checkout's
root; linked worktrees resolve to their main checkout, submodules to their
own checkout, bare and --separate-git-dir repositories to their git
directory — see docs/operations.md), else CWD. When no block carries the
project tag or the project lookup fails, falls back to blocks with the
producer-set briefing:global marker. Untagged blocks and blocks tagged
only for another project are never included.

Default: human-readable text. With --hook: reads CWD from stdin JSON,
outputs Claude Code SubagentStart hook JSON.

Setup (add to ~/.claude/settings.json):

  {
    "hooks": {
      "SubagentStart": [
        { "hooks": [ { "type": "command", "command": "ctx brief --hook" } ] }
      ]
    }
  }`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrief(getClient, hook, limit, project)
		},
	}
	cmd.Flags().BoolVar(&hook, "hook", false, "JSON output for Claude Code SubagentStart hooks (reads CWD from stdin)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum blocks to include")
	cmd.Flags().StringVar(&project, "project", "", "Project tag to filter by (default: the repository identity of CWD, else CWD)")
	return cmd
}

func runBrief(getClient func() (*Client, error), hook bool, limit int, project string) error {
	c, err := getClient()
	if err != nil {
		return nil //nolint:nilerr // silent degradation for hooks
	}

	// Resolve project tag.
	if project == "" && hook {
		project = projectFromStdin()
	}
	if project == "" {
		project = projectFromCWD()
	}

	// Project-scoped first. An empty result or a failed project request both
	// fall through to the producer-marked global briefing, never to an
	// unfiltered category read.
	var blocks []briefBlock
	if project != "" {
		blocks, _ = searchBriefBlocks(c, limit, project)
	}
	if len(blocks) == 0 {
		blocks, _ = searchBriefBlocks(c, limit, briefGlobalTag)
	}
	if len(blocks) == 0 {
		return nil
	}

	if hook {
		fmt.Print(formatBriefHook(blocks))
	} else {
		fmt.Print(formatBriefHuman(blocks))
	}
	return nil
}

// searchBriefBlocks queries agent-briefing blocks carrying tag. A legitimate
// empty result is distinct from a transport, decoding, or API error.
func searchBriefBlocks(c *Client, limit int, tag string) ([]briefBlock, error) {
	data, err := searchBriefPage(c, briefSearchLimit(limit), tag)
	if err != nil {
		return nil, err
	}
	return data.Results, nil
}

// briefSearchLimit applies the same default and REST ceiling to both paths.
func briefSearchLimit(limit int) int {
	if limit <= 0 {
		return 10 // the search API's own default (handler.defaultSearchLimit)
	}
	if limit > briefMaxLimit {
		return briefMaxLimit
	}
	return limit
}

// searchBriefPage posts one empty-query search over the agent-briefing
// category with the existing server-side scope-aware tag filter.
func searchBriefPage(c *Client, limit int, tag string) (*briefResponse, error) {
	body := map[string]any{
		"category": briefCategory,
		"tags":     []string{tag},
		"limit":    limit,
		"compact":  false,
	}

	resp, err := c.Post("search", body)
	if err != nil {
		return nil, err
	}

	var data briefResponse
	if err := json.Unmarshal(resp, &data); err != nil {
		return nil, fmt.Errorf("decode briefing search: %w", err)
	}
	if !data.Success {
		if data.Error != "" {
			return nil, fmt.Errorf("briefing search: %s", data.Error)
		}
		return nil, fmt.Errorf("briefing search failed")
	}
	return &data, nil
}

// projectFromStdin reads hook JSON from stdin and extracts CWD, then resolves its project identity.
func projectFromStdin() string {
	if stdin, ok := ReadStdin(); ok {
		var input hookInput
		if json.Unmarshal([]byte(stdin), &input) == nil && input.CWD != "" {
			return resolveProject(input.CWD)
		}
	}
	return ""
}

// projectFromCWD resolves the project tag from the current working directory.
func projectFromCWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return resolveProject(cwd)
}

// resolveProject maps dir to the project identity that tags agent-briefing
// blocks (ctx brief) and the blocks ctx persist writes. It walks up from dir to
// the nearest .git entry:
//
//   - a .git directory: the directory holding it, byte for byte the identity
//     this resolver has always produced for an ordinary checkout;
//   - a .git file (linked worktree, submodule, --separate-git-dir checkout):
//     the identity of the repository's common git directory
//     (projectFromCommonDir), so every worktree of one repository resolves to
//     the same tag;
//   - a .git file that cannot be followed to a git directory (including one
//     that is no regular file, or larger than gitMetaMaxBytes) is skipped and
//     the walk goes on, as it always did.
//
// Falls back to dir itself if no .git entry resolves. Git metadata is read
// from files, never through a git subprocess (the hook runs on every
// SubagentStart, and exec call sites are an argued exception in this module).
func resolveProject(dir string) string {
	d := dir
	for {
		gitPath := filepath.Join(d, ".git")
		if info, err := os.Stat(gitPath); err == nil {
			if info.IsDir() {
				return d
			}
			if id, ok := projectFromGitFile(gitPath); ok {
				return walkedSpelling(d, id)
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return dir
}

// gitMetaMaxBytes bounds every git metadata file the resolver reads: the .git
// file, commondir, config and config.worktree. Real ones hold a few hundred
// bytes; the bound keeps a broken or planted entry from making a hook that
// runs on every SubagentStart read without end.
const gitMetaMaxBytes = 1 << 20

// errGitMetaRejected marks a metadata path that exists but is no regular file
// of at most gitMetaMaxBytes.
var errGitMetaRejected = errors.New("git metadata: not a regular file within the size bound")

// readGitMeta reads a git metadata file only when it is a regular file of at
// most gitMetaMaxBytes. Stat comes first, because opening a FIFO already
// blocks. A missing file reports an error matching fs.ErrNotExist.
func readGitMeta(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > gitMetaMaxBytes {
		return nil, errGitMetaRejected
	}
	f, err := os.Open(path) //nolint:gosec // G304/G703: repository-local git metadata reached through the user's own .git entry, read with a size bound
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, gitMetaMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > gitMetaMaxBytes {
		return nil, errGitMetaRejected
	}
	return b, nil
}

// projectFromGitFile follows a .git file to the repository's common git
// directory and names the project from that directory alone. ok is false when
// the file is not a "gitdir: <path>" pointer to a git directory.
func projectFromGitFile(gitFile string) (string, bool) {
	gitDir, ok := gitDirFromFile(gitFile)
	if !ok {
		return "", false
	}
	common := gitDir
	c, err := readGitMeta(filepath.Join(gitDir, "commondir"))
	switch {
	case err == nil:
		if common, ok = resolveGitPath(gitDir, strings.TrimRight(string(c), "\r\n")); !ok {
			return "", false
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", false
	}
	if info, err := os.Stat(common); err != nil || !info.IsDir() { //nolint:gosec // G703: the common directory named by the followed gitdir, only stat'ed
		return "", false
	}
	return projectFromCommonDir(common), true
}

// gitDirFromFile reads a .git file the way git does: the content must start
// with exactly "gitdir: ", only trailing CR/LF is dropped (a path may end in
// a blank), and the path is resolved by resolveGitPath from the file's
// directory. The target must hold a HEAD file — part of git's own check
// before it follows a .git file.
func gitDirFromFile(gitFile string) (string, bool) {
	b, err := readGitMeta(gitFile)
	if err != nil {
		return "", false
	}
	rel, ok := strings.CutPrefix(strings.TrimRight(string(b), "\r\n"), "gitdir: ")
	if !ok || rel == "" {
		return "", false
	}
	gitDir, ok := resolveGitPath(filepath.Dir(gitFile), rel)
	if !ok {
		return "", false
	}
	if info, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil || info.IsDir() { //nolint:gosec // G703: repository-local git metadata reached through the user's own .git file, only stat'ed
		return "", false
	}
	return gitDir, true
}

// resolveGitPath resolves a path value from git metadata (gitdir, commondir,
// core.worktree) the way git does: a relative value is appended to base
// without a lexical clean, and the file system resolves the whole path, so a
// symlink before ".." leads where the link points. The result is the physical
// path; ok is false when the path does not exist.
func resolveGitPath(base, p string) (string, bool) {
	if !filepath.IsAbs(p) {
		p = base + string(filepath.Separator) + p
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// walkedSpelling returns the directory on the walk from d upward that is the
// same directory as id, spelled the way the walk reached it; otherwise id.
// git records resolved paths in gitdir files, so without this a nested linked
// worktree reached through a symlink would lose the exact tag its enclosing
// main checkout resolves to through the .git-directory rule.
func walkedSpelling(d, id string) string {
	target, err := os.Stat(id)
	if err != nil {
		return id
	}
	for a := d; ; {
		if info, err := os.Stat(a); err == nil && os.SameFile(info, target) {
			return a
		}
		parent := filepath.Dir(a)
		if parent == a {
			return id
		}
		a = parent
	}
}

// projectFromCommonDir names a repository by its common git directory, so the
// main checkout and every linked worktree agree:
//
//  1. named .git: its parent, the main checkout of an ordinary repository and
//     exactly what the .git-directory rule yields inside that checkout;
//  2. core.worktree set: that checkout, resolved from the git directory like
//     git does (a submodule records its checkout there); a checkout that does
//     not exist, where git itself would stop, keeps the lexical form;
//  3. otherwise the git directory itself: a bare repository has no checkout,
//     and a --separate-git-dir repository records none. Its parent is never
//     used, because sibling repositories share it.
func projectFromCommonDir(common string) string {
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	if wt := gitCoreWorktree(common); wt != "" {
		if resolved, ok := resolveGitPath(common, wt); ok {
			return resolved
		}
		if !filepath.IsAbs(wt) {
			wt = filepath.Join(common, wt)
		}
		return filepath.Clean(wt)
	}
	return common
}

// gitCoreWorktree reads core.worktree of the main worktree from a common git
// directory: config, overridden by config.worktree when
// extensions.worktreeConfig is on (git sparse-checkout moves core.worktree
// there). Include directives are not followed.
func gitCoreWorktree(common string) string {
	cfg := readGitConfig(filepath.Join(common, "config"))
	wt := cfg["core.worktree"]
	if gitConfigBool(cfg["extensions.worktreeconfig"]) {
		if v, ok := readGitConfig(filepath.Join(common, "config.worktree"))["core.worktree"]; ok {
			wt = v
		}
	}
	return wt
}

// readGitConfig reads the plain "section.key" entries of a git config file
// (section and key lowercased, last value wins; a key without "=" is true).
// Subsection entries keep their quoted name in the section part and so never
// collide with plain sections. A missing, unreadable or rejected file (see
// readGitMeta) reads as empty.
func readGitConfig(path string) map[string]string {
	b, err := readGitMeta(path)
	if err != nil {
		return nil
	}
	vals := map[string]string{}
	section := ""
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				section = ""
				continue
			}
			section = strings.ToLower(strings.TrimSpace(line[1:end]))
			line = strings.TrimSpace(line[end+1:])
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, value, hasValue := strings.Cut(line, "=")
		if hasValue {
			value = gitConfigValue(value)
		} else {
			value = "true"
		}
		vals[section+"."+strings.ToLower(strings.TrimSpace(key))] = value
	}
	return vals
}

// gitConfigValue decodes a config value the way git's config parser does:
// unquoted blanks before the value and after its last other character are
// dropped, blanks inside double quotes are kept, the quotes are removed,
// backslash escapes are decoded, and an unquoted '#' or ';' starts a comment.
func gitConfigValue(v string) string {
	var sb strings.Builder
	inQuote := false
	trimAt := -1 // value length before the current run of unquoted blanks
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !inQuote && (c == ' ' || c == '\t' || c == '\r') {
			if sb.Len() == 0 {
				continue
			}
			if trimAt < 0 {
				trimAt = sb.Len()
			}
			sb.WriteByte(c)
			continue
		}
		if !inQuote && (c == '#' || c == ';') {
			break
		}
		trimAt = -1
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == '\\' && i+1 < len(v):
			i++
			sb.WriteByte(gitConfigEscape(v[i]))
		default:
			sb.WriteByte(c)
		}
	}
	if trimAt >= 0 {
		return sb.String()[:trimAt]
	}
	return sb.String()
}

// gitConfigEscape decodes the byte after a backslash in a config value.
func gitConfigEscape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'b':
		return '\b'
	}
	return c
}

// gitConfigBool reports a git boolean as true: true/yes/on or a non-zero integer.
func gitConfigBool(v string) bool {
	switch strings.ToLower(v) {
	case "true", "yes", "on":
		return true
	}
	n, err := strconv.Atoi(v)
	return err == nil && n != 0
}

// formatBriefHuman renders blocks as human-readable text.
func formatBriefHuman(blocks []briefBlock) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "=== Project Briefing (%d blocks) ===\n\n", len(blocks))
	for _, b := range blocks {
		fmt.Fprintf(&sb, "--- %s ---\n%s\n\n", b.Title, strings.TrimSpace(b.Content))
	}
	return sb.String()
}

// formatBriefHook renders blocks as Claude Code SubagentStart hook JSON.
func formatBriefHook(blocks []briefBlock) string {
	var sb strings.Builder
	sb.WriteString("## Project Briefing\n\n")
	for _, b := range blocks {
		fmt.Fprintf(&sb, "### %s\n%s\n\n", b.Title, strings.TrimSpace(b.Content))
	}

	out := hookOutput{
		Continue:       true,
		SuppressOutput: false,
		HookSpecificOutput: hookPayload{
			HookEventName:     "SubagentStart",
			AdditionalContext: sb.String(),
		},
	}
	data, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(data)
}
