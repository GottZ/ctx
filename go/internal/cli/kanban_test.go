package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

const testKanbanUUID = "11111111-1111-1111-1111-111111111111"

// newKanbanRoot builds a minimal `ctx kanban` cobra tree whose client points at
// srv, so a full command line exercises the real tree (Args, flags) — not a
// bypass to runKanban. kanban is a top-level command (design §4.7, E5), so it
// mounts on the root directly, unlike the `issues` family under `project`.
func newKanbanRoot(srv *httptest.Server) *cobra.Command {
	getClient := func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}
	root := &cobra.Command{Use: "ctx", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(kanbanCmd(getClient))
	return root
}

// fixtureBoardModel is the fixed board the View() golden pins.
func fixtureBoardModel() kanbanBoard {
	return kanbanBoard{Columns: []kanbanColumn{
		{Status: "open", Count: 3, Issues: []kanbanCard{
			{ID: "11111111-1111-1111-1111-111111111111", Title: "Login broken"},
			{ID: "22222222-2222-2222-2222-222222222222", Title: "Timeout on save"},
		}},
		{Status: "closed", Count: 0, Issues: []kanbanCard{}},
	}}
}

// ── TUI View() golden (§7-W10 gate: non-pty, box-drawing) ──────────────────────.

// TestKanbanViewGolden pins the bubbletea model's View() byte-for-byte against a
// testdata golden. This is THE W10-critical gate: the render is verified through
// the View() string in a non-pty Go test, NOT through a pty capture — a pty
// elides the Unicode box-drawing chars (│ ─ ╭╮╰╯) that make up the whole board,
// so a pty render probe would be structurally blind. The model is built with the
// Ascii (color-free) lipgloss profile so the golden is a deterministic
// box-drawing string with NO SGR escapes.
//
// RED PROOF: change any glyph/width/marker in renderColumn or View (e.g. the "▸"
// caret, the column width, the "+N more" tail) and the golden mismatches.
func TestKanbanViewGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/kanban_view.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got := newAsciiBoardModel(fixtureBoardModel()).View()
	if got != string(want) {
		t.Errorf("View() golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// ── pipe JSON golden (§7-W10 gate: stable schema, non-TTY ⇒ JSON) ──────────────.

// TestKanbanPipeGolden freezes the piped board output byte-for-byte. It ALSO
// proves the TTY-detection branch: stdout under `go test` is not a TTY, so
// `ctx kanban` takes the JSON path (never a headless TUI render). The served body
// is the FROZEN wire fixture (go/web/.../board.json, pinned by
// TestContractFreezeGolden) — the pipe path forwards it verbatim (one data path,
// no second aggregation).
//
// RED PROOF: rename a field in the served fixture (e.g. workflow_status →
// status) and the hardcoded `want` no longer matches — the golden reddens on
// wire-field drift.
func TestKanbanPipeGolden(t *testing.T) {
	body, err := os.ReadFile("../../web/src/lib/api/__fixtures__/board.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	const want = `{
  "columns": [
    {
      "count": 2,
      "cursor": null,
      "issues": [
        {
          "id": "11111111-1111-1111-1111-111111111111",
          "scope": "acme:main",
          "title": "Example issue",
          "type_name": "issue",
          "updated_at": "2026-07-03T00:00:00Z",
          "workflow_status": "open"
        }
      ],
      "status": "open"
    },
    {
      "count": 0,
      "cursor": null,
      "issues": [],
      "status": "closed"
    }
  ],
  "render": "untrusted",
  "success": true
}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/board") {
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(bytes.TrimSpace(body))
	}))
	defer srv.Close()

	root := newKanbanRoot(srv)
	root.SetArgs([]string{"kanban", "--project", testKanbanUUID})

	var execErr error
	got := captureStdout(t, func() { execErr = root.Execute() })
	if execErr != nil {
		t.Fatalf("kanban pipe: %v", execErr)
	}
	if got != want {
		t.Errorf("pipe golden mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// ── board parity: one wire, both renderers, same per-column card count ──────────.

// TestKanbanBoardParity proves the TUI model and the pipe path eat the SAME wire
// struct: one fixture is decoded once, and the per-column card count seen by the
// model equals the count decoded independently from the wire (no second
// aggregation). It also asserts every loaded card's short id appears in the
// rendered View — the model renders exactly the cards the wire delivered.
func TestKanbanBoardParity(t *testing.T) {
	body, err := os.ReadFile("../../web/src/lib/api/__fixtures__/board.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	board, err := parseBoard(body)
	if err != nil {
		t.Fatalf("parseBoard: %v", err)
	}
	// Independent decode of the same wire (the "pipe" view of the data).
	var wire struct {
		Columns []struct {
			Status string `json:"status"`
			Issues []struct {
				ID string `json:"id"`
			} `json:"issues"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("wire decode: %v", err)
	}
	if len(board.Columns) != len(wire.Columns) {
		t.Fatalf("column count: model %d, wire %d", len(board.Columns), len(wire.Columns))
	}
	view := newAsciiBoardModel(board).View()
	for i, col := range board.Columns {
		if len(col.Issues) != len(wire.Columns[i].Issues) {
			t.Errorf("column %q card count: model %d, wire %d",
				col.Status, len(col.Issues), len(wire.Columns[i].Issues))
		}
		for _, card := range col.Issues {
			if !strings.Contains(view, shortID(card.ID)) {
				t.Errorf("card %s missing from View()", shortID(card.ID))
			}
		}
	}
}

// ── escape safety (§5.4 / §7-W10 gate) ─────────────────────────────────────────.

// TestKanbanViewEscapeSafety feeds an attacker-controlled title carrying ESC, the
// C1 CSI single byte 0x9b, an OSC-title hijack (ESC ] … BEL) and a \r line-
// overwrite, then asserts the rendered View() carries NONE of those control
// bytes — sanitizeTerminal neutralizes every card title before it enters a
// lipgloss cell. The benign text survives.
//
// RED PROOF: drop the sanitizeTerminal call in renderColumn and the raw ESC /
// 0x9b / BEL / \r flow straight into View() — every assertion below reddens.
func TestKanbanViewEscapeSafety(t *testing.T) {
	board := kanbanBoard{Columns: []kanbanColumn{{
		Status: "open", Count: 1, Issues: []kanbanCard{
			{ID: "33333333-3333-3333-3333-333333333333", Title: "safe\x1b]0;pwned\x07x\x9by\rz"},
		},
	}}}
	view := newAsciiBoardModel(board).View()
	for _, bad := range []string{"\x1b", "\x9b", "\x07", "\r", "\x90", "\x9d", "\x7f"} {
		if strings.Contains(view, bad) {
			t.Errorf("View() leaked control byte %q from an attacker title", bad)
		}
	}
	if !strings.Contains(view, "safe") {
		t.Error("View() dropped the benign part of the title")
	}
}

// TestKanbanPipeEscapeSafety proves the pipe (machine) contract stays escaped:
// the server already JSON-escapes controls (\u001b), and the CLI forwards the
// bytes verbatim (PrintJSON re-indents, never un-escapes). So a control-bearing
// title reaches the pipe as \u001b, never as a raw ESC byte a downstream TTY
// would execute.
//
// RED PROOF: if the pipe path decoded-and-reprinted the title (instead of
// forwarding bytes), the \u001b would become a raw 0x1b and the first assertion
// would redden.
func TestKanbanPipeEscapeSafety(t *testing.T) {
	const serverBody = `{"success":true,"render":"untrusted","columns":[{"status":"open","count":1,"cursor":null,"issues":[{"id":"33333333-3333-3333-3333-333333333333","scope":"acme:main","type_name":"issue","title":"safe\u001b]0;pwned\u0007","workflow_status":"open","updated_at":"2026-07-03T00:00:00Z"}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(serverBody))
	}))
	defer srv.Close()

	root := newKanbanRoot(srv)
	root.SetArgs([]string{"kanban", "--project", testKanbanUUID})
	var execErr error
	got := captureStdout(t, func() { execErr = root.Execute() })
	if execErr != nil {
		t.Fatalf("kanban pipe: %v", execErr)
	}
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("pipe output leaked a raw control byte: %q", got)
	}
	if !strings.Contains(got, `\u001b`) {
		t.Errorf("pipe output should keep the JSON-escaped ESC, got: %q", got)
	}
}

// ── exit 1 on success:false (envelope contract, §4.7) ──────────────────────────.

// TestKanbanExitOnFailure asserts a foreign/unknown project (404 with a
// {success:false} envelope) reaches stderr as exit 1 — the board command does
// not inherit the PrintJSON-and-exit-0 trap.
func TestKanbanExitOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"error":"not found"}`))
	}))
	defer srv.Close()

	root := newKanbanRoot(srv)
	root.SetArgs([]string{"kanban", "--project", testKanbanUUID})
	err := root.Execute()
	if err == nil {
		t.Fatal("success:false ⇒ want a non-nil error (exit 1), got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want the server reason", err)
	}
}

// ── model navigation (read-only; no mutation path exists) ───────────────────────.

// runeKey / namedKey build the tea.KeyMsg values Update matches on via String().
func runeKey(r rune) tea.KeyMsg         { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func namedKey(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

// TestKanbanNavigation drives the Update loop: right moves the focus, down clamps
// in a single-card column, and the caret follows the selection in the focused
// column. It also confirms the ONLY non-navigation effect Update has is quit —
// there is no mutation path in v1.
func TestKanbanNavigation(t *testing.T) {
	start := newAsciiBoardModel(kanbanBoard{Columns: []kanbanColumn{
		{Status: "open", Count: 2, Issues: []kanbanCard{
			{ID: "aaaaaaaa-0000-0000-0000-000000000000", Title: "a"},
			{ID: "bbbbbbbb-0000-0000-0000-000000000000", Title: "b"},
		}},
		{Status: "closed", Count: 1, Issues: []kanbanCard{
			{ID: "cccccccc-0000-0000-0000-000000000000", Title: "c"},
		}},
	}})
	m := start
	step := func(msg tea.KeyMsg) {
		nm, _ := m.Update(msg)
		m = nm.(boardModel)
	}
	step(namedKey(tea.KeyRight))
	if m.focused != 1 {
		t.Fatalf("after right: focused = %d, want 1", m.focused)
	}
	step(namedKey(tea.KeyDown)) // closed has one card only ⇒ clamped
	if m.sel[1] != 0 {
		t.Fatalf("after down in single-card column: sel = %d, want 0 (clamped)", m.sel[1])
	}
	step(runeKey('h'))
	step(runeKey('j'))
	if m.sel[0] != 1 {
		t.Fatalf("after down in open: sel = %d, want 1", m.sel[0])
	}
	if !strings.Contains(m.View(), "▸ bbbbbbbb") {
		t.Errorf("caret did not follow to the second card of the focused column")
	}
	if _, cmd := m.Update(runeKey('q')); cmd == nil {
		t.Error("q should return a quit command")
	}
}
