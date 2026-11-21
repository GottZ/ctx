// issues_core_prosa_test.go — the standing guard for the T03-10 cut
// (design/03-oberflaeche.md §5.3, §4.10). issues_core.go holds ONE issue write
// logic and returns typed sentinels; the two mappers stay in their transports
// BECAUSE they word the same class differently. Both halves of that sentence
// need a witness, or the next refactor unifies the prose and nothing goes red:
//
//   - TestIssueErrorProsaPerTransport measures the ANSWER (status + body bytes
//     on REST/manage, Content[0].Text on MCP) for every sentinel the mappers
//     translate, including the two classes design/03 §5.3 names explicitly.
//     Pulling either mapper into the core, or unifying one message, turns it red.
//   - TestIssuesCoreCarriesNoTransportTypes pins the §4.10 doctrine — the core
//     names no transport type — so "we moved the code" cannot pass as "we built
//     a core".
//
// Neither test needs a database: the mappers are pure functions of one error,
// which is exactly why the prose is measurable without a fixture.
package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

func TestIssueErrorProsaPerTransport(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		restCode int
		restBody string
		mcpText  string
	}{
		{
			// The core's registry-nil sentinel (issues_core.go). Before T03-10 the
			// three transports wrote these two answers inline; the mappers now
			// render them, byte for byte the same.
			name: "registry unavailable", err: errIssueRegistryUnavailable,
			restCode: http.StatusServiceUnavailable,
			restBody: `{"error":"type registry unavailable","success":false}`,
			mcpText:  "type registry unavailable",
		},
		{
			name: "issue not found", err: store.ErrIssueNotFound,
			restCode: http.StatusNotFound,
			restBody: `{"error":"not found","success":false}`,
			mcpText:  "not found",
		},
		{
			// Same uniform no-oracle answer as not-found, on purpose: a foreign
			// block must not be distinguishable from an absent one.
			name: "link scope violation", err: store.ErrLinkScopeViolation,
			restCode: http.StatusNotFound,
			restBody: `{"error":"not found","success":false}`,
			mcpText:  "not found",
		},
		{
			name: "scope not writable", err: store.ErrIssueScope,
			restCode: http.StatusForbidden,
			restBody: `{"error":"scope not writable","success":false}`,
			mcpText:  "scope not writable",
		},
		{
			// DIVERGENCE 1 (design/03 §5.3): one class, two wordings — the manage
			// payload carries parent_id, the MCP tool carries issue_id.
			name: "comment without a parent", err: store.ErrCommentParentRequired,
			restCode: http.StatusBadRequest,
			restBody: `{"error":"comment requires a parent_id","success":false}`,
			mcpText:  "comment requires an issue_id",
		},
		{
			name: "body cap", err: store.ErrIssueBody,
			restCode: http.StatusUnprocessableEntity,
			restBody: `{"error":"body exceeds 50 KB cap","success":false}`,
			mcpText:  "body exceeds 50 KB cap",
		},
		{
			// The one issue class that carries a machine code on BOTH wires (I7/S3b).
			name: "reserved metadata", err: store.ErrReservedMetadata,
			restCode: http.StatusForbidden,
			restBody: `{"code":"reserved_metadata","error":"` + store.ErrReservedMetadata.Error() + `","success":false}`,
			mcpText:  store.ErrReservedMetadata.Error(),
		},
		{
			name: "invalid transition", err: blocktype.ErrInvalidTransition,
			restCode: http.StatusUnprocessableEntity,
			restBody: `{"error":"` + blocktype.ErrInvalidTransition.Error() + `","success":false}`,
			mcpText:  blocktype.ErrInvalidTransition.Error(),
		},
		{
			name: "no workflow", err: blocktype.ErrNoWorkflow,
			restCode: http.StatusUnprocessableEntity,
			restBody: `{"error":"` + blocktype.ErrNoWorkflow.Error() + `","success":false}`,
			mcpText:  blocktype.ErrNoWorkflow.Error(),
		},
		{
			// DIVERGENCE 2 (design/03 §5.3): the collecting branch. REST/manage
			// answer the house 500 envelope, MCP a tool-shaped sentence.
			name: "unmapped failure", err: errors.New("some store failure"),
			restCode: http.StatusInternalServerError,
			restBody: `{"error":"Internal server error","success":false}`,
			mcpText:  "issue write failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeIssueStoreError(rec, "issue-create", tc.err, "req-pin")
			if rec.Code != tc.restCode {
				t.Errorf("REST/manage status = %d, want %d", rec.Code, tc.restCode)
			}
			if got := strings.TrimRight(rec.Body.String(), "\n"); got != tc.restBody {
				t.Errorf("REST/manage body\n got %s\nwant %s", got, tc.restBody)
			}

			res := mcpIssueError(context.Background(), "issue_create", tc.err)
			if !res.IsError {
				t.Errorf("MCP result is not marked as an error")
			}
			if got := mcpText(res); got != tc.mcpText {
				t.Errorf("MCP text\n got %q\nwant %q", got, tc.mcpText)
			}
		})
	}

	// The point of keeping two mappers: for these two classes the transports MUST
	// disagree. A core that renders one message per class would make them equal.
	for _, err := range []error{store.ErrCommentParentRequired, errors.New("some store failure")} {
		rec := httptest.NewRecorder()
		writeIssueStoreError(rec, "issue-create", err, "req-pin")
		rest := strings.TrimRight(rec.Body.String(), "\n")
		mcp := mcpText(mcpIssueError(context.Background(), "issue_create", err))
		if strings.Contains(rest, `"error":"`+mcp+`"`) {
			t.Errorf("transports converged on one wording for %v: %s", err, rest)
		}
	}
}

func TestIssuesCoreCarriesNoTransportTypes(t *testing.T) {
	src, err := os.ReadFile("issues_core.go")
	if err != nil {
		t.Fatalf("read issues_core.go: %v", err)
	}
	// design/03 §4.10: the core is a core because no signature in it names a
	// transport type. Prose counts too — a doc comment that spells the type out
	// would make the wave's grep gate unreadable.
	for _, forbidden := range []string{"http.ResponseWriter", "mcp.CallToolResult"} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("issues_core.go names the transport type %q — the core must not know it", forbidden)
		}
	}
}
