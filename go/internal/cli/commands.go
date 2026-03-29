package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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
}

// ── query ────────────────────────────────────────────────────────────

func queryCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "query [question]",
		Aliases: []string{"q"},
		Short:   "Hybrid search + LLM synthesis",
		Long:    "Query the context store with hybrid semantic + fulltext search and LLM synthesis.",
		Example: `  ctx query "What embedding model is used?"
  echo "question" | ctx query`,
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
			resp, err := c.Post("context-agent", body)
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── save ─────────────────────────────────────────────────────────────

func saveCmd(getClient func() (*Client, error)) *cobra.Command {
	var shared bool

	cmd := &cobra.Command{
		Use:     "save [--shared] <category> <title> - <content>",
		Aliases: []string{"s"},
		Short:   "Upsert block (--shared = cross-tenant)",
		Long:    "Save a knowledge block to the context store. Upserts by category+title.",
		Example: `  ctx save infrastructure "My Title" - "Content here"
  cat file.md | ctx save docs "My Doc"
  ctx save --shared reference "Shared Block" - "Visible to all tenants"`,
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			if len(args) < 2 {
				return fmt.Errorf("usage: ctx save [--shared] <category> <title> - <content>")
			}

			category := args[0]
			rest := args[1:]

			var title, content string

			// Find the " - " separator in args
			sepIdx := -1
			for i, a := range rest {
				if a == "-" {
					sepIdx = i
					break
				}
			}

			if sepIdx >= 0 {
				// title is everything before separator, content everything after
				title = strings.Join(rest[:sepIdx], " ")
				content = strings.Join(rest[sepIdx+1:], " ")
			} else {
				// No separator — title is all remaining args, content from stdin
				title = strings.Join(rest, " ")
				if stdin, ok := ReadStdin(); ok {
					content = stdin
				}
			}

			if category == "" || title == "" || content == "" {
				return fmt.Errorf("usage: ctx save [--shared] <category> <title> - <content>")
			}

			body := map[string]any{
				"category": category,
				"title":    title,
				"content":  content,
				"tags":     []string{},
				"metadata": map[string]string{"source": "claude-session"},
			}
			if shared {
				body["scope"] = "shared"
			}

			resp, err := c.Post("context-store", body)
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
	cmd.Flags().BoolVar(&shared, "shared", false, "Set scope to shared (cross-tenant)")
	return cmd
}

// ── search ───────────────────────────────────────────────────────────

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

			resp, err := c.Post("context-search", body)
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── stats ────────────────────────────────────────────────────────────

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
			resp, err := c.Post("context-manage", map[string]any{"action": "stats"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── categories ───────────────────────────────────────────────────────

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
			resp, err := c.Post("context-manage", map[string]any{"action": "list-categories"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── get ──────────────────────────────────────────────────────────────

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
			resp, err := c.Post("context-manage", map[string]any{
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

// ── delete ───────────────────────────────────────────────────────────

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
			resp, err := c.Post("context-manage", map[string]any{
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

// ── list-meta ────────────────────────────────────────────────────────

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
			resp, err := c.Post("context-manage", map[string]any{"action": "list-meta"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── digest ───────────────────────────────────────────────────────────

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
			resp, err := c.Post("context-digest", map[string]any{"trigger": "manual"})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── guard ────────────────────────────────────────────────────────────

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
	resp, err := c.Post("context-manage", map[string]any{"action": "guard-list"})
	if err != nil {
		return err
	}

	// Parse and format like the bash version
	var data map[string]any
	if err := json.Unmarshal(resp, &data); err != nil {
		PrintRaw(resp)
		return nil
	}

	if success, _ := data["success"].(bool); !success {
		errMsg, _ := data["error"].(string)
		if errMsg == "" {
			errMsg = "unknown"
		}
		return fmt.Errorf("Error: %s", errMsg)
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
			resp, err := c.Post("context-manage", map[string]any{"action": "guard-stats"})
			if err != nil {
				return err
			}

			var d map[string]any
			if err := json.Unmarshal(resp, &d); err != nil {
				PrintRaw(resp)
				return nil
			}

			if success, _ := d["success"].(bool); !success {
				errMsg, _ := d["error"].(string)
				if errMsg == "" {
					errMsg = "unknown"
				}
				return fmt.Errorf("Error: %s", errMsg)
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

			resp, err := c.Post("context-manage", map[string]any{
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
				return nil
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
				return fmt.Errorf("Error: %s", errMsg)
			}
			return nil
		},
	}
}

// ── manage ───────────────────────────────────────────────────────────

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

			resp, err := c.Post("context-manage", body)
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── health ───────────────────────────────────────────────────────────

func healthCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Healthcheck (DB + Ollama connectivity)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			// Health endpoint is GET /health, not under /webhook/
			// BaseURL is like https://ctx.janetzky.cloud/webhook
			// We need https://ctx.janetzky.cloud/health
			baseURL := c.BaseURL
			baseURL = strings.TrimSuffix(baseURL, "/webhook")
			baseURL = strings.TrimSuffix(baseURL, "/")

			resp, err := c.Get(baseURL + "/health")
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}
