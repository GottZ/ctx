package embedmigration

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// CreateParams is the caller-supplied intent for a new re-embed migration
// (design §3.2b / §4.2 / §4.10). Mode is always "dual" in v1 (E-04-1) — the
// column exists in the migration but Create never needs to pass it.
type CreateParams struct {
	FromModel string
	ToModel   string
	// ToBackend is a context_backends.name — the create-validation anchor
	// (§4.2): must exist, be Locality "local", be global-scoped, and carry
	// the model_map key "embed_next" resolving to ToModel.
	ToBackend string
	// ReuseExisting allows Create to proceed despite leftover
	// embedding_next data from a PRIOR migration, provided that data's
	// embed_model_next matches ToModel and the prior migration did not end
	// rolled_back (§4.10 — rollback data is "aktenkundig verdächtig", never
	// silently reused).
	ReuseExisting bool
}

// Validation errors — each names the exact §-anchored rule it enforces so a
// caller (CLI/API, W04-7) can render a precise rejection instead of a bare
// "create failed".
var (
	ErrModelsIdentical         = errors.New("embedmigration: create: from_model and to_model are identical")
	ErrFromModelUnknown        = errors.New("embedmigration: create: from_model is not registered in context_embed_models")
	ErrToModelUnknown          = errors.New("embedmigration: create: to_model is not registered in context_embed_models")
	ErrDimsMismatch            = errors.New("embedmigration: create: from_model and to_model have different stored_dims (dimension-change migrations are out of scope for v1, design §6.4/E-04-6)")
	ErrBackendNotFound         = errors.New("embedmigration: create: to_backend does not exist")
	ErrBackendNotLocal         = errors.New("embedmigration: create: to_backend locality is not \"local\"")
	ErrBackendNotGlobalScoped  = errors.New("embedmigration: create: to_backend is tenant-scoped, not global — invisible to the scheduler router (VisibleTo)")
	ErrBackendMissingEmbedNext = errors.New("embedmigration: create: to_backend's model_map has no \"embed_next\" entry resolving to to_model")
	ErrRestEmbeddingNextData   = errors.New("embedmigration: create: context_blocks has leftover embedding_next data from a prior migration — pass ReuseExisting or purge it first")
	ErrReuseModelMismatch      = errors.New("embedmigration: create: ReuseExisting set but leftover embedding_next data's embed_model_next does not match to_model")
	ErrReuseAfterRollback      = errors.New("embedmigration: create: ReuseExisting set but the last migration ended rolled_back — rollback data is never silently reused (§4.10)")
	ErrReuseRequiresAborted    = errors.New("embedmigration: create: ReuseExisting set but the last migration did not end aborted — only abort leftovers are unsuspicious partial work (§4.10); anything else is of unknown provenance and must be purged instead")
	ErrActiveMigrationExists   = errors.New("embedmigration: create: another migration is already active (idx_embed_migration_single_active)")
	ErrDiskCheckFailed         = errors.New("embedmigration: create: disk pre-flight check itself failed — fail-closed, refusing to start blind")
	ErrDiskInsufficient        = errors.New("embedmigration: create: disk pre-flight estimate exceeds free space")
)

// DiskEstimate is the Fehler payload for ErrDiskInsufficient — a caller can
// errors.As into it to render the actual numbers instead of a bare message.
type DiskEstimate struct {
	NeededBytes uint64
	FreeBytes   uint64
}

func (e *DiskEstimate) Error() string {
	return fmt.Sprintf("%v: needed ~%d bytes, free %d bytes", ErrDiskInsufficient, e.NeededBytes, e.FreeBytes)
}

func (e *DiskEstimate) Unwrap() error { return ErrDiskInsufficient }

// Per-vector cost constants, measured (design §6.1, Inventur 01 §6.3):
// 4.1 kB fp32 TOAST + 2.8 kB halfvec-HNSW graph entry per migrated vector.
// diskSafetyMarginPct pads the raw estimate for WAL/checkpoint overhead the
// per-vector numbers don't themselves account for (§6.1's WAL-volume line).
const (
	bytesPerVectorFP32  uint64 = 4100
	bytesPerVectorHNSW  uint64 = 2800
	diskSafetyMarginPct uint64 = 20
)

