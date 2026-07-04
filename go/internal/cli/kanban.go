// ctx kanban — the project board view (workflow W10, design/03-workflow-api-cli.md
// §7-W10, E4a). It rides the ONE board wire the UI eats (GET /api/project/{id}/board,
// W6): per-status columns, each with an index-only total count, a first page of
// cards (board order, updated_at DESC) and a per-column resume cursor. There is
// NO second aggregation path in the client — the CLI consumes the same response
// bytes the SPA does.
//
//	ctx kanban [--project ID] [--limit N]
//
// Two paths, one fetch:
//   - TTY  ⇒ an interactive bubbletea/lipgloss board (read-only in v1: navigate
//     columns/cards, no mutations). E4 decision (a): bubbletea over hand-rolled.
//   - pipe ⇒ the raw server board JSON (stable, golden-pinned) so it stays
//     scriptable; a script pages a hot column via the per-column cursor with
//     `ctx project issues list --state <col> --after <cursor>`.
//
// SCALE: a column may hold 10k+ issues. The board wire returns only a first page
// per column plus a total count and an opaque resume cursor — the CLI never loads
// a whole column. The TUI shows the page and a "+N more" tail; the pipe path
// forwards the cursor for keyset paging. The page size is the server default
// (WorkflowListLimit); --limit overrides it per column.
//
// SECURITY (§5.4): issue TITLES and workflow STATUS strings are attacker-controlled
// (any GitHub user can open an issue in a mirrored repo). Every value rendered to
// the terminal runs through sanitizeTerminal (allowlist, sanitize.go) before it
// enters a lipgloss cell, so a crafted title cannot drive escape sequences into
// the terminal / agent log / CI output. The pipe (JSON) path is the machine
// contract: the server already JSON-escapes C0 controls, and PrintJSON forwards
// it verbatim (rewriting would break the golden).

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
)

// ── wire shape (mirrors the W6 /board response; ONE struct, both renderers) ─────.

// kanbanBoard mirrors the board wire (handler.HandleBoard → store.IssueBoardColumn).
// It is the single decode target: the TUI model and the parity test both eat this,
// and the pipe path forwards the same bytes verbatim.
type kanbanBoard struct {
	Columns []kanbanColumn `json:"columns"`
}

// kanbanColumn is one workflow-status column: the config status, the total live
// count in that status, the first page of cards and the opaque per-column resume
// cursor (nil = the column fits on the page).
type kanbanColumn struct {
	Status string       `json:"status"`
	Count  int          `json:"count"`
	Issues []kanbanCard `json:"issues"`
	Cursor *string      `json:"cursor"`
}

// kanbanCard mirrors store.WorkflowBlockRow — the minimal board row (the detail
// view lives behind `ctx project issues show`). Only ID/Title/WorkflowStatus are
// rendered; the rest ride along for wire parity with the list endpoint.
type kanbanCard struct {
	ID             string `json:"id"`
	Scope          string `json:"scope"`
	TypeName       string `json:"type_name"`
	Title          string `json:"title"`
	WorkflowStatus string `json:"workflow_status"`
	UpdatedAt      string `json:"updated_at"`
}

// parseBoard decodes the board wire into the single model struct.
func parseBoard(resp []byte) (kanbanBoard, error) {
	var b kanbanBoard
	if err := json.Unmarshal(resp, &b); err != nil {
		return kanbanBoard{}, fmt.Errorf("board response: %w", err)
	}
	return b, nil
}

// ── command ─────────────────────────────────────────────────────────────────.

