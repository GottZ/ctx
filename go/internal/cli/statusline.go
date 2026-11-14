package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/clientconfig"
	"github.com/spf13/cobra"
)

// maxStatuslineResponse is the maximum bytes read from backend responses (1 MB).
const maxStatuslineResponse = 1 * 1024 * 1024

// ── statusline input schema (Claude Code stdin) ─────────────────────.

type statusInput struct {
	Model         *statusModel      `json:"model"`
	Workspace     *statusWorkspace  `json:"workspace"`
	SessionID     string            `json:"session_id"`
	ContextWindow *statusContext    `json:"context_window"`
	RateLimits    *statusRateLimits `json:"rate_limits"`
	Cost          *statusCost       `json:"cost"`
}

type statusModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type statusWorkspace struct {
	CurrentDir string `json:"current_dir"`
	ProjectDir string `json:"project_dir"`
}

type statusContext struct {
	UsedPercentage      *float64 `json:"used_percentage"`
	RemainingPercentage *float64 `json:"remaining_percentage"`
	ContextWindowSize   int      `json:"context_window_size"`
}

type statusRateLimits struct {
	FiveHour *statusRateWindow `json:"five_hour"`
	SevenDay *statusRateWindow `json:"seven_day"`
}

type statusRateWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}

type statusCost struct {
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// ── backend data ────────────────────────────────────────────────────.

// statusCache is one fetch result — what the cache file keeps (statusline_cache.go).
type statusCache struct {
	Health        string  `json:"health"`          // the server's status, or healthError / healthTimeout
	BlockCount    float64 `json:"block_count"`     // from stats, meaningful only with HasBlockCount
	HasBlockCount bool    `json:"has_block_count"` // the stats call returned a count (0 included)
}

// statuslineCallBudget is the time each statusline call gets: it runs on every
// prompt, so it waits for nobody (design/03 §5.6).
const statuslineCallBudget = 500 * time.Millisecond

// The health states the statusline knows besides the server's own status
// words. A call that ran out of statuslineCallBudget is no evidence that the
// server is down (#265: a healthy /health answered in 350–580 ms), so "no
// answer" is its own state and not the offline one. (A status word outside the
// server's vocabulary, "unknown" included, still renders as offline.)
const (
	healthError   = "error"   // refused, unreachable, or an answer that is no health document
	healthTimeout = "timeout" // no answer within statuslineCallBudget
)

// ── ANSI helpers ────────────────────────────────────────────────────.

const (
	ansiReset   = "\x1b[0m"
	ansiDim     = "\x1b[2m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiOrange  = "\x1b[38;5;208m"
	ansiRed     = "\x1b[31m"
	ansiCyan    = "\x1b[36m"
	ansiDimCyan = "\x1b[2;36m"
)

// ── Unicode smooth progress bar ─────────────────────────────────────.

// Block characters for 1/8th resolution: index 0 = empty, 8 = full.
var barBlocks = [9]rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉', '█'}

const ansiBgDark = "\x1b[48;5;236m" // dark gray background
const ansiBgReset = "\x1b[49m"

const ansiReverse = "\x1b[7m"
const ansiReverseOff = "\x1b[27m"

// smoothBar renders a progress bar with sub-character resolution and
// embedded percentage label. The label sits in the empty area when it fits,
// or right-aligned in the filled area with reverse video when it doesn't.
func smoothBar(width int, pct float64) string {
	pct = math.Max(0, math.Min(100, pct))
	total := float64(width) * 8
	filled := pct / 100 * total

	// Label to embed
	label := fmt.Sprintf("%d%%", int(pct))
	labelRunes := []rune(label)
	labelLen := len(labelRunes)

	// Determine filled boundary in cells
	filledCells := int(filled / 8)
	partial := filled - float64(filledCells)*8
	emptyStart := filledCells
	if partial > 0 {
		emptyStart++ // partial cell counts as occupied
	}
	emptyCells := width - emptyStart

	// Place label: prefer empty area (left-aligned after fill edge),
	// fall back to filled area (right-aligned before partial block).
	labelInEmpty := emptyCells >= labelLen
	var labelStart int
	if labelInEmpty {
		labelStart = emptyStart
	} else {
		// End label before the partial block cell so the edge stays visible
		labelStart = filledCells - labelLen
		if labelStart < 0 {
			labelStart = 0
		}
	}
	labelEnd := labelStart + labelLen

	var b strings.Builder
	b.WriteRune('▕')
	b.WriteString(ansiBgDark)

	for i := 0; i < width; i++ {
		inLabel := i >= labelStart && i < labelEnd

		if inLabel {
			// Open/close reverse block at label boundaries (not per char)
			if i == labelStart && !labelInEmpty {
				b.WriteString(ansiReverse)
			}
			b.WriteRune(labelRunes[i-labelStart])
			if i == labelEnd-1 && !labelInEmpty {
				b.WriteString(ansiReverseOff)
			}
		} else {
			cellStart := float64(i) * 8
			cellFill := filled - cellStart
			switch {
			case cellFill >= 8:
				b.WriteRune('█')
			case cellFill <= 0:
				b.WriteRune(' ')
			default:
				b.WriteRune(barBlocks[int(cellFill)])
			}
		}
	}

	b.WriteString(ansiBgReset)
	b.WriteRune('▏')
	return b.String()
}

// barColor returns ANSI color for a percentage value.
func barColor(pct float64) string {
	switch {
	case pct < 50:
		return ansiGreen
	case pct < 65:
		return ansiYellow
	case pct < 80:
		return ansiOrange
	default:
		return ansiRed
	}
}

// rateColor returns ANSI color for rate limit usage.
func rateColor(pct float64) string {
	switch {
	case pct < 50:
		return ansiDim
	case pct < 75:
		return ansiYellow
	case pct < 90:
		return ansiOrange
	default:
		return ansiRed
	}
}

// ── health indicator ────────────────────────────────────────────────.

func healthIndicator(status string) string {
	switch status {
	case "ok":
		return "" // healthy: no indicator
	case "degraded":
		return " " + ansiYellow + "\uEA6C " + ansiReset
	case healthTimeout: // no answer within the budget, not offline
		return " " + ansiDim + "\uF252 " + ansiReset
	default: // unhealthy, error, unknown
		return " " + ansiRed + "\uF071 " + ansiReset
	}
}

// backendSegment renders what follows the project name: the block count
// whenever the stats call returned one, then the health indicator whenever
// health is not ok — next to the count, not instead of it (#265). backend ==
// nil means there is no client config: offline.
func backendSegment(backend *statusCache) string {
	if backend == nil {
		return healthIndicator(healthError)
	}
	var s string
	if backend.HasBlockCount {
		s = ansiDimCyan + " \U000F09EE " + fmt.Sprintf("%.0f", backend.BlockCount) + ansiReset
	}
	return s + healthIndicator(backend.Health)
}

// ── fetch backend data ──────────────────────────────────────────────.

// fetchBackendData returns the cached fetch result for cfg's server and key
// while it is younger than statuslineCacheTTL, and otherwise fetches, caches
// and returns a fresh one (#265). The cache is best effort: no cache
// directory, an unreadable or broken file, or a failed write all just mean a
// fetch.
func fetchBackendData(cfg Config) *statusCache {
	id := statuslineCacheID(cfg)
	path := statuslineCachePath(id)
	if cached, ok := readStatuslineCache(path, id, time.Now()); ok {
		return cached
	}
	result := fetchBackendLive(cfg)
	_ = writeStatuslineCache(path, id, result, time.Now())
	return result
}

// fetchBackendLive makes the two calls of one fetch: /health and the stats
// action.
func fetchBackendLive(cfg Config) *statusCache {
	result := &statusCache{Health: healthError}

	// The statusline speaks the same Client as every other command, but with
	// ITS budget: statuslineCallBudget per call (it runs on every prompt) and a
	// 1 MB read cap — both are parameters, not inherited from the 120 s / 10 MB
	// main client (design/03 §5.6). Errors stay silent: a statusline must never
	// crash the prompt, so every failed step keeps its placeholder.
	c := NewClientWithTimeout(cfg, statuslineCallBudget, maxStatuslineResponse)

	baseURL := strings.TrimSuffix(cfg.BaseURL, "/")

	result.Health = fetchHealth(c, baseURL)

	// Block count via stats
	if body, err := c.Post("manage", map[string]any{"action": "stats"}); err == nil {
		var s map[string]any
		if json.Unmarshal(body, &s) == nil {
			if stats, ok := s["stats"].(map[string]any); ok {
				if n, ok := stats["total_blocks"].(float64); ok {
					result.BlockCount = n
					result.HasBlockCount = true
				}
			}
		}
	}

	return result
}

// fetchHealth reads the status word of GET /health. Deliberately NOT c.Get:
// /health is the one anonymous route (cmd/ctxd/server.go registers it before
// the auth middleware and the handler reads no header), and Client.Get would
// put X-Context-Key on it. The statusline fires on every prompt, so that would
// spray the key across the public health path for no gain — the request stays
// header-free, the budget and the 1 MB cap come from the same client.
func fetchHealth(c *Client, baseURL string) string {
	resp, err := c.HTTPClient.Get(baseURL + "/health")
	if err != nil {
		return failedHealthState(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.readLimit()))
	if err != nil {
		return failedHealthState(err)
	}
	var h struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &h) != nil {
		return healthError
	}
	return h.Status
}

// failedHealthState maps a failed health call to its state: out of the
// budget (the client's timeout, while connecting or while reading the body)
// is no answer; everything else — refused, unresolvable, reset — is offline.
func failedHealthState(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return healthTimeout
	}
	return healthError
}

