//go:build integration

package dream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
)

// Prompt-Regression Test Suite (Welle 46 S38, 2026-05-23)
//
// Zweck: Bei V6/V7-Prompt-Iterationen und/oder Welle 47+ Code-Änderungen
// im dream-Pipeline gegen einen kuratierten Korpus benchmarken. Die
// Fixture (.project/bench-session-38-armada/prompt-regression-fixtures.json)
// enthält 12 manuell validierte Cases mit Ground-Truth-Verdict.
//
// Modi:
//   - "baseline": laufe alle Cases gegen aktuelles Prompt → JSON-Output
//     mit current_rel + current_confidence pro Case.
//   - "regression": diff vs erwartete (gespeicherte) Baseline. FAIL wenn
//     Drift in unerwünschte Richtung (true_inverted → wrong, etc).
//
// Setup:
//   - PG verfügbar via CTX_DATABASE_URL (testdb.SetupTestDB() ist
//     NICHT geeignet — Fixture-Cases referenzieren echte production-IDs).
//   - Live-LLM via CTX_CHAT_HOST + CTX_CHAT_MODEL (Ollama oder kompatibel).
//   - Build-Tag: integration. Standard `go test -short` skippt.
//
// Env-Vars:
//   - CTX_REGRESSION_FIXTURE  (Path zur Fixture-JSON, optional)
//   - CTX_REGRESSION_DATABASE_URL  (PG-DSN, required oder t.Skip)
//   - CTX_REGRESSION_CHAT_HOST   (Ollama-URL, required oder t.Skip)
//   - CTX_REGRESSION_CHAT_MODEL  (Model name, default: qwen3.6:27b)
//   - CTX_REGRESSION_OUTPUT      (Path für Baseline-JSON-Output, default: stdout)

// FixtureCase mirrors the prompt-regression-fixtures.json schema.
type FixtureCase struct {
	ID                int     `json:"id"`
	SrcBlockID        string  `json:"src_block_id"`
	TgtBlockID        string  `json:"tgt_block_id"`
	LLMClassifiedRel  string  `json:"llm_classified_rel"`
	LLMRawConfidence  *float64 `json:"llm_raw_confidence"`
	ArmadaAgent       int     `json:"armada_agent"`
	ArmadaVerdict     string  `json:"armada_verdict"`
	ArmadaNote        string  `json:"armada_note"`
	ValidatorVerdict  string  `json:"validator_verdict"`
	ValidatorNote     string  `json:"validator_note"`
	Category          string  `json:"category"`
	IdealRelationship string  `json:"ideal_relationship"`
	IdealDirection    string  `json:"ideal_direction"`
	LessonForPrompt   string  `json:"lesson_for_prompt"`
}