func kanbanCmd(getClient func() (*Client, error)) *cobra.Command {
	var project string
	var limit int
	cmd := &cobra.Command{
		Use:   "kanban",
		Short: "Show a project's issue board (interactive on a TTY, JSON when piped)",
		Long: "Render a project's workflow-status board. The columns come from the type's\n" +
			"status config (never hardcoded); each shows a total count and a first page of\n" +
			"cards. The project is detected from the current repo (like `ctx project`) or\n" +
			"named with --project (a project id or an identity). On a TTY this is an\n" +
			"interactive, read-only board (arrows / hjkl to navigate, q to quit); piped, it\n" +
			"is the raw server JSON. A column can hold 10k+ issues, so only a first page is\n" +
			"loaded per column (--limit sets the page size, default = server default); page\n" +
			"a hot column via its cursor with `ctx project issues list --state <col> --after`.",
		Example: `  ctx kanban
  ctx kanban --project github:acme/widget
  ctx kanban | jq '.columns[] | {status, count}'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKanban(getClient, project, limit)
		},
	}
	cmd.Flags().StringVar(&project, "project", "",
		"project override: a project id (UUID) or an identity (github:o/r | git-root:sha | manual:slug); default = detect in the current repo")
	cmd.Flags().IntVar(&limit, "limit", 0, "per-column page size (server default when 0)")
	return cmd
}

// runKanban makes the ONE board fetch, then forks: pipe ⇒ raw JSON, TTY ⇒ the
// interactive bubbletea board. Both eat the same response.
func runKanban(getClient func() (*Client, error), project string, limit int) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	pid, err := resolveProjectID(c, project)
	if err != nil {
		return err
	}
	path := "/api/project/" + pid + "/board"
	if limit > 0 {
		qv := url.Values{}
		qv.Set("limit", fmt.Sprintf("%d", limit))
		path += "?" + qv.Encode()
	}
	resp, _, err := c.Do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	// Pipe / non-TTY: the machine contract — forward the server board verbatim
	// (pty NOTE §7-W10: a pty would elide the TUI's box-drawing chars, so the
	// non-TTY branch is the JSON path, never a headless render).
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	board, err := parseBoard(resp)
	if err != nil {
		return err
	}
	m := newBoardModel(board, lipgloss.DefaultRenderer())
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// ── bubbletea model (View() is golden-tested non-pty; §7-W10) ──────────────────.

const (
	// colContentWidth is the lipgloss content-box width of a column (text +
	// horizontal padding), fixed so View() is deterministic (golden) regardless
	// of the terminal size. The rounded border adds one column on each side.
	colContentWidth = 30
	// colTextWidth is the text area after the 1-column horizontal padding on each
	// side — the width the separator rule and card lines are sized/truncated to,
	// so nothing wraps inside the box.
	colTextWidth = colContentWidth - 2
)

// boardModel is the read-only kanban TUI state: the decoded board, the focused
// column, and a per-column selected-card index. A lipgloss renderer is injected
// so the View() golden test can pin an Ascii (color-free) profile while the
// interactive program uses the auto-detected terminal profile.
type boardModel struct {
	board    kanbanBoard
	renderer *lipgloss.Renderer
	focused  int   // focused column index
	sel      []int // selected card index, per column
}

// newBoardModel builds the model from a decoded board and a renderer. The
// per-column selection starts at the first card of each column.
func newBoardModel(board kanbanBoard, r *lipgloss.Renderer) boardModel {
	return boardModel{
		board:    board,
		renderer: r,
		sel:      make([]int, len(board.Columns)),
	}
}

// newAsciiBoardModel is the test constructor: it forces the color-free (Ascii)
// profile so View() is a deterministic box-drawing string with NO SGR escapes —
// which is exactly why a pty probe would be blind (it elides box-drawing chars)
// and the View() golden is not (§7-W10).
func newAsciiBoardModel(board kanbanBoard) boardModel {
	r := lipgloss.NewRenderer(nil)
	r.SetColorProfile(termenv.Ascii)
	return newBoardModel(board, r)
}

func (m boardModel) Init() tea.Cmd { return nil }

// Update handles navigation and quit only — v1 is strictly read-only (no status
// mutations). Left/right (h/l) move between columns; up/down (j/k) move the
// selection within the focused column; q / esc / ctrl+c quit.
func (m boardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "left", "h":
		if m.focused > 0 {
			m.focused--
		}
	case "right", "l":
		if m.focused < len(m.board.Columns)-1 {
			m.focused++
		}
	case "up", "k":
		if m.sel[m.focused] > 0 {
			m.sel[m.focused]--
		}
	case "down", "j":
		if n := len(m.board.Columns[m.focused].Issues); n > 0 && m.sel[m.focused] < n-1 {
			m.sel[m.focused]++
		}
	}
	return m, nil
}

// View renders every column as a bordered lipgloss box joined horizontally. Card
// titles and status strings are sanitized (allowlist) before they enter a cell,
// so an attacker-controlled title cannot inject escape sequences. The focused
// column's selected card carries a "▸" caret (a text marker, not color, so focus
// is legible even under the Ascii profile).
func (m boardModel) View() string {
	if len(m.board.Columns) == 0 {
		return "No board columns.\n"
	}
	boxes := make([]string, 0, len(m.board.Columns))
	for ci, col := range m.board.Columns {
		boxes = append(boxes, m.renderColumn(ci, col))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, boxes...)
	return body + "\n\narrows/hjkl: navigate · q: quit\n"
}

// renderColumn renders one column box: a header (status + count), its cards, and
// a "+N more" tail when the total count exceeds the loaded page.
func (m boardModel) renderColumn(ci int, col kanbanColumn) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("%s (%d)", sanitizeTerminal(col.Status), col.Count))
	lines = append(lines, strings.Repeat("─", colTextWidth))
	if len(col.Issues) == 0 {
		lines = append(lines, "(empty)")
	}
	for ii, card := range col.Issues {
		marker := "  "
		if ci == m.focused && ii == m.sel[ci] {
			marker = "▸ "
		}
		title := sanitizeTerminal(card.Title)
		text := truncateCell(marker+shortID(card.ID)+" "+title, colTextWidth)
		lines = append(lines, text)
	}
	if more := col.Count - len(col.Issues); more > 0 {
		lines = append(lines, fmt.Sprintf("+%d more", more))
	}
	style := m.renderer.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(colContentWidth)
	return style.Render(strings.Join(lines, "\n"))
}

// truncateCell clamps a line to w runes with a single-char ellipsis so a long
// title never overflows the column (rune-safe, not byte-safe).
func truncateCell(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}
