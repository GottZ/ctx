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
Scoped by project tag (git root or CWD). When no block carries the
project tag or the project lookup fails, falls back to blocks with the
producer-set briefing:global marker. Untagged blocks and blocks tagged
only for another project are never included. Broader .git-file worktree
resolution is tracked in #146.

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
