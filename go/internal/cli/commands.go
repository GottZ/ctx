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
	root.AddCommand(ejectCmd(getClient))
	root.AddCommand(quotaCmd(getClient))
	root.AddCommand(blocksCmd(getClient))
	root.AddCommand(blockGrantCmd(getClient))
	root.AddCommand(tenantCmd(getClient))
	root.AddCommand(projectCmd(getClient))
	root.AddCommand(kanbanCmd(getClient))
	root.AddCommand(apiCmd(getClient))
	root.AddCommand(initCmd())
	root.AddCommand(contractCmd(getClient))
	root.AddCommand(adminCmd(getClient))
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
		Use:     "search [category] [query:text] [tags:a,b] [cluster:handle] [limit:N] [compact:false]",
		Aliases: []string{"browse", "b"},
		Short:   "Compact search (no LLM)",
		Long: "Search the context store with compact results. Key-value args for filtering.\n\n" +
			"cluster:<handle> restricts the result to ONE topic of the cluster map — the\n" +
			"stable handle the graph surfaces emit (`topic` on /api/graph/overview and on\n" +
			"the ego annotation), never an internal id. The facet is server-gated\n" +
			"(cluster.facet_enabled); while it is off the argument is ignored, so the\n" +
			"answer is an ordinary unfiltered search.",
		Example: `  ctx search learnings query:prompt
  ctx search decisions query:guard tags:security limit:5
  ctx search cluster:019c9629-0000-7000-9000-00000000a001`,
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
				case strings.HasPrefix(arg, "cluster:"):
					// C6 topic facet. Parsed unconditionally — the CLI cannot know
					// the server's cluster.facet_enabled state, and probing for it
					// would be a round trip per search. The server decides: gate off
					// ⇒ field ignored, malformed handle ⇒ 400 with a plain reason.
					body["cluster"] = strings.TrimPrefix(arg, "cluster:")
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

type guardListOpts struct {
	status   string
	category string
	types    []string
	limit    int
	idsOnly  bool
}

func guardListCmd(getClient func() (*Client, error)) *cobra.Command {
	opts := &guardListOpts{}
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List flagged blocks",
		Long:    "List flagged blocks. Composable with resolve:\n  ctx guard list --status needs_review --type checkpoint --ids-only | xargs ctx guard resolve keep",
		RunE: func(cmd *cobra.Command, args []string) error {
			return guardListRunOpts(getClient, opts)
		},
	}
	cmd.Flags().StringVar(&opts.status, "status", "", "filter by guard status (e.g. needs_review)")
	cmd.Flags().StringVar(&opts.category, "category", "", "filter by category")
	cmd.Flags().StringSliceVar(&opts.types, "type", nil, "filter by block type (repeatable)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "max results (server clamps to 200)")
	cmd.Flags().BoolVar(&opts.idsOnly, "ids-only", false, "print block ids only, one per line")
	return cmd
}

func guardListRun(getClient func() (*Client, error)) error {
	return guardListRunOpts(getClient, &guardListOpts{})
}

func guardListRunOpts(getClient func() (*Client, error), opts *guardListOpts) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	payload := map[string]any{"action": "guard-list"}
	if opts.status != "" {
		payload["status"] = opts.status
	}
	if opts.category != "" {
		payload["category"] = opts.category
	}
	if len(opts.types) > 0 {
		payload["types"] = opts.types
	}
	if opts.limit > 0 {
		payload["limit"] = opts.limit
	}
	resp, err := c.Post("manage", payload)
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
		if !opts.idsOnly {
			fmt.Println("No flagged blocks.")
		}
		return nil
	}

	if opts.idsOnly {
		for _, raw := range blocks {
			if b, ok := raw.(map[string]any); ok {
				fmt.Printf("%v\n", b["id"])
			}
		}
		return nil
	}

	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fmt.Printf("[%v] %v\n", b["guard_status"], b["title"])
		fmt.Printf("  id: %v  category: %v  type: %v\n", b["id"], b["category"], b["type_name"])
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

