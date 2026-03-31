package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// persistMarkerRe matches [PERSIST:category:title] markers.
var persistMarkerRe = regexp.MustCompile(`(?m)^\[PERSIST:([^:\]]+):([^\]]+)\]\s*$`)

// persistHookInput is the JSON passed on stdin by Claude Code SubagentStop hooks.
type persistHookInput struct {
	CWD                  string `json:"cwd"`
	SessionID            string `json:"session_id"`
	AgentType            string `json:"agent_type"`
	AgentID              string `json:"agent_id"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// persistBlock is a parsed [PERSIST:category:title] block from agent output.
type persistBlock struct {
	Category string
	Title    string
	Content  string
}

func persistCmd(getClient func() (*Client, error)) *cobra.Command {
	var hook bool

	cmd := &cobra.Command{
		Use:   "persist",
		Short: "Persist agent findings from [PERSIST:cat:title] markers",
		Long: `Parse [PERSIST:category:title] markers from agent output and save to the context store.

With --hook: reads SubagentStop JSON from stdin (last_assistant_message + session metadata).
Without --hook: reads plain text from stdin and parses markers.

Markers in agent output:
  [PERSIST:learnings:My Finding]
  Content goes here, multi-line.

  [PERSIST:decisions:Another Block]
  More content.

Metadata (session_id, hostname, project, agent) is stored automatically.
Fails silently if no markers found or ctx unreachable.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPersist(getClient, hook)
		},
	}
	cmd.Flags().BoolVar(&hook, "hook", false, "Read SubagentStop JSON from stdin")
	return cmd
}

func runPersist(getClient func() (*Client, error), hook bool) error {
	c, err := getClient()
	if err != nil {
		return nil //nolint:nilerr // silent degradation
	}

	var message string
	var meta persistMetadata

	if hook {
		input := readPersistHookInput()
		if input == nil {
			return nil
		}
		message = input.LastAssistantMessage
		meta = buildMetadata(input)
	} else {
		if stdin, ok := ReadStdin(); ok {
			message = stdin
		}
		meta = buildMetadataFromEnv()
	}

	if message == "" {
		return nil
	}

	blocks := parseMarkers(message)
	if len(blocks) == 0 {
		return nil
	}

	project := resolveProject(meta.Project)
	count := 0
	for _, b := range blocks {
		body := map[string]any{
			"category": b.Category,
			"title":    b.Title,
			"content":  b.Content,
			"tags":     []string{project},
			"metadata": map[string]string{
				"source":     "agent-persist",
				"session_id": meta.SessionID,
				"agent_type": meta.AgentType,
				"agent_id":   meta.AgentID,
				"hostname":   meta.Hostname,
				"project":    project,
			},
		}
		if _, err := c.Post("store", body); err == nil {
			count++
		}
	}

	if count > 0 {
		fmt.Fprintf(os.Stderr, "ctx persist: %d block(s) saved\n", count)
	}
	return nil
}

// persistMetadata holds session and machine context for cross-referencing.
type persistMetadata struct {
	SessionID string
	AgentType string
	AgentID   string
	Hostname  string
	Project   string
}

func readPersistHookInput() *persistHookInput {
	stdin, ok := ReadStdin()
	if !ok || stdin == "" {
		return nil
	}
	var input persistHookInput
	if err := json.Unmarshal([]byte(stdin), &input); err != nil {
		return nil
	}
	if input.LastAssistantMessage == "" {
		return nil
	}
	return &input
}

func buildMetadata(input *persistHookInput) persistMetadata {
	hostname, _ := os.Hostname()
	project := input.CWD
	if project == "" {
		project, _ = os.Getwd()
	}
	return persistMetadata{
		SessionID: input.SessionID,
		AgentType: input.AgentType,
		AgentID:   input.AgentID,
		Hostname:  hostname,
		Project:   project,
	}
}

func buildMetadataFromEnv() persistMetadata {
	hostname, _ := os.Hostname()
	cwd, _ := os.Getwd()
	return persistMetadata{
		Hostname: hostname,
		Project:  cwd,
	}
}

// parseMarkers extracts [PERSIST:category:title] blocks from text.
func parseMarkers(text string) []persistBlock {
	matches := persistMarkerRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}

	var blocks []persistBlock
	for i, m := range matches {
		category := text[m[2]:m[3]]
		title := text[m[4]:m[5]]

		// Content: from end of marker line to start of next marker (or end of text).
		contentStart := m[1]
		var contentEnd int
		if i+1 < len(matches) {
			contentEnd = matches[i+1][0]
		} else {
			contentEnd = len(text)
		}

		content := strings.TrimSpace(text[contentStart:contentEnd])
		if content == "" {
			continue
		}

		blocks = append(blocks, persistBlock{
			Category: category,
			Title:    title,
			Content:  content,
		})
	}
	return blocks
}
