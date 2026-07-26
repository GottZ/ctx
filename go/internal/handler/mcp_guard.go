package handler

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GottZ/ctx/internal/store"
)

// MCP guard tools (needs_review pipeline W3): the review queue becomes
// workable for agents — list the flagged blocks (with the matched partner and
// similarity the Guard persisted) and resolve them, singly or in batch. Both
// tools are strict REST parity: guard_list mirrors manage guard-list,
// guard_resolve mirrors manage guard-resolve incl. the ids[] batch contract
// (every id accounted for: resolved XOR skipped+reason).

type guardListInput struct {
	Status   string   `json:"status,omitempty" jsonschema:"filter by guard status (needs_review, near_duplicate, possible_duplicate); empty = all flagged"`
	Category string   `json:"category,omitempty" jsonschema:"filter by category"`
	Types    []string `json:"types,omitempty" jsonschema:"only these block types (e.g. knowledge, checkpoint)"`
	Limit    int      `json:"limit,omitempty" jsonschema:"max results (default 50, server cap 200)"`
}

type guardResolveInput struct {
	IDs        []string `json:"ids" jsonschema:"block ids to resolve (1..500)"`
	Resolution string   `json:"resolution" jsonschema:"'keep' (clear the flag) or 'archive' (archive as duplicate)"`
}

const guardListDesc = "List blocks the write guard flagged for duplicate review. " +
	"Each entry carries the matched partner block and the similarity score, ordered most-similar first."

const guardResolveDesc = "Resolve flagged blocks: 'keep' clears the flag, 'archive' archives the block as a duplicate. " +
	"Accepts one or many ids (batch cap 500); every id is accounted for as resolved or skipped with a reason. " +
	"Keys with the confirm_writes capability cannot archive via this tool (not stageable) — they get keep only."

// registerGuardTools adds the two W3 guard review tools. Called from
// registerTools (mcp.go). Own function so the tool set is one grep (the
// registerIssueTools convention).
func registerGuardTools(server *mcp.Server, cfg MCPConfig) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "guard_list",
		Description: guardListDesc,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpGuardListHandler(cfg))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "guard_resolve",
		Description: guardResolveDesc,
	}, mcpGuardResolveHandler(cfg))
}

func mcpGuardListHandler(cfg MCPConfig) mcp.ToolHandlerFor[guardListInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input guardListInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed: never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		limit := input.Limit
		if limit <= 0 {
			limit = 50
		}
		items, err := store.GuardList(ctx, cfg.Pool, ar.ReadScopes, input.Category, input.Status, input.Types, limit)
		if err != nil {
			slog.Error("mcp: guard_list error", "error", err)
			return errResult("guard_list failed: internal error"), nil, nil
		}
		// JSON payload, REST field parity (GuardListItem json tags).
		body, err := json.Marshal(map[string]any{
			"success": true,
			"count":   len(items),
			"blocks":  items,
		})
		if err != nil {
			return errResult("guard_list failed: internal error"), nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(string(body))},
		}, nil, nil
	}
}

func mcpGuardResolveHandler(cfg MCPConfig) mcp.ToolHandlerFor[guardResolveInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input guardResolveInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed: never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		if input.Resolution != "archive" && input.Resolution != "keep" {
			return errResult("resolution must be 'archive' or 'keep'"), nil, nil
		}
		if len(input.IDs) == 0 {
			return errResult("ids is required (1..500 block ids)"), nil, nil
		}
		// A confirm_writes key stages its store/update writes for human
		// confirmation. guard_resolve has no stage path, and archive is the
		// subtractive arm — so it is refused fail-closed instead of silently
		// bypassing the staging policy. keep (flag clear, additive posture,
		// the issue-tools line) executes directly.
		if ar.ConfirmWrites && input.Resolution == "archive" {
			return errResult("archive is not available for confirm_writes keys (not stageable); use keep, or resolve via the REST manage path"), nil, nil
		}

		resolved, skipped, err := store.GuardResolveBatch(ctx, cfg.Pool, input.IDs, input.Resolution, writableBlockScopes(ar))
		if err != nil {
			slog.Error("mcp: guard_resolve error", "error", err)
			return errResult("guard_resolve failed: internal error"), nil, nil
		}
		body, err := json.Marshal(map[string]any{
			"success":        true,
			"resolution":     input.Resolution,
			"resolved_count": len(resolved),
			"skipped_count":  len(skipped),
			"resolved":       resolved,
			"skipped":        skipped,
		})
		if err != nil {
			return errResult("guard_resolve failed: internal error"), nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(string(body))},
		}, nil, nil
	}
}
