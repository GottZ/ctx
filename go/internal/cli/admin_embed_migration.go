// ctx admin embed-migration — CLI for the re-embed migration control surface
// (Evokoa-Clean-Room design/04 §7 W04-7). A thin transport over the admin-gated
// /api/manage embed-migration-* family: this file builds requests and renders
// compact output; ALL policy/mechanics live server-side (embedmigration.* +
// events.*EmbedMigration). Every subcommand parses the response envelope so a
// fail-closed refusal (403 non-admin, 409 active-migration, a model-guard pause,
// the disk pre-flight) reaches stderr with exit 1 — the settings/backends CLI
// discipline, not PrintJSON-and-exit-0.
package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// adminCmd is the server-admin operator namespace. Today it carries only the
// embed-migration surface; it is a parent so future server-admin operator tools
// (schema, corpus maintenance) attach without a new top-level command each.
func adminCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Server-admin operator tools (admin key required)",
		Long:  "Operator-level controls that act across the whole process, not within one tenant.",
	}
	cmd.AddCommand(embedMigrationCmd(getClient))
	return cmd
}

func embedMigrationCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "embed-migration",
		Aliases: []string{"reembed"},
		Short:   "Drive the re-embed migration statemachine",
		Long: "Control the corpus embedding-space migration: create → (running/verifying) →\n" +
			"confirm → cleanup, with pause/resume/abort/rollback/purge. Status is arithmetic\n" +
			"(no count(*) on the corpus); verify_report + block-IDs are admin-only.",
		Example: `  ctx admin embed-migration create --from qwen3-embedding-8b --to qwen3-next --backend llama-embed-next
  ctx admin embed-migration status
  ctx admin embed-migration status --exact
  ctx admin embed-migration pause
  ctx admin embed-migration confirm
  ctx admin embed-migration rollback --reason "recall regression on large stratum"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEmbedMigrationStatus(getClient, "", false)
		},
	}
	cmd.AddCommand(embedMigrationCreateCmd(getClient))
	cmd.AddCommand(embedMigrationStatusCmd(getClient))
	cmd.AddCommand(embedMigrationTransitionCmd(getClient, "pause", "Pause the running migration (running→paused)"))
	cmd.AddCommand(embedMigrationTransitionCmd(getClient, "resume", "Start/resume the migration (pending or paused → running)"))
	cmd.AddCommand(embedMigrationReasonCmd(getClient, "abort", "Abort the active migration (reason required)"))
	cmd.AddCommand(embedMigrationTransitionCmd(getClient, "confirm", "Confirm the verified migration — run the cutover"))
	cmd.AddCommand(embedMigrationReasonCmd(getClient, "rollback", "Roll back a done migration (reason required; needs the _old anchor)"))
	cmd.AddCommand(embedMigrationTransitionCmd(getClient, "cleanup", "Drop the _old rollback anchor of a done migration"))
	cmd.AddCommand(embedMigrationTransitionCmd(getClient, "purge", "Null leftover embedding_next data (only when no migration is active)"))
	cmd.AddCommand(embedMigrationFailuresCmd(getClient))
	return cmd
}

// emPost posts an embed-migration-<action> to /api/manage and returns the parsed
// envelope error (nil on success) plus the raw body for rendering.
func emPost(getClient func() (*Client, error), action string, data map[string]any) ([]byte, error) {
	c, err := getClient()
	if err != nil {
		return nil, err
	}
	body := map[string]any{"action": "embed-migration-" + action}
	if data != nil {
		body["data"] = data
	}
	resp, _, err := c.Do(http.MethodPost, "/api/manage", body)
	if err != nil {
		return nil, err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func embedMigrationCreateCmd(getClient func() (*Client, error)) *cobra.Command {
	var from, to, backend string
	var reuse bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a migration (validates models/backend + real disk pre-flight)",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := emPost(getClient, "create", map[string]any{
				"from_model": from, "to_model": to, "to_backend": backend, "reuse_existing": reuse,
			})
			if err != nil {
				return err
			}
			var r struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(resp, &r)
			fmt.Printf("migration created: %s\n", r.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "from_model (must be registered in context_embed_models)")
	cmd.Flags().StringVar(&to, "to", "", "to_model (must be registered)")
	cmd.Flags().StringVar(&backend, "backend", "", "to_backend name (local, global-scoped, model_map embed_next→to_model)")
	cmd.Flags().BoolVar(&reuse, "reuse-existing", false, "reuse leftover embedding_next from a prior ABORTED migration (model must match)")
	return cmd
}

func embedMigrationStatusCmd(getClient func() (*Client, error)) *cobra.Command {
	var exact bool
	var id string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the active (or --id) migration: counts + arithmetic pending",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEmbedMigrationStatus(getClient, id, exact)
		},
	}
	cmd.Flags().BoolVar(&exact, "exact", false, "additionally compute the EXACT index count (a scan — use sparingly)")
	cmd.Flags().StringVar(&id, "id", "", "target a specific migration id (default: the active one)")
	return cmd
}

func runEmbedMigrationStatus(getClient func() (*Client, error), id string, exact bool) error {
	data := map[string]any{}
	if exact {
		data["exact"] = true
	}
	if len(data) == 0 {
		data = nil
	}
	c, err := getClient()
	if err != nil {
		return err
	}
	body := map[string]any{"action": "embed-migration-status"}
	if data != nil {
		body["data"] = data
	}
	if id != "" {
		body["id"] = id
	}
	resp, _, err := c.Do(http.MethodPost, "/api/manage", body)
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
		Migration *struct {
			ID                string  `json:"id"`
			Status            string  `json:"status"`
			FromModel         string  `json:"from_model"`
			ToModel           string  `json:"to_model"`
			ToBackend         string  `json:"to_backend"`
			TotalBlocks       int64   `json:"total_blocks"`
			MigratedCount     int64   `json:"migrated_count"`
			FailedCount       int64   `json:"failed_count"`
			SkippedCount      int64   `json:"skipped_count"`
			Pending           int64   `json:"pending"`
			PendingExact      *int64  `json:"pending_exact"`
			InfinityMigration int64   `json:"infinity_migration"`
			InfinityBackfill  int64   `json:"infinity_backfill"`
			HasVerifyReport   bool    `json:"has_verify_report"`
			LastError         *string `json:"last_error"`
		} `json:"migration"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return nil //nolint:nilerr // raw already printed
	}
	if payload.Migration == nil {
		fmt.Println("No active migration.")
		return nil
	}
	m := payload.Migration
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "id:\t%s\n", m.ID)
	_, _ = fmt.Fprintf(w, "status:\t%s\n", m.Status)
	_, _ = fmt.Fprintf(w, "models:\t%s → %s (backend %s)\n", m.FromModel, m.ToModel, m.ToBackend)
	_, _ = fmt.Fprintf(w, "total:\t%d\n", m.TotalBlocks)
	_, _ = fmt.Fprintf(w, "migrated:\t%d\n", m.MigratedCount)
	_, _ = fmt.Fprintf(w, "failed:\t%d\n", m.FailedCount)
	_, _ = fmt.Fprintf(w, "skipped:\t%d\n", m.SkippedCount)
	_, _ = fmt.Fprintf(w, "pending (arith):\t%d\n", m.Pending)
	if m.PendingExact != nil {
		_, _ = fmt.Fprintf(w, "pending (exact):\t%d\n", *m.PendingExact)
	}
	_, _ = fmt.Fprintf(w, "parked ∞ (migration):\t%d\n", m.InfinityMigration)
	_, _ = fmt.Fprintf(w, "parked ∞ (backfill):\t%d\n", m.InfinityBackfill)
	_, _ = fmt.Fprintf(w, "verify_report:\t%v\n", m.HasVerifyReport)
	if m.LastError != nil && *m.LastError != "" {
		_, _ = fmt.Fprintf(w, "last_error:\t%s\n", *m.LastError)
	}
	_ = w.Flush()
	return nil
}

