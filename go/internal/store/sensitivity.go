// Package store — block sensitivity lookup (F3-P3 trust gating).
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Source: https://github.com/GottZ/ctx
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BlockSensitivity is one row of the batch lookup: the stored classification
// plus the scope the scope-floor keys on.
type BlockSensitivity struct {
	Sensitivity backends.Sensitivity
	Scope       string
}

// FetchSensitivities batch-loads sensitivity + scope for ALL result IDs in
// one PK =ANY lookup (<1ms for ~210 IDs). Deliberately NOT threaded through
// ctx_rrf (that would mean CREATE OR REPLACE of the PG function = its own
// migration + mirror maintenance). IDs without a returned row (block deleted
// or archived between RRF and lookup) are MISSING from the map — the caller
// treats a miss as credentials (fail-closed, F3 §2.3a).
// GetBlockSensitivity reads the current classification of one block in the
// caller's write-eligible scopes (the downgrade-guard comparison base).
// found=false when the block does not exist in those scopes.
func GetBlockSensitivity(ctx context.Context, pool *pgxpool.Pool, id string, writeScopes []string) (backends.Sensitivity, bool, error) {
	var sens string
	err := pool.QueryRow(ctx,
		`SELECT sensitivity FROM context_blocks
		 WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived`, id, writeScopes).Scan(&sens)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get block sensitivity: %w", err)
	}
	return backends.Sensitivity(sens), true, nil
}

