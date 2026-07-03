package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// RegisterCommands adds all subcommands to the root command.
func RegisterCommands(root *cobra.Command) {
	cfg, cfgErr := LoadConfig()
	// Lazy client — only created if a command actually runs.
	var client *Client
	getClient := func() (*Client, error) {
		if cfgErr != nil {
			return nil, cfgErr
		}
		if client == nil {
			client = NewClient(cfg)
		}
		return client, nil
	}

	root.AddCommand(queryCmd(getClient))
	root.AddCommand(saveCmd(getClient))
	root.AddCommand(searchCmd(getClient))
	root.AddCommand(statsCmd(getClient))
	root.AddCommand(categoriesCmd(getClient))
	root.AddCommand(getCmd(getClient))
	root.AddCommand(deleteCmd(getClient))
	root.AddCommand(listMetaCmd(getClient))
	root.AddCommand(digestCmd(getClient))
	root.AddCommand(guardCmd(getClient))
	root.AddCommand(manageCmd(getClient))
	root.AddCommand(healthCmd(getClient))
	root.AddCommand(IngestCmd(getClient))
	root.AddCommand(statuslineCmd(getClient))
	root.AddCommand(dreamCmd(getClient))
	root.AddCommand(mcpCmd(getClient))
	root.AddCommand(keysCmd(getClient))
	root.AddCommand(briefCmd(getClient))
	root.AddCommand(persistCmd(getClient))
	root.AddCommand(settingsCmd(getClient))
	root.AddCommand(secretsCmd(getClient))
	root.AddCommand(typesCmd(getClient))
	root.AddCommand(backendsCmd(getClient))
	root.AddCommand(gamingCmd(getClient))
	root.AddCommand(quotaCmd(getClient))
	root.AddCommand(blocksCmd(getClient))
	root.AddCommand(blockGrantCmd(getClient))
	root.AddCommand(tenantCmd(getClient))
	root.AddCommand(initCmd())
}

// ── query ────────────────────────────────────────────────────────────.

