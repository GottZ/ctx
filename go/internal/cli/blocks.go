// ctx blocks — block corpus maintenance: sensitivity LLM audit (G41) and the
// deterministic credentials pattern re-audit (G40).
//
//	ctx blocks audit                  # = status
//	ctx blocks audit status           # progress: pending, by-source, run state
//	ctx blocks audit sample [--n 30]  # N-block dry run (sample gate, no writes)
//	ctx blocks audit start [--limit N]# live run (writes verdicts)
//	ctx blocks classify               # = status
//	ctx blocks classify dry-run       # full pattern scan, NO writes (FP gate)
//	ctx blocks classify start [--limit N] # live run (raises hits to credentials)
//
// All subcommands need an ADMIN key (corpus-wide mutation + topology
// disclosure). Envelope discipline: success:false reaches stderr with exit 1.
package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
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
			return runBlocksAuditStatus(getClient, nil)
		},
	}

	auditCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show audit progress (pending, by-source, current/last run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksAuditStatus(getClient, nil)
		},
	})

	var sampleN int
	sampleCmd := &cobra.Command{
		Use:   "sample",
		Short: "Dry-run N random pending blocks — verdicts WITHOUT writes (sample gate)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksAuditStart(getClient, blocksStartData(true, sampleN))
		},
	}
	sampleCmd.Flags().IntVar(&sampleN, "n", 30, "sample size")
	auditCmd.AddCommand(sampleCmd)

	var startLimit int
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the live audit run (writes verdicts; 'manual' stays untouched)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksAuditStart(getClient, blocksStartData(false, startLimit))
		},
	}
	startCmd.Flags().IntVar(&startLimit, "limit", 0, "stop after N blocks (0 = drain)")
	auditCmd.AddCommand(startCmd)

	cmd.AddCommand(auditCmd)
	cmd.AddCommand(classifyCmd(getClient))
	return cmd
}

// classifyStatusView mirrors the server's blocks-classify-status wire shape.
type classifyStatusView struct {
	Scope    string         `json:"scope"`
	BySource map[string]int `json:"by_source"`
	Run      struct {
		Running    bool   `json:"running"`
		DryRun     bool   `json:"dry_run"`
		StartedAt  string `json:"started_at"`
		FinishedAt string `json:"finished_at"`
		Scanned    int    `json:"scanned"`
		Upgraded   int    `json:"upgraded"`
		Discarded  int    `json:"discarded"`
		Aborted    bool   `json:"aborted"`
		LastError  string `json:"last_error"`
		Samples    []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Kind  string `json:"kind"`
		} `json:"samples"`
	} `json:"run"`
}

// classifyCmd builds `ctx blocks classify` (G40): the deterministic credentials
// PATTERN re-audit. dry-run scans without writing (see what WOULD be raised on
// the real corpus first); start raises every hit to credentials, upgrade-only.
func classifyCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "classify",
		Short: "Credentials pattern re-audit: raise hits to credentials (G40)",
		Long: "Scans every home-scope block (except manual / already-credentials) with the\n" +
			"deterministic pattern+entropy detector and raises hits to credentials\n" +
			"(sensitivity_source='pattern' — the veto the G41 LLM audit can never re-touch).\n" +
			"Upgrade-only: it never downgrades. Run dry-run FIRST to measure false positives.",
		Example: `  ctx blocks classify                # status
  ctx blocks classify dry-run        # full scan, NO writes (FP gate)
  ctx blocks classify start          # live run, raise every hit
  ctx blocks classify start --limit 100`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksClassifyStatus(getClient, nil)
		},
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show classify progress (by-source, current/last run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksClassifyStatus(getClient, nil)
		},
	})

	var dryLimit int
	dryCmd := &cobra.Command{
		Use:   "dry-run",
		Short: "Scan WITHOUT writing — list what would be raised (FP gate)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksClassifyStart(getClient, blocksStartData(true, dryLimit))
		},
	}
	dryCmd.Flags().IntVar(&dryLimit, "limit", 0, "stop after N blocks scanned (0 = all)")
	cmd.AddCommand(dryCmd)

	var startLimit int
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Live run: raise every pattern hit to credentials (upgrade-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlocksClassifyStart(getClient, blocksStartData(false, startLimit))
		},
	}
	startCmd.Flags().IntVar(&startLimit, "limit", 0, "stop after N blocks scanned (0 = all)")
	cmd.AddCommand(startCmd)

	return cmd
}