// embedMigrationTransitionCmd builds a reason-less action command (pause/resume/
// confirm/cleanup/purge). Confirm renders the cutover numbers.
func embedMigrationTransitionCmd(getClient func() (*Client, error), action, short string) *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:   action,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			body := map[string]any{"action": "embed-migration-" + action}
			if id != "" {
				body["id"] = id
			}
			resp, _, err := c.Do(http.MethodPost, "/api/manage", body)
			if err != nil {
				return err
			}
			if err := checkSettingsEnvelope(resp); err != nil {
				return err
			}
			if action == "confirm" {
				renderConfirm(resp)
				return nil
			}
			if !StdoutIsTTY() {
				PrintJSON(resp)
				return nil
			}
			fmt.Printf("%s: ok\n", action)
			return nil
		},
	}
	if action != "purge" {
		cmd.Flags().StringVar(&id, "id", "", "target migration id (default: the active one)")
	}
	return cmd
}

func renderConfirm(resp []byte) {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return
	}
	var r struct {
		ID                   string   `json:"id"`
		FromModel            string   `json:"from_model"`
		ToModel              string   `json:"to_model"`
		VisibilityLoss       int64    `json:"visibility_loss"`
		PostWatermarkPending int64    `json:"post_watermark_pending"`
		SweepCleared         int64    `json:"sweep_cleared"`
		MemosCopied          int64    `json:"memos_copied"`
		FlippedBackends      []string `json:"flipped_backends"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		PrintJSON(resp)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "cutover done:\t%s (%s → %s)\n", r.ID, r.FromModel, r.ToModel)
	_, _ = fmt.Fprintf(w, "visibility_loss:\t%d\n", r.VisibilityLoss)
	_, _ = fmt.Fprintf(w, "post_watermark_pending:\t%d\n", r.PostWatermarkPending)
	_, _ = fmt.Fprintf(w, "sweep_cleared:\t%d\n", r.SweepCleared)
	_, _ = fmt.Fprintf(w, "memos_copied:\t%d\n", r.MemosCopied)
	_, _ = fmt.Fprintf(w, "flipped_backends:\t%v\n", r.FlippedBackends)
	_ = w.Flush()
}

// embedMigrationReasonCmd builds a reason-carrying action (abort/rollback) — the
// mandatory reason is a --reason flag surfaced to the server (empty → the
// mechanics refuse verbatim).
func embedMigrationReasonCmd(getClient func() (*Client, error), action, short string) *cobra.Command {
	var reason, id string
	cmd := &cobra.Command{
		Use:   action,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			body := map[string]any{
				"action": "embed-migration-" + action,
				"data":   map[string]any{"reason": reason},
			}
			if id != "" {
				body["id"] = id
			}
			resp, _, err := c.Do(http.MethodPost, "/api/manage", body)
			if err != nil {
				return err
			}
			if err := checkSettingsEnvelope(resp); err != nil {
				return err
			}
			if action == "rollback" {
				renderConfirm(resp) // same shape family (from/to/sweep/flipped)
				return nil
			}
			if !StdoutIsTTY() {
				PrintJSON(resp)
				return nil
			}
			fmt.Printf("%s: ok\n", action)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "operator reason (mandatory)")
	cmd.Flags().StringVar(&id, "id", "", "target migration id (default: the active one)")
	return cmd
}

func embedMigrationFailuresCmd(getClient func() (*Client, error)) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "failures",
		Short: "List context_embed_failures memos (admin-only; last_error is normalized)",
		RunE: func(cmd *cobra.Command, args []string) error {
			data := map[string]any{}
			if limit > 0 {
				data["limit"] = limit
			}
			if len(data) == 0 {
				data = nil
			}
			resp, err := emPost(getClient, "failures", data)
			if err != nil {
				return err
			}
			if !StdoutIsTTY() {
				PrintJSON(resp)
				return nil
			}
			var payload struct {
				Failures []struct {
					BlockID       string `json:"block_id"`
					Attempts      int    `json:"attempts"`
					LastClass     string `json:"last_class"`
					NextAttemptAt string `json:"next_attempt_at"`
					LastError     string `json:"last_error"`
				} `json:"failures"`
			}
			if err := json.Unmarshal(resp, &payload); err != nil {
				PrintJSON(resp)
				return nil //nolint:nilerr // raw already printed
			}
			if len(payload.Failures) == 0 {
				fmt.Println("No embed failures.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "BLOCK\tATTEMPTS\tCLASS\tNEXT_ATTEMPT\tLAST_ERROR")
			for _, f := range payload.Failures {
				_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", f.BlockID, f.Attempts, f.LastClass, f.NextAttemptAt, f.LastError)
			}
			_ = w.Flush()
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (default 50, max 500)")
	return cmd
}