func queryCmd(getClient func() (*Client, error)) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:     "query [question]",
		Aliases: []string{"q"},
		Short:   "Hybrid search + LLM synthesis",
		Long:    "Query the context store with hybrid semantic + fulltext search and LLM synthesis.",
		Example: `  ctx q What embedding model is used
  ctx query welche modelle werden verwendet
  echo "question" | ctx query
  ctx q --json What models are configured`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			query := strings.Join(args, " ")
			if query == "" {
				if stdin, ok := ReadStdin(); ok {
					query = stdin
				}
			}
			if query == "" {
				return fmt.Errorf("usage: ctx query <question>")
			}

			body := map[string]any{
				"query": query,
				"limit": 5,
			}
			resp, err := c.Post("query", body)
			if err != nil {
				return err
			}

			if jsonOutput {
				PrintJSON(resp)
				return nil
			}

			var result queryResult
			if err := json.Unmarshal(resp, &result); err != nil {
				PrintJSON(resp) // Fallback to raw JSON on parse error.
				return err
			}

			if !result.Success {
				return fmt.Errorf("query failed: %s", result.Error)
			}

			// Formatted output: answer + sources.
			fmt.Println(result.Answer)
			if len(result.Sources) > 0 {
				fmt.Printf("\n  Sources (%s):\n", result.Confidence)
				for i, s := range result.Sources {
					fmt.Printf("  [%d] %s (%s, %dd ago) id:%s\n", i+1, s.Title, s.Category, s.AgeDays, s.ID)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output raw JSON instead of formatted text")
	return cmd
}

type queryResult struct {
	Success    bool   `json:"success"`
	Error      string `json:"error"`
	Answer     string `json:"answer"`
	Confidence string `json:"confidence"`
	Sources    []struct {
		ID       string  `json:"id"`
		Title    string  `json:"title"`
		Category string  `json:"category"`
		Score    float64 `json:"score"`
		AgeDays  int     `json:"age_days"`
	} `json:"sources"`
}

// ── save ─────────────────────────────────────────────────────────────.

func saveCmd(getClient func() (*Client, error)) *cobra.Command {
	var (
		shared      bool
		tags        []string
		sensitivity string
	)

	cmd := &cobra.Command{
		Use:     "save [--shared] [--tag TAG]... [--sensitivity LEVEL] <category> <title> [content...]",
		Aliases: []string{"s"},
		Short:   "Upsert block (--shared = the default tenant's shared scope)",
		Long:    "Save a knowledge block to the context store. Upserts by category+title.\nContent can be inline args, piped via stdin, or separated with '-'.\n--sensitivity classifies for trust gating (credentials|personal|internal|public);\nomitted = settings default pool.default_block_sensitivity (fail-closed).",
		Example: `  ctx save infrastructure "My Title" Content goes here
  ctx save decisions "My Decision" - Also works with dash
  cat file.md | ctx save docs "My Doc"
  ctx save --shared reference "Shared Block" Shared across the default tenant
  ctx save --sensitivity internal learnings "Public-safe note" No secrets here
  ctx save --tag /compose/n8n agent-briefing "Briefing" Project context`,
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			if len(args) < 2 {
				return fmt.Errorf("usage: ctx save [--shared] <category> <title> [content...]")
			}

			category := args[0]
			rest := args[1:]

			var title, content string

			// Find optional "-" or "--" separator in args (backward compat)
			sepIdx := -1
			for i, a := range rest {
				if a == "-" || a == "--" {
					sepIdx = i
					break
				}
			}

			if sepIdx >= 0 {
				// Explicit separator: title before, content after
				title = strings.Join(rest[:sepIdx], " ")
				content = strings.Join(rest[sepIdx+1:], " ")
			} else if len(rest) >= 2 {
				// No separator: first arg is title, rest is content
				title = rest[0]
				content = strings.Join(rest[1:], " ")
			} else {
				// Single arg: title only, content from stdin
				title = rest[0]
				if stdin, ok := ReadStdin(); ok {
					content = stdin
				}
			}

			if category == "" || title == "" || content == "" {
				return fmt.Errorf("usage: ctx save [--shared] <category> <title> [content...]")
			}

			if tags == nil {
				tags = []string{}
			}
			body := map[string]any{
				"category": category,
				"title":    title,
				"content":  content,
				"tags":     tags,
				"metadata": map[string]string{"source": "claude-session"},
			}
			if shared {
				body["scope"] = "shared"
			}
			if sensitivity != "" {
				body["sensitivity"] = sensitivity
			}

			resp, err := c.Post("store", body)
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
	cmd.Flags().BoolVar(&shared, "shared", false, "Set scope to shared (the default tenant's shared layer, not cross-tenant)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Add tag (repeatable)")
	cmd.Flags().StringVar(&sensitivity, "sensitivity", "", "Trust-gating level: credentials|personal|internal|public (default: settings key)")
	return cmd
}

// ── search ───────────────────────────────────────────────────────────.

func searchCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "search [category] [query:text] [tags:a,b] [limit:N] [compact:false]",
		Aliases: []string{"browse", "b"},
		Short:   "Compact search (no LLM)",
		Long:    "Search the context store with compact results. Key-value args for filtering.",
		Example: `  ctx search learnings query:prompt
  ctx search decisions query:guard tags:security limit:5`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			body := map[string]any{
				"limit":   10,
				"compact": true,
			}

			for _, arg := range args {
				switch {
				case strings.HasPrefix(arg, "query:"):
					body["query"] = strings.TrimPrefix(arg, "query:")
				case strings.HasPrefix(arg, "tags:"):
					body["tags"] = strings.Split(strings.TrimPrefix(arg, "tags:"), ",")
				case strings.HasPrefix(arg, "limit:"):
					if n, err := strconv.Atoi(strings.TrimPrefix(arg, "limit:")); err == nil {
						body["limit"] = n
					}
				case strings.HasPrefix(arg, "compact:"):
					body["compact"] = strings.TrimPrefix(arg, "compact:") != "false"
				default:
					// Positional = category
					body["category"] = arg
				}
			}

			resp, err := c.Post("search", body)
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── stats ────────────────────────────────────────────────────────────.

func statsCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "stats",
		Aliases: []string{"st"},
		Short:   "DB statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{"action": "stats"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── categories ───────────────────────────────────────────────────────.

func categoriesCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "categories",
		Aliases: []string{"cats"},
		Short:   "List categories",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{"action": "list-categories"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── get ──────────────────────────────────────────────────────────────.

func getCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "get <block-id>",
		Aliases: []string{"g"},
		Short:   "Fetch full block",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "get",
				"id":     args[0],
			})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── delete ───────────────────────────────────────────────────────────.

func deleteCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "delete <block-id>",
		Aliases: []string{"del"},
		Short:   "Delete block",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "delete",
				"id":     args[0],
			})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── list-meta ────────────────────────────────────────────────────────.

func listMetaCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "list-meta",
		Aliases: []string{"lm"},
		Short:   "All blocks (no content)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{"action": "list-meta"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── digest ───────────────────────────────────────────────────────────.

func digestCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "digest",
		Aliases: []string{"d"},
		Short:   "Rebuild topic map",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("digest", map[string]any{"trigger": "manual"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── guard ────────────────────────────────────────────────────────────.

func guardCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "guard [list|stats|resolve]",
		Aliases: []string{"gd"},
		Short:   "Write Guard",
		Long:    "Manage the Write Guard: list flagged blocks, show stats, or resolve flags.",
	}

	cmd.AddCommand(guardListCmd(getClient))
	cmd.AddCommand(guardStatsCmd(getClient))
	cmd.AddCommand(guardResolveCmd(getClient))

	// Default to list if no subcommand given
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return guardListRun(getClient)
	}

	return cmd
}

func guardListCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List flagged blocks",
		RunE: func(cmd *cobra.Command, args []string) error {
			return guardListRun(getClient)
		},
	}
}

func guardListRun(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, err := c.Post("manage", map[string]any{"action": "guard-list"})
	if err != nil {
		return err
	}

	// Parse and format like the bash version
	var data map[string]any
	if err := json.Unmarshal(resp, &data); err != nil {
		PrintRaw(resp)
		return nil //nolint:nilerr // raw response already printed above
	}

	if success, _ := data["success"].(bool); !success {
		errMsg, _ := data["error"].(string)
		if errMsg == "" {
			errMsg = "unknown"
		}
		return fmt.Errorf("error: %s", errMsg)
	}

	blocks, _ := data["blocks"].([]any)
	if len(blocks) == 0 {
		fmt.Println("No flagged blocks.")
		return nil
	}

	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fmt.Printf("[%v] %v\n", b["guard_status"], b["title"])
		fmt.Printf("  id: %v  category: %v\n", b["id"], b["category"])
		if sim, ok := b["similarity"]; ok && sim != nil {
			matched := b["matched_title"]
			if matched == nil {
				matched = b["matched_id"]
			}
			if matched == nil {
				matched = "?"
			}
			fmt.Printf("  Similarity: %v  matched: %v\n", sim, matched)
		}
		fmt.Println()
	}

	count := data["count"]
	if count == nil {
		count = len(blocks)
	}
	fmt.Printf("%v flagged block(s).\n", count)
	return nil
}

func guardStatsCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "stats",
		Aliases: []string{"st"},
		Short:   "Guard status overview",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{"action": "guard-stats"})
			if err != nil {
				return err
			}

			var d map[string]any
			if err := json.Unmarshal(resp, &d); err != nil {
				PrintRaw(resp)
				return nil //nolint:nilerr // raw response already printed above
			}

			if success, _ := d["success"].(bool); !success {
				errMsg, _ := d["error"].(string)
				if errMsg == "" {
					errMsg = "unknown"
				}
				return fmt.Errorf("error: %s", errMsg)
			}

			fmtVal := func(key string) any {
				if v, ok := d[key]; ok && v != nil {
					return v
				}
				return "?"
			}

			fmt.Println("Write Guard Stats:")
			fmt.Printf("  Total blocks:     %v\n", fmtVal("total_blocks"))
			fmt.Printf("  Active:           %v\n", fmtVal("active"))
			fmt.Printf("  Clean:            %v\n", fmtVal("clean"))
			fmt.Printf("  Needs review:     %v\n", fmtVal("needs_review"))
			fmt.Printf("  Near duplicate:   %v\n", fmtVal("near_duplicate"))
			fmt.Printf("  Unchecked:        %v\n", fmtVal("unchecked"))
			fmt.Printf("  Archived (dup):   %v\n", fmtVal("archived_dups"))
			fmt.Printf("  Write log:        %v\n", fmtVal("write_log_entries"))
			fmt.Printf("  Pending:          %v\n", fmtVal("pending_count"))

			if gs, ok := d["dirty_since"]; ok && gs != nil {
				fmt.Printf("  Dirty since:      %v\n", gs)
			}
			if gl, ok := d["last_guard_at"]; ok && gl != nil {
				fmt.Printf("  Last guard run:   %v\n", gl)
			}
			return nil
		},
	}
}

func guardResolveCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "resolve <block-id> <archive|keep>",
		Aliases: []string{"r"},
		Short:   "Resolve a flagged block",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			blockID := args[0]
			resolution := args[1]

			if resolution != "archive" && resolution != "keep" {
				return fmt.Errorf("resolution must be 'archive' or 'keep'")
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			resp, err := c.Post("manage", map[string]any{
				"action": "guard-resolve",
				"id":     blockID,
				"data":   map[string]string{"resolution": resolution},
			})
			if err != nil {
				return err
			}

			var d map[string]any
			if err := json.Unmarshal(resp, &d); err != nil {
				PrintRaw(resp)
				return nil //nolint:nilerr // raw response already printed above
			}

			if success, _ := d["success"].(bool); success {
				resolved, _ := d["resolved"].(map[string]any)
				if resolved != nil {
					id := fmt.Sprintf("%v", resolved["id"])
					if len(id) > 12 {
						id = id[:12] + "..."
					}
					fmt.Printf("%v: %v (%s)\n", resolved["guard_status"], resolved["title"], id)
				}
			} else {
				errMsg, _ := d["error"].(string)
				if errMsg == "" {
					errMsg = "unknown"
				}
				return fmt.Errorf("error: %s", errMsg)
			}
			return nil
		},
	}
}

// ── manage ───────────────────────────────────────────────────────────.

func manageCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "manage <action> [id] [data-json]",
		Aliases: []string{"m"},
		Short:   "Raw manage endpoint",
		Long:    "Direct access to the context-manage endpoint with arbitrary action/id/data.",
		Example: `  ctx manage update <id> '{"tags":["a","b"]}'
  ctx manage stats`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			body := map[string]any{
				"action": args[0],
			}

			if len(args) >= 2 {
				body["id"] = args[1]
			}

			if len(args) >= 3 {
				// Parse data as JSON
				var data any
				if err := json.Unmarshal([]byte(args[2]), &data); err != nil {
					// If not valid JSON, use as string
					body["data"] = args[2]
				} else {
					body["data"] = data
				}
			}

			resp, err := c.Post("manage", body)
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── health ───────────────────────────────────────────────────────────.

func healthCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Healthcheck (DB + Ollama connectivity)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			baseURL := strings.TrimSuffix(c.BaseURL, "/")
			resp, err := c.Get(baseURL + "/health")
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

func dreamCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "dream [stats|review]",
		Aliases: []string{"dr"},
		Short:   "Dream Mode — cross-reference engine",
		Long:    "View Dream Mode statistics or review links for quality assessment.",
	}

	cmd.AddCommand(dreamStatsCmd(getClient))
	cmd.AddCommand(dreamReviewCmd(getClient))
	cmd.AddCommand(dreamEnableCmd(getClient))
	cmd.AddCommand(dreamDisableCmd(getClient))
	cmd.AddCommand(dreamThrottleCmd(getClient))

	// Default to stats if no subcommand given.
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return dreamStatsRun(getClient)
	}

	return cmd
}

func dreamStatsCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "stats",
		Aliases: []string{"st"},
		Short:   "Show Dream Mode statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			return dreamStatsRun(getClient)
		},
	}
}

func dreamStatsRun(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	statsRaw, err := c.Post("manage", map[string]any{"action": "dream-stats"})
	if err != nil {
		return err
	}
	var merged map[string]any
	if err := json.Unmarshal(statsRaw, &merged); err != nil {
		PrintJSON(statsRaw)
		return err
	}
	modeRaw, err := c.Post("manage", map[string]any{"action": "dream-mode"})
	if err == nil {
		var modeMap map[string]any
		if err := json.Unmarshal(modeRaw, &modeMap); err == nil {
			merged["dream_mode"] = modeMap["mode"]
			merged["dream_interval"] = modeMap["interval"]
		}
	}
	// Human-readable summary on an interactive terminal; machine-readable JSON
	// when piped/redirected so scripts and `| jq` keep working unchanged.
	if StdoutIsTTY() {
		fmt.Print(renderDreamStatsHuman(merged))
		return nil
	}
	out, _ := json.MarshalIndent(merged, "", "  ")
	fmt.Println(string(out))
	return nil
}

func dreamReviewCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "review",
		Aliases: []string{"rv"},
		Short:   "Review Dream links (low confidence, supersedes, recent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{"action": "dream-review"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

func dreamEnableCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "enable",
		Aliases: []string{"on"},
		Short:   "Enable Dream Mode (full throttle)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "dream-mode",
				"data":   map[string]any{"mode": "on"},
			})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

func dreamDisableCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "disable",
		Aliases: []string{"off"},
		Short:   "Disable Dream Mode (maintenance/dev)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "dream-mode",
				"data":   map[string]any{"mode": "off"},
			})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

func dreamThrottleCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "throttle [duration]",
		Aliases: []string{"tr"},
		Short:   "Throttled mode — GPU cooldown between LLM calls (default 20s)",
		Long:    "Sets Dream to throttled mode with optional interval. Examples: ctx dream throttle, ctx dream throttle 60s",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data := map[string]any{"mode": "throttled"}
			if len(args) == 1 {
				d, err := parseDuration(args[0])
				if err != nil {
					return err
				}
				data["interval"] = int(d.Seconds())
			}
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "dream-mode",
				"data":   data,
			})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── mcp ──────────────────────────────────────────────────────────────.

func mcpCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP OAuth clients",
		Long:  "Create, list, and delete OAuth client registrations for MCP remote access.",
	}

	cmd.AddCommand(mcpAddCmd(getClient))
	cmd.AddCommand(mcpListCmd(getClient))
	cmd.AddCommand(mcpDeleteCmd(getClient))

	// Default to list.
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return mcpListRun(getClient)
	}

	return cmd
}

func mcpAddCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "add <label>",
		Short: "Register a new MCP OAuth client",
		Example: `  ctx mcp add "Claude AI"
  ctx mcp add "Claude Code Desktop"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := strings.Join(args, " ")
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "mcp-client-create",
				"data":   map[string]any{"label": label},
			})
			if err != nil {
				return err
			}
			var result struct {
				Success      bool   `json:"success"`
				ClientID     string `json:"client_id"`
				ClientSecret string `json:"client_secret"`
				Label        string `json:"label"`
				Error        string `json:"error"`
			}
			if err := json.Unmarshal(resp, &result); err != nil {
				PrintJSON(resp)
				return err
			}
			if !result.Success {
				return fmt.Errorf("failed: %s", result.Error)
			}

			fmt.Printf("MCP Client registered: %s\n\n", result.Label)
			fmt.Printf("  client_id:     %s\n", result.ClientID)
			fmt.Printf("  client_secret: %s\n", result.ClientSecret)
			fmt.Printf("\n  Save the secret now — it cannot be retrieved later.\n")
			return nil
		},
	}
}

func mcpListCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List registered MCP OAuth clients",
		RunE: func(cmd *cobra.Command, args []string) error {
			return mcpListRun(getClient)
		},
	}
}

func mcpListRun(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, err := c.Post("manage", map[string]any{"action": "mcp-client-list"})
	if err != nil {
		return err
	}
	var result struct {
		Success bool `json:"success"`
		Clients []struct {
			ClientID  string `json:"client_id"`
			Label     string `json:"label"`
			Active    bool   `json:"active"`
			CreatedAt string `json:"created_at"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		PrintJSON(resp)
		return err
	}
	if len(result.Clients) == 0 {
		fmt.Println("No MCP clients registered. Use: ctx mcp add <label>")
		return nil
	}
	for _, cl := range result.Clients {
		status := "active"
		if !cl.Active {
			status = "revoked"
		}
		fmt.Printf("  %s  %s  [%s]  %s\n", cl.ClientID, cl.Label, status, cl.CreatedAt[:10])
	}
	return nil
}

func mcpDeleteCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <client_id>",
		Short: "Revoke an MCP OAuth client",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "mcp-client-delete",
				"data":   map[string]any{"client_id": args[0]},
			})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// parseDuration parses a duration string. Bare integers are treated as seconds.