// bySourceOrder is the reading order of the sensitivity_source classes, and the
// ONE list both status renderers use. It mirrors the CHECK constraint on
// context_blocks.sensitivity_source (113_baseline.sql, extended by migration 144
// with 'derived') in the order an operator reads the pipeline: unclassified
// first, then the two automatic classes, then the untouchable manual verdict,
// then the folded value a derived block inherits from its sources.
//
// It used to be two copies of a four-name literal, one per renderer, and the
// W01-4 review found what that costs: the server had been counting five classes
// since migration 144, the wire carried all five, and 'derived' fell off the TTY
// path in both places without a trace. Two copies also meant a fix could land in
// one and not the other.
var bySourceOrder = []string{"default", "llm-audit", "pattern", "manual", "derived"}

// formatBySource renders the by-source counts as "class=n  class=n", known
// classes in bySourceOrder, then anything else sorted after them. It returns ""
// for an empty or nil map, so a caller can decide whether to print a line at all.
//
// The trailing sorted remainder is the actual repair. Listing 'derived' fixes
// today's symptom; the DEFECT was a renderer that silently drops whatever it
// does not recognise, which turns the next class somebody adds into the same
// invisible bug. Now an unknown class shows up — out of the reading order, which
// is the honest signal that this list wants updating — instead of vanishing.
func formatBySource(bySource map[string]int) string {
	if len(bySource) == 0 {
		return ""
	}
	parts := make([]string, 0, len(bySource))
	known := make(map[string]bool, len(bySourceOrder))
	for _, k := range bySourceOrder {
		known[k] = true
		if n, ok := bySource[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n))
		}
	}
	rest := make([]string, 0, len(bySource))
	for k := range bySource {
		if !known[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		parts = append(parts, fmt.Sprintf("%s=%d", k, bySource[k]))
	}
	return strings.Join(parts, "  ")
}

func printClassifyStatus(resp json.RawMessage) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var v classifyStatusView
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
	fmt.Printf("scope: %s   run: %s\n", v.Scope, state)

	if line := formatBySource(v.BySource); line != "" {
		fmt.Printf("by-source: %s\n", line)
	}

	if v.Run.Scanned > 0 || v.Run.Running {
		verb := "upgraded"
		if v.Run.DryRun {
			verb = "would-upgrade"
		}
		fmt.Printf("scanned: %d   %s: %d   discarded: %d\n", v.Run.Scanned, verb, v.Run.Upgraded, v.Run.Discarded)
	}

	if len(v.Run.Samples) > 0 {
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "KIND\tTITLE\tID")
		for _, s := range v.Run.Samples {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", s.Kind, truncate(s.Title, 48), s.ID)
		}
		_ = w.Flush()
	}
	return nil
}

// blocksRun binds one blocks-* manage action to the renderer of its family:
// start and status answer through one envelope per family (server side:
// design 03 §4.5), so the action name and the printer are the whole difference
// between the four legs. data is nil for a status leg and the start payload
// for a start leg.
func blocksRun(action string, print func(json.RawMessage) error) func(func() (*Client, error), json.RawMessage) error {
	return func(getClient func() (*Client, error), data json.RawMessage) error {
		resp, err := blocksManageCall(getClient, action, data)
		if err != nil {
			return err
		}
		return print(resp)
	}
}

// blocksStartData is the {"dry_run","limit"} body both start actions take.
func blocksStartData(dryRun bool, limit int) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"dry_run": dryRun, "limit": limit})
	return data
}

// The four CLI legs, one per blocks-* manage action.
var (
	runBlocksAuditStatus    = blocksRun("blocks-audit-status", printAuditStatus)
	runBlocksAuditStart     = blocksRun("blocks-audit-start", printAuditStatus)
	runBlocksClassifyStatus = blocksRun("blocks-classify-status", printClassifyStatus)
	runBlocksClassifyStart  = blocksRun("blocks-classify-start", printClassifyStatus)
)

func blocksManageCall(getClient func() (*Client, error), action string, data json.RawMessage) (json.RawMessage, error) {
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

	if line := formatBySource(v.BySource); line != "" {
		fmt.Printf("by-source: %s\n", line)
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