// EstimateDiskBytes is the Pre-Flight-Disk-Check formula (§6.1): total
// eligible blocks × measured per-vector cost, padded by the safety margin.
func EstimateDiskBytes(totalBlocks int64) uint64 {
	if totalBlocks <= 0 {
		return 0
	}
	perVector := bytesPerVectorFP32 + bytesPerVectorHNSW
	raw := uint64(totalBlocks) * perVector
	return raw + raw*diskSafetyMarginPct/100
}

// DiskChecker reports free bytes at the volume the migration's temporary
// data (embedding_next + its HNSW build) would land on. Production wires
// StatfsChecker against a real mount; tests inject a stub so the fail-closed
// negative path is reachable without actually filling a disk (design §6.1:
// "realer statfs … reicht als Positiv-Pfad, der Negativ-Pfad braucht
// Injektion").
type DiskChecker func() (freeBytes uint64, err error)

// StatfsChecker returns a DiskChecker backed by a real statfs(2) call
// against path. Bavail (blocks available to an unprivileged user) times
// Bsize, matching `df`'s "available" column rather than raw free (Bfree
// includes the root-reserved slice, which this fail-closed check should not
// count as usable headroom).
func StatfsChecker(path string) DiskChecker {
	return func() (uint64, error) {
		return statfsAvailableBytes(path)
	}
}

// Querier already declared in state.go covers Exec/QueryRow/Query for both
// *pgxpool.Pool and pgx.Tx.

// Create validates the requested migration against the full v1 rule set and,
// if every check passes, inserts the context_embed_migrations row. Returns
// the new row's id on success.
//
// Validation order (all checks run before ANY write — fail-closed, cheapest
// checks first):
//  1. from_model != to_model (params + DB CHECK both enforce this; the Go
//     check exists so the caller gets a named error instead of parsing a
//     constraint-violation message).
//  2. both models registered in context_embed_models.
//  3. stored_dims equal (§6.4/E-04-6: v1 rejects dimension changes).
//  4. to_backend exists, Locality local, globally scoped, carries
//     model_map["embed_next"].Model == to_model (§4.2).
//  5. no leftover embedding_next data unless ReuseExisting — and if reused,
//     the leftover data's embed_model_next matches to_model AND the prior
//     migration did not end rolled_back (§4.10).
//  6. disk pre-flight (§6.1): estimated temp+index bytes for the eligible
//     block count against disk's free bytes. A disk-check ERROR is
//     fail-closed (ErrDiskCheckFailed), not silently skipped.
//  7. INSERT — a concurrent second active migration surfaces as
//     ErrActiveMigrationExists (the single-active partial-unique index is
//     the actual race-proof gate; steps 1-6 are advisory/fast-fail).
func Create(ctx context.Context, q Querier, p CreateParams, disk DiskChecker) (id string, err error) {
	if p.FromModel == p.ToModel {
		return "", ErrModelsIdentical
	}

	fromDims, err := storedDims(ctx, q, p.FromModel)
	if err != nil {
		return "", err
	}
	if fromDims < 0 {
		return "", ErrFromModelUnknown
	}
	toDims, err := storedDims(ctx, q, p.ToModel)
	if err != nil {
		return "", err
	}
	if toDims < 0 {
		return "", ErrToModelUnknown
	}
	if fromDims != toDims {
		return "", ErrDimsMismatch
	}

	if err := validateToBackend(ctx, q, p.ToBackend, p.ToModel); err != nil {
		return "", err
	}

	if err := validateNoLeftoverNextData(ctx, q, p); err != nil {
		return "", err
	}

	totalBlocks, err := countEligibleBlocks(ctx, q)
	if err != nil {
		return "", fmt.Errorf("embedmigration: create: count eligible blocks: %w", err)
	}
	if disk != nil {
		free, derr := disk()
		if derr != nil {
			return "", fmt.Errorf("%w: %w", ErrDiskCheckFailed, derr)
		}
		needed := EstimateDiskBytes(totalBlocks)
		if free < needed {
			return "", &DiskEstimate{NeededBytes: needed, FreeBytes: free}
		}
	}

	err = q.QueryRow(ctx,
		`INSERT INTO context_embed_migrations (from_model, to_model, to_backend, total_blocks)
		 VALUES ($1, $2, $3, $4) RETURNING id::text`,
		p.FromModel, p.ToModel, p.ToBackend, totalBlocks,
	).Scan(&id)
	if err != nil {
		if pgxdb.UniqueViolation(err) {
			return "", ErrActiveMigrationExists
		}
		return "", fmt.Errorf("embedmigration: create: insert: %w", err)
	}
	return id, nil
}