func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: use e.g. 20, 30s, 1m", s)
	}
	return d, nil
}

// ── keys ─────────────────────────────────────────────────────────────.
//
// `ctx keys create <label> --home <scope>` provisions a new API key.
// home_scope is required (v2.0.0 breaking change) — no default fallback.

func keysCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage API keys (create, list, delete)",
		Long:  "Provision and revoke X-Context-Key API keys. home_scope is required at creation time (v2.0.0).",
	}
	cmd.AddCommand(keysCreateCmd(getClient))
	cmd.AddCommand(keysListCmd(getClient))
	cmd.AddCommand(keysDeleteCmd(getClient))

	// Default to list.
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return keysListRun(getClient)
	}
	return cmd
}

func keysCreateCmd(getClient func() (*Client, error)) *cobra.Command {
	var (
		homeScope     string
		allowedScopes []string
	)
	cmd := &cobra.Command{
		Use:   "create <label>",
		Short: "Provision a new API key (home_scope is required)",
		Example: `  ctx keys create bench-crag --home crag
  ctx keys create work-key --home work --allowed shared,work
  ctx keys create temp --home private --allowed shared`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := strings.Join(args, " ")
			if homeScope == "" {
				return fmt.Errorf("--home is required (e.g. --home crag)")
			}
			c, err := getClient()
			if err != nil {
				return err
			}
			data := map[string]any{
				"label":      label,
				"home_scope": homeScope,
			}
			if len(allowedScopes) > 0 {
				data["allowed_scopes"] = allowedScopes
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "api-key-create",
				"data":   data,
			})
			if err != nil {
				return err
			}
			var result struct {
				Success       bool     `json:"success"`
				ID            string   `json:"id"`
				Label         string   `json:"label"`
				HomeScope     string   `json:"home_scope"`
				AllowedScopes []string `json:"allowed_scopes"`
				ApiKey        string   `json:"api_key"`
				Error         string   `json:"error"`
			}
			if err := json.Unmarshal(resp, &result); err != nil {
				PrintJSON(resp)
				return err
			}
			if !result.Success {
				return fmt.Errorf("failed: %s", result.Error)
			}
			fmt.Printf("API Key created: %s\n\n", result.Label)
			fmt.Printf("  id:             %s\n", result.ID)
			fmt.Printf("  home_scope:     %s\n", result.HomeScope)
			fmt.Printf("  allowed_scopes: %v\n", result.AllowedScopes)
			fmt.Printf("  api_key:        %s\n", result.ApiKey)
			fmt.Printf("\n  Save the key now — it cannot be retrieved later.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&homeScope, "home", "", "home_scope for the new key (REQUIRED, e.g. 'private', 'work', 'crag')")
	cmd.Flags().StringSliceVar(&allowedScopes, "allowed", nil, "additional allowed read scopes (comma-separated, default: shared)")
	return cmd
}

func keysListCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List provisioned API keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			return keysListRun(getClient)
		},
	}
}

func keysListRun(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, err := c.Post("manage", map[string]any{"action": "api-key-list"})
	if err != nil {
		return err
	}
	var result struct {
		Success bool `json:"success"`
		Keys    []struct {
			ID            string   `json:"id"`
			Label         string   `json:"label"`
			HomeScope     string   `json:"home_scope"`
			AllowedScopes []string `json:"allowed_scopes"`
			Active        bool     `json:"active"`
			CreatedAt     string   `json:"created_at"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		PrintJSON(resp)
		return err
	}
	if len(result.Keys) == 0 {
		fmt.Println("No API keys provisioned. Use: ctx keys create <label> --home <scope>")
		return nil
	}
	for _, k := range result.Keys {
		status := "active"
		if !k.Active {
			status = "revoked"
		}
		created := k.CreatedAt
		if len(created) >= 10 {
			created = created[:10]
		}
		fmt.Printf("  %s  %s  home=%s  [%s]  %s\n", k.ID, k.Label, k.HomeScope, status, created)
	}
	return nil
}

func keysDeleteCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Revoke an API key (soft delete; sets active=false)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "api-key-delete",
				"data":   map[string]any{"id": args[0]},
			})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}