// FetchSensitivities batch-loads sensitivity + scope for ALL result IDs in
// one PK =ANY lookup (<1ms for ~210 IDs). Deliberately NOT threaded through
// ctx_rrf (that would mean CREATE OR REPLACE of the PG function = its own
// migration + mirror maintenance). IDs without a returned row (block deleted
// or archived between RRF and lookup) are MISSING from the map — the caller
// treats a miss as credentials (fail-closed, F3 §2.3a).
func FetchSensitivities(ctx context.Context, pool *pgxpool.Pool, ids []string) (map[string]BlockSensitivity, error) {
	if len(ids) == 0 {
		return map[string]BlockSensitivity{}, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT id, sensitivity, scope FROM context_blocks
		 WHERE id = ANY($1::uuid[]) AND NOT is_archived`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: fetch sensitivities: %w", err)
	}
	defer rows.Close()

	out := make(map[string]BlockSensitivity, len(ids))
	for rows.Next() {
		var id, sens, scope string
		if err := rows.Scan(&id, &sens, &scope); err != nil {
			return nil, fmt.Errorf("store: fetch sensitivities scan: %w", err)
		}
		out[id] = BlockSensitivity{Sensitivity: backends.Sensitivity(sens), Scope: scope}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: fetch sensitivities rows: %w", err)
	}
	return out, nil
}

// G41 sensitivity LLM audit — pick / verdict / progress.
//
// The audit only ever touches sensitivity_source='default' rows: 'manual' is
// untouchable BY THE SQL PREDICATE, not by caller discipline — a concurrent
// manual classification between pick and verdict makes the verdict UPDATE
// match zero rows. 'llm-audit'/'pattern' rows are already classified and
// never re-enter the pick set (re-audit = an explicit source reset).
//
// 'derived' (migration 144, W01-4) deliberately does NOT join that inclusion
// list. A folded classification carries provenance — it is a decision over the
// source blocks, not a missing one — and the audit re-deciding it would drop
// the fold on the floor. The correction path for a derived row is the G40
// pattern sweep below, whose predicate excludes rather than includes.

// AuditBlock is one pick-set row handed to the classifier.
type AuditBlock struct {
	ID      string
	Title   string
	Content string
	// ContentMD5 is the digest of the CONTENT COLUMN AS PICKED — the version
	// the model judged and the version the structural veto scanned. It travels
	// back into ApplyAuditVerdict so a verdict can never be stamped on a
	// content the run never saw (design 04 §2.4-C). md5 is a CHANGE detector
	// here, not a security hash: the required property is "different bytes,
	// different digest", not collision resistance against an adversary who
	// would have to hit a digest they cannot read.
	ContentMD5 string
}

// PickAuditBlocks loads up to limit unclassified blocks of the server's home
// scope (design 03 §2.3d: classification follows block ownership — foreign
// scopes stay fail-closed at the credentials default until THEIR tenant
// audits them). retryCooldown skips blocks whose last attempt produced no
// verdict (audited_at stamped, source still 'default'). randomOrder serves
// the N=30 sample gate — a representative slice, not the oldest corner.
func PickAuditBlocks(ctx context.Context, pool *pgxpool.Pool, homeScope string,
	retryCooldown time.Duration, limit int, randomOrder bool,
) ([]AuditBlock, error) {
	order := "created_at ASC"
	if randomOrder {
		order = "random()"
	}
	rows, err := pool.Query(ctx,
		`SELECT id, title, content, md5(content) FROM context_blocks
		 WHERE sensitivity_source = 'default' AND NOT is_archived AND scope = $1
		   AND (sensitivity_audited_at IS NULL OR sensitivity_audited_at < now() - make_interval(secs => $2))
		 ORDER BY `+order+` LIMIT $3`,
		homeScope, retryCooldown.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: pick audit blocks: %w", err)
	}
	defer rows.Close()

	var out []AuditBlock
	for rows.Next() {
		var b AuditBlock
		if err := rows.Scan(&b.ID, &b.Title, &b.Content, &b.ContentMD5); err != nil {
			return nil, fmt.Errorf("store: pick audit blocks scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pick audit blocks rows: %w", err)
	}
	return out, nil
}

// ApplyAuditVerdict writes one LLM verdict: sensitivity, source='llm-audit',
// audit timestamp. The source predicate IS the manual-untouchable guarantee
// (negatively probed in the integration test); applied=false means the row
// was (re)classified by someone else since the pick — the verdict is
// discarded, never forced.
//
// contentMD5 binds the verdict to the content VERSION it was formed over
// (design 04 §2.4-C, wave H10): the LLM call happens outside any transaction,
// and an ordinary content update does not reset sensitivity_source, so without
// this conjunct a writer could store harmless v1, let the audit judge v1, and
// overwrite with v2 before the verdict lands. It binds the structural veto just
// as much as the model verdict — clampVerdict scans the SAME picked copy, so a
// stale write would carry a "no secret here" decision about text nobody
// scanned. A mismatch is the identical, already-modelled outcome as the manual
// race: applied=false, Discarded++, block untouched, re-picked next run.
func ApplyAuditVerdict(ctx context.Context, pool *pgxpool.Pool, id string, sens backends.Sensitivity, contentMD5 string) (applied bool, err error) {
	tag, err := pool.Exec(ctx,
		`UPDATE context_blocks
		    SET sensitivity = $2, sensitivity_source = 'llm-audit', sensitivity_audited_at = now()
		  WHERE id = $1 AND sensitivity_source = 'default' AND md5(content) = $3`,
		id, string(sens), contentMD5)
	if err != nil {
		return false, fmt.Errorf("store: apply audit verdict: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkAuditAttempt stamps a no-verdict attempt (parse failure): the block
// stays at the credentials default with source='default', the timestamp
// keeps it out of the pick set for the retry cooldown.
func MarkAuditAttempt(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET sensitivity_audited_at = now()
		  WHERE id = $1 AND sensitivity_source = 'default'`, id)
	if err != nil {
		return fmt.Errorf("store: mark audit attempt: %w", err)
	}
	return nil
}