// splitGuardResolveArgs separates the resolution keyword from the block ids.
// Exactly one argument must be 'archive' or 'keep'; its position is free, so
// both the documented form (resolve <id> keep) and the xargs-friendly form
// (resolve keep <id...>) work.
func splitGuardResolveArgs(args []string) (resolution string, ids []string, err error) {
	for _, a := range args {
		if a == "archive" || a == "keep" {
			if resolution != "" {
				return "", nil, fmt.Errorf("resolution given twice (%q and %q)", resolution, a)
			}
			resolution = a
			continue
		}
		ids = append(ids, a)
	}
	if resolution == "" {
		return "", nil, fmt.Errorf("resolution must be 'archive' or 'keep'")
	}
	if len(ids) == 0 {
		return "", nil, fmt.Errorf("at least one block id required")
	}
	return resolution, ids, nil
}

func guardResolveCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "resolve <block-id...> <archive|keep>",
		Aliases: []string{"r"},
		Short:   "Resolve flagged blocks (one or many)",
		Long:    "Resolve flagged blocks. The resolution keyword may come first or last, so this composes with list --ids-only:\n  ctx guard list --status needs_review --type checkpoint --ids-only | xargs ctx guard resolve keep",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolution, ids, err := splitGuardResolveArgs(args)
			if err != nil {
				return err
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			// Single id keeps the original wire shape; many ids use the batch.
			payload := map[string]any{"action": "guard-resolve"}
			if len(ids) == 1 {
				payload["id"] = ids[0]
				payload["data"] = map[string]any{"resolution": resolution}
			} else {
				payload["data"] = map[string]any{"resolution": resolution, "ids": ids}
			}

			resp, err := c.Post("manage", payload)
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

			if resolved, ok := d["resolved"].(map[string]any); ok && resolved != nil {
				// Single-id response.
				id := fmt.Sprintf("%v", resolved["id"])
				if len(id) > 12 {
					id = id[:12] + "..."
				}
				fmt.Printf("%v: %v (%s)\n", resolved["guard_status"], resolved["title"], id)
				return nil
			}

			// Batch response.
			fmt.Printf("Resolved %v block(s) as %v", d["resolved_count"], resolution)
			if sc, ok := d["skipped_count"].(float64); ok && sc > 0 {
				fmt.Printf(", skipped %v", int(sc))
			}
			fmt.Println(".")
			if skipped, ok := d["skipped"].([]any); ok {
				for _, raw := range skipped {
					if s, ok := raw.(map[string]any); ok {
						fmt.Printf("  skipped %v: %v\n", s["id"], s["reason"])
					}
				}
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
	cmd.AddCommand(dreamResolveCmd(getClient))
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

func dreamResolveCmd(getClient func() (*Client, error)) *cobra.Command {
	var rationale string
	cmd := &cobra.Command{
		Use:     "resolve <source-id> <target-id> <relationship> <confirm|delete>",
		Aliases: []string{"r"},
		Short:   "Confirm (pin) or delete one dream link",
		Long: "Resolve one dream link from `ctx dream review`: confirm pins it so it survives the dream replace sweep " +
			"(optionally recording a durable --rationale); delete removes it and reverts a supersedes snapshot-marking. " +
			"The link is addressed as seen in the review output (source_id, target_id, relationship).",
		Args: cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolution := args[3]
			if resolution != "confirm" && resolution != "delete" {
				return fmt.Errorf("resolution must be 'confirm' or 'delete'")
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			data := map[string]any{
				"source_id":    args[0],
				"target_id":    args[1],
				"relationship": args[2],
				"resolution":   resolution,
			}
			if rationale != "" {
				data["rationale"] = rationale
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "dream-link-resolve",
				"data":   data,
			})
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

			resolved, _ := d["resolved"].(map[string]any)
			rel := args[2]
			if r, ok := resolved["relationship"].(string); ok && r != "" {
				rel = r
			}
			switch resolution {
			case "confirm":
				fmt.Printf("pinned: %s -[%s]-> %s\n", args[0], rel, args[1])
				if rat, ok := resolved["rationale"].(string); ok && rat != "" {
					fmt.Printf("  rationale: %s\n", rat)
				}
			case "delete":
				fmt.Printf("deleted: %s -[%s]-> %s\n", args[0], rel, args[1])
				if reverted, ok := resolved["supersedes_reverted"].(bool); ok && reverted {
					fmt.Printf("  supersedes reverted: %s back to lifecycle_state=knowledge\n", args[1])
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rationale, "rationale", "", "Durable justification stored on the link")
	return cmd
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
	var redirectURIs []string
	cmd := &cobra.Command{
		Use:   "add <label>",
		Short: "Register a new MCP OAuth client",
		Example: `  ctx mcp add "Claude AI"
  ctx mcp add "Claude Code Desktop"
  ctx mcp add "My Client" --redirect-uri https://example.com/cb --redirect-uri http://127.0.0.1:7777/cb`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := strings.Join(args, " ")
			c, err := getClient()
			if err != nil {
				return err
			}
			data := map[string]any{"label": label}
			if len(redirectURIs) > 0 {
				data["redirect_uris"] = redirectURIs
			}
			resp, err := c.Post("manage", map[string]any{
				"action": "mcp-client-create",
				"data":   data,
			})
			if err != nil {
				return err
			}
			var result struct {
				Success      bool     `json:"success"`
				ClientID     string   `json:"client_id"`
				ClientSecret string   `json:"client_secret"`
				Label        string   `json:"label"`
				RedirectURIs []string `json:"redirect_uris"`
				Error        string   `json:"error"`
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
			if len(result.RedirectURIs) > 0 {
				fmt.Printf("  redirect_uris: %s\n", strings.Join(result.RedirectURIs, ", "))
			}
			fmt.Printf("\n  Save the secret now — it cannot be retrieved later.\n")
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&redirectURIs, "redirect-uri", nil,
		"exact redirect URI to allowlist (repeatable; https, or http on loopback)")
	return cmd
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
		writeScopes   []string
	)
	cmd := &cobra.Command{
		Use:   "create <label>",
		Short: "Provision a new API key (home_scope is required)",
		Example: `  ctx keys create bench-crag --home crag
  ctx keys create work-key --home work --allowed shared,work
  ctx keys create writer --home private --allowed shared,work --write work
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
			// write_scopes (078, E4b): scopes the key may WRITE to beyond home_scope.
			// Server rejects any not ⊆ allowed_scopes ∪ {home_scope}.
			if len(writeScopes) > 0 {
				data["write_scopes"] = writeScopes
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
				WriteScopes   []string `json:"write_scopes"`
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
			fmt.Printf("  write_scopes:   %v\n", result.WriteScopes)
			fmt.Printf("  api_key:        %s\n", result.ApiKey)
			fmt.Printf("\n  Save the key now — it cannot be retrieved later.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&homeScope, "home", "", "home_scope for the new key (REQUIRED, e.g. 'private', 'work', 'crag')")
	cmd.Flags().StringSliceVar(&allowedScopes, "allowed", nil, "additional allowed read scopes (comma-separated, default: shared)")
	cmd.Flags().StringSliceVar(&writeScopes, "write", nil, "additional write scopes (comma-separated; must be ⊆ home + allowed) [078]")
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
			WriteScopes   []string `json:"write_scopes"`
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
		write := ""
		if len(k.WriteScopes) > 0 {
			write = fmt.Sprintf("  write=%v", k.WriteScopes)
		}
		fmt.Printf("  %s  %s  home=%s%s  [%s]  %s\n", k.ID, k.Label, k.HomeScope, write, status, created)
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
