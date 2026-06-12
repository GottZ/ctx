// ctx blocks — block corpus maintenance (G41: sensitivity LLM audit).
//
//	ctx blocks audit                  # = status
//	ctx blocks audit status           # progress: pending, by-source, run state
//	ctx blocks audit sample [--n 30]  # N-block dry run (sample gate, no writes)
//	ctx blocks audit start [--limit N]# live run (writes verdicts)
//
// All subcommands need an ADMIN key (bulk downgrades are the opsec
// direction). Envelope discipline: success:false reaches stderr with exit 1.
package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// auditStatusView mirrors the server's blocks-audit-status wire shape.
type auditStatusView struct {
	Scope    string         `json:"scope"`
	Pending  int            `json:"pending"`
	BySource map[string]int `json:"by_source"`
	Run      struct {
		Running         bool   `json:"running"`
		DryRun          bool   `json:"dry_run"`
		StartedAt       string `json:"started_at"`
		FinishedAt      string `json:"finished_at"`
		Processed       int    `json:"processed"`
		KeptCredentials int    `json:"kept_credentials"`
		ToPersonal      int    `json:"to_personal"`
		ToInternal      int    `json:"to_internal"`
		NoVerdict       int    `json:"no_verdict"`
		Discarded       int    `json:"discarded"`
		Aborted         bool   `json:"aborted"`
		LastError       string `json:"last_error"`
		Samples         []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Credentials *bool  `json:"credentials"`
			Personal    *bool  `json:"personal"`
			Verdict     string `json:"verdict"`
		} `json:"samples"`
	} `json:"run"`
}

func blocksCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "blocks",
		Short: "Block corpus maintenance (admin key required)",
	}

	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Sensitivity LLM audit: classify default-sensitivity blocks (G41)",
		Long: "Classifies every home-scope block with sensitivity_source='default' via two\n" +
			"yes/no questions over the hard-local classify chain. Downgrade to internal only\n" +
			"on nein×2; personal on personal-ja; everything else stays credentials.\n" +
			"'manual' classifications are untouchable; public is never assigned.",
		Example: `  ctx blocks audit                  # status
  ctx blocks audit sample --n 30    # dry-run sample gate (no writes)
  ctx blocks audit start            # full live run
  ctx blocks audit start --limit 50 # live run, stop after 50 blocks`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksAuditStatus(getClient)
		},
	}

	auditCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show audit progress (pending, by-source, current/last run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksAuditStatus(getClient)
		},
	})

	var sampleN int
	sampleCmd := &cobra.Command{
		Use:   "sample",
		Short: "Dry-run N random pending blocks — verdicts WITHOUT writes (sample gate)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksAuditStart(getClient, true, sampleN)
		},
	}
	sampleCmd.Flags().IntVar(&sampleN, "n", 30, "sample size")
	auditCmd.AddCommand(sampleCmd)

	var startLimit int
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the live audit run (writes verdicts; 'manual' stays untouched)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksAuditStart(getClient, false, startLimit)
		},
	}
	startCmd.Flags().IntVar(&startLimit, "limit", 0, "stop after N blocks (0 = drain)")
	auditCmd.AddCommand(startCmd)

	cmd.AddCommand(auditCmd)
	return cmd
}

func runBlocksAuditStatus(getClient func() (*Client, error)) error {
	resp, err := blocksAuditCall(getClient, "blocks-audit-status", nil)
	if err != nil {
		return err
	}
	return printAuditStatus(resp)
}

func runBlocksAuditStart(getClient func() (*Client, error), dryRun bool, limit int) error {
	data, _ := json.Marshal(map[string]any{"dry_run": dryRun, "limit": limit})
	resp, err := blocksAuditCall(getClient, "blocks-audit-start", data)
	if err != nil {
		return err
	}
	return printAuditStatus(resp)
}

func blocksAuditCall(getClient func() (*Client, error), action string, data json.RawMessage) (json.RawMessage, error) {
	c, err := getClient()
	if err != nil {
		return nil, err
	}
	body := map[string]any{"action": action}
	if len(data) > 0 {
		body["data"] = data
	}
	resp, _, err := c.Do(http.MethodPost, "/api/manage", body)
	if err != nil {
		return nil, err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func printAuditStatus(resp json.RawMessage) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var v auditStatusView
	if err := json.Unmarshal(resp, &v); err != nil {
		PrintJSON(resp)
		return err
	}

	state := "idle"
	switch {
	case v.Run.Running && v.Run.DryRun:
		state = "running (dry-run)"
	case v.Run.Running:
		state = "running"
	case v.Run.Aborted:
		state = "ABORTED: " + v.Run.LastError
	case v.Run.StartedAt != "":
		state = "finished"
	}
	fmt.Printf("scope: %s   pending: %d   run: %s\n", v.Scope, v.Pending, state)

	if len(v.BySource) > 0 {
		parts := make([]string, 0, len(v.BySource))
		for _, k := range []string{"default", "llm-audit", "pattern", "manual"} {
			if n, ok := v.BySource[k]; ok {
				parts = append(parts, fmt.Sprintf("%s=%d", k, n))
			}
		}
		fmt.Printf("by-source: %s\n", strings.Join(parts, "  "))
	}

	if v.Run.Processed > 0 || v.Run.Running {
		fmt.Printf("processed: %d   credentials: %d   personal: %d   internal: %d   no-verdict: %d   discarded: %d\n",
			v.Run.Processed, v.Run.KeptCredentials, v.Run.ToPersonal, v.Run.ToInternal,
			v.Run.NoVerdict, v.Run.Discarded)
	}

	if len(v.Run.Samples) > 0 {
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "VERDICT\tCRED\tPERS\tTITLE\tID")
		for _, s := range v.Run.Samples {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				s.Verdict, boolMark(s.Credentials), boolMark(s.Personal), truncate(s.Title, 48), s.ID)
		}
		_ = w.Flush()
	}
	return nil
}

func boolMark(b *bool) string {
	switch {
	case b == nil:
		return "-"
	case *b:
		return "ja"
	default:
		return "nein"
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