// ── command ─────────────────────────────────────────────────────────.

func statuslineCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "statusline",
		Short: "Claude Code statusline (reads JSON from stdin)",
		Long: `Claude Code statusline — renders a rich status bar with context store health,
block count, smooth context window progress bar, rate limits, session cost,
and model name.

SETUP (add to ~/.claude/settings.json):

  {
    "statusLine": {
      "type": "command",
      "command": "ctx statusline"
    }
  }

REQUIRED CONFIG (~/.config/ctx/config or %APPDATA%\ctx\config on Windows):

  CTX_BASE_URL=https://your-ctx-host.example
  CTX_KEY=your-api-key-here

  CTX_BASE_URL must point to the ctx API root (reverse proxy or direct).
  CTX_KEY is the API key from the context_api_keys table in your context_store DB.

OPTIONAL (add to ~/.claude/settings.json to disable autocompact):

  {
    "environmentVariables": {
      "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "0"
    }
  }

  When autocompact is disabled, the progress bar shows raw context usage.
  When active, it normalizes against the 83.5% usable range.

DISPLAY COMPONENTS:

  project 421●▕██▋17%          ▏ 5h:92% │ 7d:41% │ $8.14 │ Opus
  │       │  │└─ smooth progress bar (128 resolution steps, embedded %)
  │       │  └── health indicator (hidden when ok; U+EA6C degraded, U+F252 no
  │       │      answer within the call budget, U+F071 unhealthy or
  │       │      offline)
  │       └───── block count with document icon (nerdfont U+F09EE), shown
  │              whenever the stats call returned one
  └───────────── project directory name (from workspace.project_dir)

  Progress bar colors: green <50%, yellow <65%, orange <80%, red ≥80%
  Rate limits show REMAINING percentage, color-coded by usage.
  Requires Nerd Fonts for optimal icon rendering.

PROTOCOL:

  Reads Claude Code JSON from stdin (context_window, rate_limits, model, etc).
  Reads ctx /health and /api/manage stats (call budget ` + statuslineCallBudget.String() + `), at most
  once every ` + statuslineCacheTTL.String() + ` per server and key: the result is kept in a private cache file
  (statusline-<digest>.json under $XDG_CACHE_HOME/ctx, %LOCALAPPDATA%\ctx on
  Windows, else ~/.cache/ctx; the digest stands for server and key, neither
  is stored in clear). Outputs ANSI-colored text to stdout. Silent on errors.`,
		Example: `  # Test with mock data:
  echo '{"model":{"display_name":"Opus"},"context_window":{"remaining_percentage":70}}' | ctx statusline

  # Verify health + block count:
  echo '{}' | ctx statusline`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Read JSON from stdin with timeout
			inputCh := make(chan []byte, 1)
			go func() {
				data, _ := io.ReadAll(io.LimitReader(os.Stdin, maxStatuslineResponse))
				inputCh <- data
			}()

			var rawInput []byte
			select {
			case rawInput = <-inputCh:
			case <-time.After(3 * time.Second):
				return nil // Silent exit on timeout
			}

			var input statusInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil //nolint:nilerr // silent fail, statusline must not crash
			}

			// Load config (best effort — statusline should not crash)
			cfg, cfgErr := clientconfig.Load()

			// Fetch backend data (cached for statuslineCacheTTL, statuslineCallBudget per call)
			var backend *statusCache
			if cfgErr == nil {
				backend = fetchBackendData(cfg)
			}

			// ── Build output segments ───────────────────────────

			var segments []string

			// 1. project name + health dot + block count + context bar
			dirName := "ctx"
			if input.Workspace != nil {
				if input.Workspace.ProjectDir != "" {
					dirName = filepath.Base(input.Workspace.ProjectDir)
				} else if input.Workspace.CurrentDir != "" {
					dirName = filepath.Base(input.Workspace.CurrentDir)
				}
			}
			ctxPart := ansiCyan + dirName + ansiReset
			ctxPart += backendSegment(backend)

			if input.ContextWindow != nil && input.ContextWindow.RemainingPercentage != nil {
				remaining := *input.ContextWindow.RemainingPercentage

				// If autocompact is active, normalize against usable range (83.5%)
				autocompactOff := os.Getenv("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE") == "0"
				var used float64
				if autocompactOff {
					used = 100 - remaining
				} else {
					const autoCompactBuffer = 16.5
					usableRemaining := (remaining - autoCompactBuffer) / (100 - autoCompactBuffer) * 100
					used = 100 - usableRemaining
				}
				used = math.Max(0, math.Min(100, used))

				bar := smoothBar(16, used)
				color := barColor(used)
				ctxPart += fmt.Sprintf("%s%s%s", color, bar, ansiReset)
			}
			// Append 5h rate limit directly to bar (no separator)
			if input.RateLimits != nil && input.RateLimits.FiveHour != nil {
				fh := input.RateLimits.FiveHour
				color := rateColor(fh.UsedPercentage)
				remaining := 100 - fh.UsedPercentage
				ctxPart += fmt.Sprintf(" %s5h:%.0f%%%s", color, remaining, ansiReset)
			}
			segments = append(segments, ctxPart)

			// 3. Rate limits (7d only — 5h is appended to bar above)
			if input.RateLimits != nil {
				if sd := input.RateLimits.SevenDay; sd != nil {
					color := rateColor(sd.UsedPercentage)
					remaining := 100 - sd.UsedPercentage
					segments = append(segments, fmt.Sprintf("%s7d:%.0f%%%s", color, remaining, ansiReset))
				}
			}

			// 4. Cost
			if input.Cost != nil && input.Cost.TotalCostUSD > 0 {
				segments = append(segments, fmt.Sprintf("%s$%.2f%s", ansiDim, input.Cost.TotalCostUSD, ansiReset))
			}

			// 5. Model (dim, at the end)
			model := "Claude"
			if input.Model != nil && input.Model.DisplayName != "" {
				model = input.Model.DisplayName
			}
			segments = append(segments, ansiDim+model+ansiReset)

			fmt.Print(strings.Join(segments, " │ "))
			return nil
		},
	}
}