// AuditProgress aggregates the classification state of the home scope for
// the status surface: rows per sensitivity_source plus the pending count
// (source='default' — what a full audit run still has in front of it).
func AuditProgress(ctx context.Context, pool *pgxpool.Pool, homeScope string) (pending int, bySource map[string]int, err error) {
	rows, err := pool.Query(ctx,
		`SELECT sensitivity_source, count(*) FROM context_blocks
		 WHERE NOT is_archived AND scope = $1
		 GROUP BY sensitivity_source`, homeScope)
	if err != nil {
		return 0, nil, fmt.Errorf("store: audit progress: %w", err)
	}
	defer rows.Close()

	bySource = make(map[string]int, 4)
	for rows.Next() {
		var source string
		var n int
		if err := rows.Scan(&source, &n); err != nil {
			return 0, nil, fmt.Errorf("store: audit progress scan: %w", err)
		}
		bySource[source] = n
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("store: audit progress rows: %w", err)
	}
	return bySource["default"], bySource, nil
}

// G40 credentials pattern re-audit — keyset-paginated candidate pick + the
// upgrade-only verdict write. The detector NEVER downgrades: it only raises a
// block to credentials, stamping sensitivity_source='pattern' (the veto marker
// the G41 audit's source='default' pick set can never re-touch).

// ClassifyBlock is one candidate the pattern detector may still raise.
type ClassifyBlock struct {
	ID      string
	Title   string
	Content string
}

// PickClassifyCandidates keyset-paginates (by id) the home-scope blocks the
// detector may still raise: sensitivity <> 'credentials' AND source <> 'manual'
// (manual is untantastbar, already-credentials needs no raise). The source
// predicate EXCLUDES rather than includes, so every source class that is not
// 'manual' stays sweepable — 'derived' (migration 144) included, without a
// predicate change. That is what keeps a folded block correctable for
// EXISTING rows; the write-time detector (ApplyWriteDetector) only ever sees
// writes. Pinned in both directions by TestDerivedSweepCoverage_Integration.
//
// afterID "" = start at the beginning. Keyset over OFFSET is drain-safe under concurrent
// upgrades: a row that turns credentials drops out of the predicate, but its id
// is already behind the cursor so it is never re-picked or skipped.
func PickClassifyCandidates(ctx context.Context, pool *pgxpool.Pool, homeScope, afterID string, limit int) ([]ClassifyBlock, error) {
	if afterID == "" {
		afterID = "00000000-0000-0000-0000-000000000000"
	}
	rows, err := pool.Query(ctx,
		`SELECT id, title, content FROM context_blocks
		 WHERE scope = $1 AND NOT is_archived
		   AND sensitivity <> 'credentials' AND sensitivity_source <> 'manual'
		   AND id > $2::uuid
		 ORDER BY id LIMIT $3`,
		homeScope, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pick classify candidates: %w", err)
	}
	defer rows.Close()

	var out []ClassifyBlock
	for rows.Next() {
		var b ClassifyBlock
		if err := rows.Scan(&b.ID, &b.Title, &b.Content); err != nil {
			return nil, fmt.Errorf("store: pick classify candidates scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pick classify candidates rows: %w", err)
	}
	return out, nil
}

// ApplyPatternVerdict raises one home-scope block to credentials with
// sensitivity_source='pattern' and records metadata.sensitivity_detector
// (kind + reason, never the matched secret). The WHERE predicate re-checks the
// upgrade-only + manual-untouchable invariants at write time: applied=false
// means the block raced to credentials or manual since the pick — discard, not
// force. Scope-bound by construction (2.3d: no cross-tenant reclassification).
func ApplyPatternVerdict(ctx context.Context, pool *pgxpool.Pool, id, homeScope, kind, reason string) (applied bool, err error) {
	tag, err := pool.Exec(ctx,
		`UPDATE context_blocks
		    SET sensitivity = 'credentials', sensitivity_source = 'pattern',
		        sensitivity_audited_at = now(),
		        metadata = COALESCE(metadata, '{}'::jsonb)
		                 || jsonb_build_object('sensitivity_detector',
		                        jsonb_build_object('kind', $3::text, 'reason', $4::text))
		  WHERE id = $1 AND scope = $2 AND NOT is_archived
		    AND sensitivity <> 'credentials' AND sensitivity_source <> 'manual'`,
		id, homeScope, kind, reason)
	if err != nil {
		return false, fmt.Errorf("store: apply pattern verdict: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