type Fixture struct {
	SchemaVersion string        `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Cases         []FixtureCase `json:"cases"`
}

// RunResult holds the LLM output for one case in this run.
type RunResult struct {
	CaseID          int     `json:"case_id"`
	SrcBlockID      string  `json:"src_block_id"`
	TgtBlockID      string  `json:"tgt_block_id"`
	BaselineRel     string  `json:"baseline_rel"`     // from fixture (V5-original)
	CurrentRel      string  `json:"current_rel"`      // this-run LLM output
	CurrentConf     float64 `json:"current_confidence"`
	IdealRel        string  `json:"ideal_relationship"`
	Match           string  `json:"match"`            // "baseline" | "ideal" | "drifted" | "skipped" | "filtered"
	Drift           string  `json:"drift_summary"`
	Category        string  `json:"category"`
}

func locateFixture(t *testing.T) string {
	t.Helper()
	if env := os.Getenv("CTX_REGRESSION_FIXTURE"); env != "" {
		return env
	}
	candidates := []string{
		"/compose/n8n/.project/bench-session-38-armada/prompt-regression-fixtures.json",
		filepath.Join(os.Getenv("HOME"), ".project/bench-session-38-armada/prompt-regression-fixtures.json"),
		"../../../../.project/bench-session-38-armada/prompt-regression-fixtures.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func loadFixture(t *testing.T) Fixture {
	t.Helper()
	path := locateFixture(t)
	if path == "" {
		t.Skip("fixture not found — set CTX_REGRESSION_FIXTURE or check .project submodule")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var fx Fixture
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if fx.SchemaVersion != "1.0" {
		t.Fatalf("fixture schema_version mismatch: got %q want 1.0", fx.SchemaVersion)
	}
	return fx
}

func openProductionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CTX_REGRESSION_DATABASE_URL")
	if dsn == "" {
		t.Skip("CTX_REGRESSION_DATABASE_URL not set — regression test needs prod-like DB with fixture block IDs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fetchBlockInfo loads the BlockInfo fields dream.EvaluateRelationships needs.
// Returns nil if the block does not exist (fixture IDs may be stale).
func fetchBlockInfo(ctx context.Context, pool *pgxpool.Pool, id string) (*dream.BlockInfo, error) {
	if id == "" || id[0] == '?' {
		return nil, fmt.Errorf("invalid id: %q", id)
	}
	var b dream.BlockInfo
	err := pool.QueryRow(ctx, `
		SELECT id::text, title, category, content, scope,
		       COALESCE(quality_score, 0.5),
		       updated_at, created_at,
		       COALESCE(dream_keywords, ARRAY[]::text[]),
		       dream_temporal_validated_at
		FROM context_blocks
		WHERE id = $1::uuid AND NOT is_archived
	`, id).Scan(
		&b.ID, &b.Title, &b.Category, &b.Content, &b.Scope,
		&b.QualityScore, &b.UpdatedAt, &b.CreatedAt,
		&b.DreamKeywords, &b.DreamTemporalValidatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// TestPromptRegression_BaselineRun runs the current dream-evaluate prompt
// against every fixture case and emits a Result-JSON. Reviewer compares
// the JSON to the fixture's ideal_relationship + category to assess whether
// V6/V7 improved or regressed.
//
// No hard assertions — purely diagnostic. Use TestPromptRegression_AssertNoBackslide
// for CI-style hard gates against a saved baseline.
func TestPromptRegression_BaselineRun(t *testing.T) {
	fx := loadFixture(t)
	pool := openProductionPool(t)

	chatHost := os.Getenv("CTX_REGRESSION_CHAT_HOST")
	if chatHost == "" {
		t.Skip("CTX_REGRESSION_CHAT_HOST not set")
	}
	chatModel := os.Getenv("CTX_REGRESSION_CHAT_MODEL")
	if chatModel == "" {
		chatModel = "qwen3.6:27b"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	results := make([]RunResult, 0, len(fx.Cases))
	for _, c := range fx.Cases {
		r := RunResult{
			CaseID:      c.ID,
			SrcBlockID:  c.SrcBlockID,
			TgtBlockID:  c.TgtBlockID,
			BaselineRel: c.LLMClassifiedRel,
			IdealRel:    c.IdealRelationship,
			Category:    c.Category,
		}

		if c.Category == "unvalidated" {
			r.Match = "skipped"
			r.Drift = "case marked unvalidated in fixture"
			results = append(results, r)
			continue
		}

		src, err := fetchBlockInfo(ctx, pool, c.SrcBlockID)
		if err != nil {
			r.Match = "skipped"
			r.Drift = fmt.Sprintf("src fetch failed: %v", err)
			results = append(results, r)
			continue
		}
		tgt, err := fetchBlockInfo(ctx, pool, c.TgtBlockID)
		if err != nil {
			r.Match = "skipped"
			r.Drift = fmt.Sprintf("tgt fetch failed: %v", err)
			results = append(results, r)
			continue
		}

		links, err := dream.EvaluateRelationships(
			ctx, pool,
			testRouterFor(chatHost, chatModel, backends.ProtocolOllama),
			llm.Options{Temperature: 0.0, NumCtx: 8192, NumPredict: 400},
			*src, []dream.BlockInfo{*tgt},
		)
		if err != nil {
			r.Match = "skipped"
			r.Drift = fmt.Sprintf("evaluate error: %v", err)
			results = append(results, r)
			continue
		}

		if len(links) == 0 {
			r.CurrentRel = "(filtered)"
			r.Match = "filtered"
			r.Drift = "post-W46 filter dropped the link (acceptSupersedes/enforceSupersedesDirection)"
		} else {
			r.CurrentRel = links[0].Relationship
			r.CurrentConf = links[0].Confidence
			switch r.CurrentRel {
			case c.IdealRelationship:
				r.Match = "ideal"
				r.Drift = "matches validator's ideal_relationship"
			case c.LLMClassifiedRel:
				r.Match = "baseline"
				r.Drift = "matches V5-baseline classification (no drift)"
			default:
				r.Match = "drifted"
				r.Drift = fmt.Sprintf("V5=%s, V_now=%s, ideal=%s",
					c.LLMClassifiedRel, r.CurrentRel, c.IdealRelationship)
			}
		}
		results = append(results, r)
	}

	// Aggregate
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Match]++
	}
	t.Logf("PROMPT-REGRESSION BASELINE-RUN:")
	t.Logf("  fixture cases: %d", len(fx.Cases))
	t.Logf("  ideal:     %d (current rel == validator's ideal)", counts["ideal"])
	t.Logf("  baseline:  %d (matches V5 — no improvement, no regression)", counts["baseline"])
	t.Logf("  drifted:   %d (V_now differs from V5 AND from ideal)", counts["drifted"])
	t.Logf("  filtered:  %d (link dropped by filter chain)", counts["filtered"])
	t.Logf("  skipped:   %d (fetch/parse error)", counts["skipped"])

	// Output JSON for offline analysis
	output := os.Getenv("CTX_REGRESSION_OUTPUT")
	if output == "" {
		t.Logf("set CTX_REGRESSION_OUTPUT=/path/file.json to persist results")
	} else {
		blob, _ := json.MarshalIndent(map[string]any{
			"fixture_schema_version": fx.SchemaVersion,
			"fixture_generated_at":   fx.GeneratedAt,
			"run_at":                 time.Now().UTC().Format(time.RFC3339),
			"chat_model":             chatModel,
			"counts":                 counts,
			"results":                results,
		}, "", "  ")
		if err := os.WriteFile(output, blob, 0o600); err != nil {
			t.Errorf("write output %s: %v", output, err)
		} else {
			t.Logf("baseline written to %s", output)
		}
	}

	// Per-case detail (sorted by case_id)
	sort.Slice(results, func(i, j int) bool { return results[i].CaseID < results[j].CaseID })
	for _, r := range results {
		t.Logf("Case %2d [%s] V5=%s now=%s ideal=%s match=%s",
			r.CaseID, r.Category, r.BaselineRel, r.CurrentRel, r.IdealRel, r.Match)
	}
}

// TestPromptRegression_AssertNoBackslide loads a previously saved baseline
// (path via CTX_REGRESSION_BASELINE) and fails if the current run shows
// REGRESSION on any case marked "false_inverted_*" (those should keep being
// VALID or improve to "ideal").
//
// This is the CI-gate variant. Run TestPromptRegression_BaselineRun first
// to produce CTX_REGRESSION_OUTPUT, then re-use as baseline.
func TestPromptRegression_AssertNoBackslide(t *testing.T) {
	baselinePath := os.Getenv("CTX_REGRESSION_BASELINE")
	if baselinePath == "" {
		t.Skip("CTX_REGRESSION_BASELINE not set — gate-test skipped (run BaselineRun first)")
	}
	t.Skip("TODO Welle 47: implement diff vs baseline JSON. Skeleton-Stub: compare counts['drifted'] curr vs baseline, FAIL if curr > baseline.")
}
