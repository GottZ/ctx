package cli

import (
	"fmt"
	"strings"
)

// dim wraps text in ANSI dim unless NO_COLOR is set.
func dim(s string) string {
	if noColor() {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

// bold wraps text in ANSI bold unless NO_COLOR is set.
func bold(s string) string {
	if noColor() {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

// num coerces a JSON number (float64) or fallback to an int for display.
func num(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// flt coerces a JSON number to float64 (0 on miss).
func flt(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// fmtDuration renders an hours value compactly: <24h as "Nh", <10d as "N.Nd",
// else "Nd". Keeps the back-off table readable across the 12h→45d range.
func fmtDuration(hours float64) string {
	if hours < 24 {
		return fmt.Sprintf("%dh", int(hours+0.5))
	}
	days := hours / 24
	if days < 10 {
		return fmt.Sprintf("%.1fd", days)
	}
	return fmt.Sprintf("%dd", int(days+0.5))
}

// bar renders a proportional bar of width cols for value/max, reusing the
// statusline's eighth-block palette (barBlocks: ' ▏▎▍▌▋▊▉█') for sub-character
// resolution — partial blocks give smooth sub-steps instead of whole-cell jumps.
// A non-zero value always shows at least a one-eighth sliver; the bar is padded
// with spaces to exactly cols runes so columns stay aligned.
func bar(value, max, cols int) string {
	if cols <= 0 {
		return ""
	}
	eighths := 0
	if max > 0 && value > 0 {
		eighths = value * cols * 8 / max
		if eighths == 0 {
			eighths = 1 // guarantee a visible sliver for any non-zero bucket
		}
		if eighths > cols*8 {
			eighths = cols * 8
		}
	}
	var b strings.Builder
	full := eighths / 8
	for i := 0; i < full; i++ {
		b.WriteRune('█')
	}
	used := full
	if rem := eighths % 8; rem > 0 {
		b.WriteRune(barBlocks[rem]) // partial cell (barBlocks[1..7] = ▏▎▍▌▋▊▉)
		used++
	}
	for i := used; i < cols; i++ {
		b.WriteByte(' ') // pad to full width for column alignment
	}
	return b.String()
}

// renderDreamStatsHuman renders the dream-stats JSON map as a human-readable
// summary for interactive terminals. The merged map is the dream-stats response
// plus dream_mode/dream_interval grafted in by dreamStatsRun.
func renderDreamStatsHuman(d map[string]any) string {
	var sb strings.Builder

	mode, _ := d["dream_mode"].(string)
	if mode == "" {
		mode = "?"
	}
	fmt.Fprintf(&sb, "%s  mode=%s  interval=%vs\n",
		bold("Dream Mode"), mode, valOr(d["dream_interval"], "?"))

	// Coverage line.
	cov := 0.0
	if f, ok := d["coverage_pct"].(float64); ok {
		cov = f
	}
	fmt.Fprintf(&sb, "  blocks %d   checked %d (%.0f%%)   links %d   unchecked %d\n",
		num(d["total_blocks"]), num(d["dream_checked"]), cov,
		num(d["dream_links"]), num(d["unchecked"]))

	// Queue / backlog.
	if q, ok := d["queue"].(map[string]any); ok {
		fmt.Fprintf(&sb, "  queue: pickable %d   in-cooldown %d   incoming 1h/6h %d/%d\n",
			num(q["pickable_now"]), num(q["in_cooldown"]),
			num(q["incoming_1h"]), num(q["incoming_6h"]))
		if np, ok := q["next_pending_at"].(string); ok && np != "" {
			fmt.Fprintf(&sb, "  %s\n", dim("next pull at "+np))
		}
	}

	// Back-off policy + per-eval-count maturity distribution.
	if bo, ok := d["backoff"].(map[string]any); ok {
		fmt.Fprintf(&sb, "\n%s  mode=%v factor=%v min=%s grace=%v cap=%vd  %s\n",
			bold("Re-dream back-off"),
			valOr(bo["mode"], "?"), valOr(bo["factor"], "?"),
			fmtDuration(flt(bo["min_hours"])),
			valOr(bo["grace"], "?"), valOr(bo["cap_days"], "?"),
			dim(fmt.Sprintf("(cooldown by eval count; inert +%d)", num(bo["inert_offset"]))))

		levels, _ := bo["levels"].([]any)
		maxBlocks := 0
		for _, li := range levels {
			if l, ok := li.(map[string]any); ok {
				if n := num(l["blocks"]); n > maxBlocks {
					maxBlocks = n
				}
			}
		}
		for _, li := range levels {
			l, ok := li.(map[string]any)
			if !ok {
				continue
			}
			n := num(l["eval_count"])
			blocks := num(l["blocks"])
			ch := flt(l["cooldown_hours"])
			fmt.Fprintf(&sb, "  n=%-3d %4d  %-24s %s\n",
				n, blocks, bar(blocks, maxBlocks, 24),
				dim("\u2192 "+fmtDuration(ch)))
		}
		if t, _ := bo["truncated"].(bool); t {
			fmt.Fprintf(&sb, "  %s\n",
				dim(fmt.Sprintf("\u2026 list truncated (max eval_count %d)", num(bo["max_eval_count"]))))
		}
	}

	return sb.String()
}

// valOr returns v as-is for display, or fallback when nil/empty.
func valOr(v any, fallback string) any {
	if v == nil {
		return fallback
	}
	if s, ok := v.(string); ok && s == "" {
		return fallback
	}
	// JSON numbers arrive as float64; trim a trailing .0 for whole numbers.
	if f, ok := v.(float64); ok && f == float64(int(f)) {
		return int(f)
	}
	return v
}
