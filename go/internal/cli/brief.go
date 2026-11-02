package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// briefCategory is the dedicated store category for agent briefings.
const briefCategory = "agent-briefing"

// Paging bounds of the untagged fallback. /api/search has no "no tags"
// filter, so the fallback browses the category and keeps the untagged blocks
// on the client; a page holds at most briefPageSize rows (the REST cap) and it
// reads at most briefMaxPages pages, so a store full of other projects'
// briefings costs a hook a bounded number of requests.
const (
	briefPageSize = 50
	briefMaxPages = 10
)

// briefBlock represents a single block from the search response.
type briefBlock struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
}

// briefResponse is the search API response shape. NextAfter is the keyset
// cursor of the next page (JSON null on the last page), passed back verbatim
// as "after".
type briefResponse struct {
	Success   bool            `json:"success"`
	Count     int             `json:"count"`
	Results   []briefBlock    `json:"results"`
	NextAfter json.RawMessage `json:"next_after"`
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
Scoped by project tag (git root or CWD). When no block carries the
project tag, falls back to the blocks without any tag; blocks tagged for
another project are never included.

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
	cmd.Flags().StringVar(&project, "project", "", "Project tag to filter by (default: git root or CWD)")
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

	// Project-scoped first; without project blocks (or without a project),
	// the untagged blocks only. Never the blocks tagged for another project.
	var blocks []briefBlock
	if project != "" {
		blocks = searchBriefBlocks(c, limit, project)
	}
	if len(blocks) == 0 {
		blocks = searchUntaggedBriefBlocks(c, limit)
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

// searchBriefBlocks queries the agent-briefing blocks tagged with project.
func searchBriefBlocks(c *Client, limit int, project string) []briefBlock {
	data := searchBriefPage(c, limit, []string{project}, nil)
	if data == nil {
		return nil
	}
	return data.Results
}

// searchUntaggedBriefBlocks returns up to limit agent-briefing blocks that
// carry no tag at all, most recently updated first. The filter runs here because the search
// API cannot ask for "no tags", so it pages through the category until it has
// limit blocks, the store runs out, or briefMaxPages pages were read.
func searchUntaggedBriefBlocks(c *Client, limit int) []briefBlock {
	if limit <= 0 {
		limit = 10 // the search API's own default (handler.defaultSearchLimit)
	}
	var out []briefBlock
	var after json.RawMessage
	for range briefMaxPages {
		data := searchBriefPage(c, briefPageSize, nil, after)
		if data == nil {
			break
		}
		for _, b := range data.Results {
			if len(b.Tags) > 0 {
				continue
			}
			out = append(out, b)
			if len(out) == limit {
				return out
			}
		}
		if len(data.NextAfter) == 0 || string(data.NextAfter) == "null" {
			break
		}
		after = data.NextAfter
	}
	return out
}

// searchBriefPage posts one empty-query search over the agent-briefing
// category. tags (if any) filter server-side; after is the cursor of the
// previous page (nil = first page). nil = error or no results.
func searchBriefPage(c *Client, limit int, tags []string, after json.RawMessage) *briefResponse {
	body := map[string]any{
		"category": briefCategory,
		"limit":    limit,
		"compact":  false,
	}
	if len(tags) > 0 {
		body["tags"] = tags
	}
	if after != nil {
		body["after"] = after
	}

	resp, err := c.Post("search", body)
	if err != nil {
		return nil
	}

	var data briefResponse
	if err := json.Unmarshal(resp, &data); err != nil || !data.Success || data.Count == 0 {
		return nil
	}
	return &data
}

// projectFromStdin reads hook JSON from stdin and extracts CWD, then resolves to git root.
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

// resolveProject walks up from dir to find a .git directory (git root = project identity).
// Falls back to dir itself if no .git found.
func resolveProject(dir string) string {
	d := dir
	for {
		if info, err := os.Stat(filepath.Join(d, ".git")); err == nil && info.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return dir
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