// storedDims returns the model's registered stored_dims, or -1 if the model
// is not registered (a sentinel rather than a second error return so callers
// stay terse — the caller already knows which of the two lookups produced
// -1 from which variable it inspects).
func storedDims(ctx context.Context, q Querier, modelKey string) (int, error) {
	var dims int
	err := q.QueryRow(ctx,
		`SELECT stored_dims FROM context_embed_models WHERE model_key = $1`, modelKey,
	).Scan(&dims)
	if errors.Is(err, pgx.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("embedmigration: create: lookup model %q: %w", modelKey, err)
	}
	return dims, nil
}

// validateToBackend enforces §4.2's create-time anchor: existence, local
// locality, global scope (VisibleTo — tenant-private backends are invisible
// to the scheduler router), and a model_map["embed_next"] entry resolving to
// toModel.
func validateToBackend(ctx context.Context, q Querier, backendName, toModel string) error {
	var locality, scope string
	var modelMapRaw []byte
	err := q.QueryRow(ctx,
		`SELECT locality, scope, model_map FROM context_backends WHERE name = $1`, backendName,
	).Scan(&locality, &scope, &modelMapRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBackendNotFound
	}
	if err != nil {
		return fmt.Errorf("embedmigration: create: lookup backend %q: %w", backendName, err)
	}
	if locality != backends.LocalityLocal {
		return ErrBackendNotLocal
	}
	if scope != backends.GlobalScope {
		return ErrBackendNotGlobalScoped
	}
	modelMap, err := backends.ParseModelMap(modelMapRaw)
	if err != nil {
		return fmt.Errorf("embedmigration: create: parse backend %q model_map: %w", backendName, err)
	}
	spec, ok := modelMap[ModelMapKeyEmbedNext]
	if !ok || spec.Model != toModel {
		return ErrBackendMissingEmbedNext
	}
	return nil
}

// validateNoLeftoverNextData enforces §4.10's create-vs-rest-data rule.
func validateNoLeftoverNextData(ctx context.Context, q Querier, p CreateParams) error {
	var hasLeftover bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM context_blocks WHERE embedding_next IS NOT NULL)`,
	).Scan(&hasLeftover); err != nil {
		return fmt.Errorf("embedmigration: create: leftover embedding_next probe: %w", err)
	}
	if !hasLeftover {
		return nil
	}
	if !p.ReuseExisting {
		return ErrRestEmbeddingNextData
	}

	var mismatch bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM context_blocks WHERE embedding_next IS NOT NULL AND embed_model_next IS DISTINCT FROM $1)`,
		p.ToModel,
	).Scan(&mismatch); err != nil {
		return fmt.Errorf("embedmigration: create: leftover model-match probe: %w", err)
	}
	if mismatch {
		return ErrReuseModelMismatch
	}

	var lastStatus string
	err := q.QueryRow(ctx,
		`SELECT status FROM context_embed_migrations ORDER BY created_at DESC LIMIT 1`,
	).Scan(&lastStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("embedmigration: create: last-migration-status probe: %w", err)
	}
	if lastStatus == string(StatusRolledBack) {
		return ErrReuseAfterRollback
	}
	// W04-6 tightening (design §4.10 point 1): reuse is only legitimate on
	// top of ABORT leftovers — an abort is unsuspicious partial work of a
	// known migration. Any other last status (done, or no migration row at
	// all despite leftover data) means the _next data's provenance is
	// unknown; refusing here forces the operator through purge instead of
	// silently adopting vectors nobody can attribute. The rolled_back case
	// keeps its own, more specific error above (Rot-4 gate wording).
	if lastStatus != string(StatusAborted) {
		return ErrReuseRequiresAborted
	}
	return nil
}

// countEligibleBlocks mirrors the migration-backfill pending universe's base
// set (§3.3, before subtracting already-migrated rows — Create wants the
// TOTAL denominator for total_blocks, not the current pending count): live,
// non-archived blocks that currently carry a from-space vector.
func countEligibleBlocks(ctx context.Context, q Querier) (int64, error) {
	var n int64
	err := q.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE embedding IS NOT NULL AND NOT is_archived`,
	).Scan(&n)
	return n, err
}
