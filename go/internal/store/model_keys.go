package store

// Model consumers and model keys — the store primitives of the OpenAI-compatible
// model API (design/01 §3.2, §4.2, §4.6; migration 161).
//
// A model consumer is an external caller of the models ctx serves; a model key
// (ctxm_…, internal/modelkey) is a credential of exactly one consumer and
// carries no policy. The rules these primitives keep:
//
//   - Tenant binding through the consumer. context_model_keys has no tenant
//     column, so every key primitive reaches its tenant through
//     context_model_consumers (V-01-g): a key or consumer of another tenant,
//     an unknown id and a malformed id all read as the same not-found error.
//   - No delete path. A consumer is deactivated (active = false; the
//     migration-161 trigger revokes its keys) and a key is revoked; only the
//     tenant prune (tenant.go, step 7b) removes rows (I-K6).
//   - The plaintext of a key leaves the store once, as the modelkey.Key the
//     mint returns (Key.Plaintext); the store keeps only its SHA-256 and its
//     hint, and no read here selects key_hash.
//   - At most two live keys per consumer, two only within a rotation's grace
//     window (I-K2): RotateKey runs under a FOR UPDATE lock of the consumer
//     row. Mint and rotation take that lock themselves and check active under
//     it — the insert guard of migration 161 (23514 into an inactive
//     consumer) is the second door, not the only one.
//   - No path raises expires_at of an existing key (V-01-d): a rotation only
//     ever shortens the predecessor's expiry, and ModelConsumerPatch has no
//     expiry field; a new expiry needs a new key.
//   - Every mutating transaction stamps its audit attribution first
//     (stampAudit); the triggers write the rows. The usage flush is the one
//     unstamped write, and the UPDATE OF list of migration 161 keeps it out of
//     the audit trail.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxRotationGrace bounds the grace window of a rotation, the time the
// predecessor key stays valid next to its successor. A mechanism constant
// (design/01 §4.9 "hardcoded-legitimate"): the default grace is policy
// (modelapi.key_rotation_grace) and reaches RotateKey as a parameter.
const MaxRotationGrace = 7 * 24 * time.Hour

// MaxKeyHistoryLimit is the largest page KeyHistory serves (design/01 §4.6:
// model-key-history limit ≤ 100); the caller clamps its own parameter.
const MaxKeyHistoryLimit = 100

// ModelKeyUsageFlushInterval is the cadence of the batched last_used_at flush
// (design/01 §4.2, §4.9 mechanism constant): the field's resolution, and the
// most stamps a crash can lose.
const ModelKeyUsageFlushInterval = 60 * time.Second

// modelKeyUsageFinalFlushTimeout bounds the flush Run performs after its
// context ends, on a context of its own (the cancelled one cannot carry it).
const modelKeyUsageFinalFlushTimeout = 5 * time.Second

var (
	// ErrModelConsumerNotFound: no consumer with this id in the caller's reach —
	// unknown, malformed or another tenant's, indistinguishably (no oracle).
	ErrModelConsumerNotFound = errors.New("store: model consumer not found")
	// ErrModelKeyNotFound: no key with this id in the caller's reach, read the
	// same way as ErrModelConsumerNotFound.
	ErrModelKeyNotFound = errors.New("store: model key not found")
	// ErrModelConsumerCapExceeded: the tenant already has max_model_consumers
	// active consumers (create, reactivation). An inactive consumer holds no slot.
	ErrModelConsumerCapExceeded = errors.New("store: tenant model consumer cap exceeded")
	// ErrModelConsumerInactive: a key was to be minted for an inactive consumer —
	// refused by the store's own check under the consumer lock, or by the
	// migration-161 insert guard (SQLSTATE 23514), which this maps to.
	ErrModelConsumerInactive = errors.New("store: model consumer is not active")
	// ErrModelConsumerLabelTaken: the tenant already has a consumer of this label
	// (uq_model_consumers_tenant_label).
	ErrModelConsumerLabelTaken = errors.New("store: model consumer label already exists in this tenant")
	// ErrRotationGraceOutOfRange: a rotation grace below 0 or above
	// MaxRotationGrace.
	ErrRotationGraceOutOfRange = errors.New("store: rotation grace out of range")
)

// ModelAccess binds a primitive to its caller: a tenant-admin reaches the
// consumers of TenantID, a server-admin (ServerAdmin) those of every tenant.
// An empty TenantID without ServerAdmin reaches nothing (fail-closed).
type ModelAccess struct {
	TenantID    string
	ServerAdmin bool
}

// args returns the ($serverAdmin, $tenant) pair of the access predicate
// sqlConsumerAccess; an empty tenant binds NULL, which matches no row.
func (a ModelAccess) args() (bool, *string) {
	if a.ServerAdmin || a.TenantID == "" {
		return a.ServerAdmin, nil
	}
	t := a.TenantID
	return false, &t
}

// ModelConsumer is a consumer as the store reads it — never a key secret.
// LastUsedAt, Keys and KeysTotal are filled by ListConsumers only: Keys holds
// the live keys (at most two, I-K2), KeysTotal counts the whole chain.
type ModelConsumer struct {
	ID                string
	TenantID          string
	Label             string
	Description       string
	Kind              string
	TenantScope       *string
	AllModels         bool
	PromptSensitivity string
	FairWeight        int16
	AdmissionLane     string
	AllowedCIDRs      []string
	AllowPlaintext    bool
	Active            bool
	PeerInstanceID    *string
	PeerBoundAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	LastUsedAt        *time.Time
	Keys              []ModelKeyInfo
	KeysTotal         int
}

// ModelKeyInfo is a key as the store reads it: id, hint and lifecycle. There
// is no field for the hash or the plaintext.
type ModelKeyInfo struct {
	ID          string     `json:"id"`
	ConsumerID  string     `json:"consumer_id"`
	Hint        string     `json:"hint"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	RotatedFrom *string    `json:"rotated_from"`
}

// ModelConsumerSpec is a new consumer. A zero Kind, PromptSensitivity,
// FairWeight or AdmissionLane takes the column default of migration 161
// ('app', 'credentials', 1, 'bulk' — the fail-closed values); nil
// TenantScope means _global backends only, nil AllowedCIDRs any source. The
// handler validates field tiers and the tenant's ownership of TenantScope
// before it calls; the database CHECKs are the backstop.
type ModelConsumerSpec struct {
	TenantID          string
	Label             string
	Description       string
	Kind              string
	TenantScope       *string
	AllModels         bool
	PromptSensitivity string
	FairWeight        int16
	AdmissionLane     string
	AllowedCIDRs      []string
	AllowPlaintext    bool
}

// ModelConsumerPatch is a partial update of a consumer's policy: a nil field
// stays unchanged. ClearTenantScope / ClearAllowedCIDRs write NULL (the field
// beside it must then be nil). Active false deactivates (the trigger revokes
// every live key), Active true reactivates (counted against the cap; it
// revives no key). There is deliberately no field for kind, the peer binding
// or any key expiry: a patch cannot extend a key (V-01-d).
type ModelConsumerPatch struct {
	Label             *string
	Description       *string
	TenantScope       *string
	ClearTenantScope  bool
	AllModels         *bool
	PromptSensitivity *string
	FairWeight        *int16
	AdmissionLane     *string
	AllowedCIDRs      []string
	ClearAllowedCIDRs bool
	AllowPlaintext    *bool
	Active            *bool
}

// KeyCursor is the keyset position of KeyHistory: the (created_at, id) of the
// last key of the previous page.
type KeyCursor struct {
	CreatedAt time.Time
	ID        string
}

// ListConsumersOptions narrows ListConsumers; the zero value lists active
// consumers.
type ListConsumersOptions struct {
	IncludeInactive bool
}

const (
	// sqlActorKey / sqlActorPrincipal resolve the stamped actor
	// (ctx.api_key_id) to the actor columns: its key id and its principal,
	// NULL when nothing is stamped or the key no longer exists — the
	// existence-subselect of the migration-161 deactivation trigger, so a
	// stale actor never fails a write on the foreign key.
	sqlActorKey = `(SELECT a.id FROM context_api_keys a
	                 WHERE a.id = NULLIF(current_setting('ctx.api_key_id', true), '')::uuid)`
	sqlActorPrincipal = `(SELECT a.principal_id FROM context_api_keys a
	                       WHERE a.id = NULLIF(current_setting('ctx.api_key_id', true), '')::uuid)`

	// sqlConsumerAccess is the tenant predicate of every primitive on the
	// consumer alias c, bound to ModelAccess.args at $2/$3 (ListConsumers,
	// without an id parameter, binds them at $1/$2: sqlListAccess).
	sqlConsumerAccess = `($2::boolean OR c.tenant_id = $3::uuid)`
	sqlListAccess     = `($1::boolean OR c.tenant_id = $2::uuid)`

	// sqlKeyLive is I-K2's "live" on the key alias k.
	sqlKeyLive = `k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())`

	sqlConsumerCols = `c.id::text, c.tenant_id::text, c.label, c.description, c.kind, c.tenant_scope,
	       c.all_models, c.prompt_sensitivity, c.fair_weight, c.admission_lane,
	       c.allowed_cidrs::text[], c.allow_plaintext, c.active, c.peer_instance_id::text,
	       c.peer_bound_at, c.created_at, c.updated_at`

	sqlKeyCols = `k.id::text, k.consumer_id::text, k.key_hint, k.created_at, k.expires_at,
	       k.revoked_at, k.last_used_at, k.rotated_from::text`
)

// consumerDest returns the scan targets of sqlConsumerCols.
func consumerDest(c *ModelConsumer) []any {
	return []any{&c.ID, &c.TenantID, &c.Label, &c.Description, &c.Kind, &c.TenantScope,
		&c.AllModels, &c.PromptSensitivity, &c.FairWeight, &c.AdmissionLane,
		&c.AllowedCIDRs, &c.AllowPlaintext, &c.Active, &c.PeerInstanceID,
		&c.PeerBoundAt, &c.CreatedAt, &c.UpdatedAt}
}

// keyDest returns the scan targets of sqlKeyCols.
func keyDest(k *ModelKeyInfo) []any {
	return []any{&k.ID, &k.ConsumerID, &k.Hint, &k.CreatedAt, &k.ExpiresAt,
		&k.RevokedAt, &k.LastUsedAt, &k.RotatedFrom}
}

// CreateConsumerWithKey creates a consumer of spec.TenantID and its first key
// in one transaction and returns the key once: its plaintext is
// key.Plaintext(), nowhere else (String and LogValue give the hint).
//
// Cap: the tenant row is locked FOR UPDATE (as MintKeyWithQuota does), so the
// count of ACTIVE consumers against max_model_consumers and the insert are one
// unit — two creates at cap − 1 cannot both pass. NULL = unlimited. An
// unknown or malformed tenant is ErrTenantNotFound. keyExpiresAt is the
// expiry the caller resolved for the consumer's kind (W01-5); nil = none.
func CreateConsumerWithKey(ctx context.Context, pool *pgxpool.Pool, spec ModelConsumerSpec,
	keyExpiresAt *time.Time, stamp AuditStamp) (ModelConsumer, ModelKeyInfo, modelkey.Key, error) {

	if strings.TrimSpace(spec.Label) == "" {
		return ModelConsumer{}, ModelKeyInfo{}, modelkey.Key{}, errors.New("store: model consumer: label is required")
	}
	key, err := modelkey.Generate()
	if err != nil {
		return ModelConsumer{}, ModelKeyInfo{}, modelkey.Key{}, fmt.Errorf("store: model consumer: %w", err)
	}
	var (
		c ModelConsumer
		k ModelKeyInfo
	)
	if err := pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "model_consumers: create begin", Commit: "model_consumers: create commit"},
		func(tx pgx.Tx) error {
			if err := stampAudit(ctx, tx, stamp); err != nil {
				return err
			}
			limit, err := lockTenantRow(ctx, tx, spec.TenantID, ErrTenantNotFound)
			if err != nil {
				return err
			}
			if err := checkConsumerCap(ctx, tx, spec.TenantID, limit); err != nil {
				return err
			}
			if c, err = insertConsumerTx(ctx, tx, spec); err != nil {
				return err
			}
			k, err = insertModelKeyTx(ctx, tx, c.ID, key, keyExpiresAt, nil)
			return err
		}); err != nil {
		return ModelConsumer{}, ModelKeyInfo{}, modelkey.Key{}, err
	}
	return c, k, key, nil
}

// lockTenantRow locks the tenant row FOR UPDATE — the one serialisation point
// of every write that can raise the tenant's active-consumer count (create,
// reactivation) — and returns its max_model_consumers (nil = unlimited).
// missing is the error of an absent or malformed tenant.
func lockTenantRow(ctx context.Context, tx pgx.Tx, tenantID string, missing error) (*int, error) {
	var limit *int
	if err := tx.QueryRow(ctx,
		`SELECT max_model_consumers FROM context_tenants WHERE id = $1::uuid FOR UPDATE`,
		tenantID).Scan(&limit); err != nil {
		if pgxdb.AbsentOrMalformed(err) {
			return nil, missing
		}
		return nil, fmt.Errorf("model_consumers: tenant lock: %w", err)
	}
	return limit, nil
}

// checkConsumerCap refuses with ErrModelConsumerCapExceeded when the tenant's
// ACTIVE consumers already fill limit. Call it under lockTenantRow, so it sees
// every competing create or reactivation committed.
func checkConsumerCap(ctx context.Context, tx pgx.Tx, tenantID string, limit *int) error {
	if limit == nil {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM context_model_consumers WHERE tenant_id = $1::uuid AND active`,
		tenantID).Scan(&n); err != nil {
		return fmt.Errorf("model_consumers: count: %w", err)
	}
	if n >= *limit {
		return ErrModelConsumerCapExceeded
	}
	return nil
}

// insertConsumerTx inserts spec; a zero optional field is left to its column
// default so the fail-closed defaults live in one place (migration 161).
func insertConsumerTx(ctx context.Context, tx pgx.Tx, spec ModelConsumerSpec) (ModelConsumer, error) {
	cols := []string{"tenant_id", "label", "description", "tenant_scope", "all_models",
		"allowed_cidrs", "allow_plaintext", "created_by_key", "created_by_principal"}
	vals := []string{"$1::uuid", "$2", "$3", "$4", "$5", "$6::cidr[]", "$7", sqlActorKey, sqlActorPrincipal}
	args := []any{spec.TenantID, spec.Label, spec.Description, spec.TenantScope, spec.AllModels,
		spec.AllowedCIDRs, spec.AllowPlaintext}
	for _, o := range []struct {
		col, cast string
		set       bool
		val       any
	}{
		{"kind", "", spec.Kind != "", spec.Kind},
		{"prompt_sensitivity", "", spec.PromptSensitivity != "", spec.PromptSensitivity},
		{"fair_weight", "::smallint", spec.FairWeight != 0, spec.FairWeight},
		{"admission_lane", "", spec.AdmissionLane != "", spec.AdmissionLane},
	} {
		if o.set {
			args = append(args, o.val)
			cols = append(cols, o.col)
			vals = append(vals, fmt.Sprintf("$%d%s", len(args), o.cast))
		}
	}
	var c ModelConsumer
	sql := `INSERT INTO context_model_consumers AS c (` + strings.Join(cols, ", ") + `)
	        VALUES (` + strings.Join(vals, ", ") + `)
	        RETURNING ` + sqlConsumerCols
	if err := tx.QueryRow(ctx, sql, args...).Scan(consumerDest(&c)...); err != nil {
		if pgxdb.UniqueViolation(err) {
			return ModelConsumer{}, ErrModelConsumerLabelTaken
		}
		return ModelConsumer{}, fmt.Errorf("model_consumers: insert: %w", err)
	}
	return c, nil
}

// insertModelKeyTx inserts key for consumerID — a new row, never an update of
// key_hash. created_at is the clock at the insert, not the transaction start:
// keys of a consumer are inserted one at a time under its row lock, so their
// created_at order is their insert order even when this transaction began
// before the predecessor's committed — the order RotateKey and KeyHistory
// read. A 23514 without a constraint name is the migration-161 guard
// (consumer not active): ErrModelConsumerInactive.
func insertModelKeyTx(ctx context.Context, tx pgx.Tx, consumerID string, key modelkey.Key,
	expiresAt *time.Time, rotatedFrom *string) (ModelKeyInfo, error) {

	var k ModelKeyInfo
	if err := tx.QueryRow(ctx,
		`INSERT INTO context_model_keys AS k
		        (consumer_id, key_hash, key_hint, expires_at, rotated_from, created_at,
		         created_by_key, created_by_principal)
		 VALUES ($1::uuid, $2, $3, $4, $5::uuid, clock_timestamp(), `+sqlActorKey+`, `+sqlActorPrincipal+`)
		 RETURNING `+sqlKeyCols,
		consumerID, key.Hash(), key.Hint(), expiresAt, rotatedFrom).Scan(keyDest(&k)...); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "" {
			return ModelKeyInfo{}, ErrModelConsumerInactive
		}
		return ModelKeyInfo{}, fmt.Errorf("model_keys: insert: %w", err)
	}
	return k, nil
}

// lockConsumer locks the consumer row FOR UPDATE within the caller's reach and
// returns its tenant and active flag; anything else is ErrModelConsumerNotFound.
func lockConsumer(ctx context.Context, tx pgx.Tx, consumerID string, access ModelAccess) (tenant string, active bool, err error) {
	admin, tenantArg := access.args()
	if err := tx.QueryRow(ctx,
		`SELECT c.tenant_id::text, c.active FROM context_model_consumers c
		  WHERE c.id = $1::uuid AND `+sqlConsumerAccess+`
		    FOR UPDATE`,
		consumerID, admin, tenantArg).Scan(&tenant, &active); err != nil {
		if pgxdb.AbsentOrMalformed(err) {
			return "", false, ErrModelConsumerNotFound
		}
		return "", false, fmt.Errorf("model_consumers: lock: %w", err)
	}
	return tenant, active, nil
}

// RotateKey mints a new key for a consumer and retires the old ones, in one
// transaction under the consumer's FOR UPDATE lock (design/01 §4.6):
//
//  1. every live key except the youngest is revoked;
//  2. the youngest keeps at most grace: expires_at = LEAST(expires_at, now() +
//     grace) — never later than it was (V-01-d); grace 0 revokes it at once
//     (the leak case);
//  3. the new key is INSERTed with rotated_from = the youngest, or NULL when
//     the consumer has no live key (the first key after a reactivation).
//
// Result: at most two live keys (I-K2). grace must lie in [0,
// MaxRotationGrace]; expiresAt is the new key's expiry as the caller resolved
// it (W01-5), nil = none. An inactive consumer is ErrModelConsumerInactive —
// checked under the lock, so a deactivation either waits for the rotation
// (and its trigger revokes the new key) or runs first and is seen here.
func RotateKey(ctx context.Context, pool *pgxpool.Pool, consumerID string, access ModelAccess,
	grace time.Duration, expiresAt *time.Time, stamp AuditStamp) (ModelKeyInfo, modelkey.Key, error) {

	if grace < 0 || grace > MaxRotationGrace {
		return ModelKeyInfo{}, modelkey.Key{}, ErrRotationGraceOutOfRange
	}
	key, err := modelkey.Generate()
	if err != nil {
		return ModelKeyInfo{}, modelkey.Key{}, fmt.Errorf("store: rotate model key: %w", err)
	}
	var k ModelKeyInfo
	if err := pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "model_keys: rotate begin", Commit: "model_keys: rotate commit"},
		func(tx pgx.Tx) error {
			if err := stampAudit(ctx, tx, stamp); err != nil {
				return err
			}
			_, active, err := lockConsumer(ctx, tx, consumerID, access)
			if err != nil {
				return err
			}
			if !active {
				return ErrModelConsumerInactive
			}
			youngest, err := retirePredecessorsTx(ctx, tx, consumerID, grace)
			if err != nil {
				return err
			}
			k, err = insertModelKeyTx(ctx, tx, consumerID, key, expiresAt, youngest)
			return err
		}); err != nil {
		return ModelKeyInfo{}, modelkey.Key{}, err
	}
	return k, key, nil
}

// retirePredecessorsTx runs steps 1 and 2 of RotateKey and returns the id of
// the youngest live key (nil when there is none). A key committed after this
// transaction began is newer than now(): it is revoked at its created_at
// (ck_model_keys_revoke_order), as the deactivation trigger does.
func retirePredecessorsTx(ctx context.Context, tx pgx.Tx, consumerID string, grace time.Duration) (*string, error) {
	var youngest *string
	if err := tx.QueryRow(ctx,
		`SELECT k.id::text FROM context_model_keys k
		  WHERE k.consumer_id = $1::uuid AND `+sqlKeyLive+`
		  ORDER BY k.created_at DESC, k.id DESC LIMIT 1`, consumerID).Scan(&youngest); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("model_keys: rotate youngest: %w", err)
	}
	// grace 0: the youngest goes too (NULL is distinct from every id).
	keep := youngest
	if grace == 0 {
		keep = nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE context_model_keys k
		    SET revoked_at = GREATEST(now(), k.created_at),
		        revoked_by_key = `+sqlActorKey+`, revoked_by_principal = `+sqlActorPrincipal+`
		  WHERE k.consumer_id = $1::uuid AND `+sqlKeyLive+` AND k.id IS DISTINCT FROM $2::uuid`,
		consumerID, keep); err != nil {
		return nil, fmt.Errorf("model_keys: rotate revoke: %w", err)
	}
	if keep != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE context_model_keys
			    SET expires_at = LEAST(COALESCE(expires_at, 'infinity'::timestamptz),
			                           now() + $2::bigint * interval '1 microsecond')
			  WHERE id = $1::uuid`, *keep, grace.Microseconds()); err != nil {
			return nil, fmt.Errorf("model_keys: rotate grace: %w", err)
		}
	}
	return youngest, nil
}

// RevokeKey revokes one key. The key reaches its tenant only through its
// consumer, so the UPDATE joins context_model_consumers (V-01-g): a key of
// another tenant is ErrModelKeyNotFound, byte-identical to an unknown or
// malformed id, and stays untouched. Revoking an already revoked key of the
// caller's reach is a no-op success.
func RevokeKey(ctx context.Context, pool *pgxpool.Pool, keyID string, access ModelAccess, stamp AuditStamp) error {
	admin, tenantArg := access.args()
	return pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "model_keys: revoke begin", Commit: "model_keys: revoke commit"},
		func(tx pgx.Tx) error {
			if err := stampAudit(ctx, tx, stamp); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx,
				`UPDATE context_model_keys k
				    SET revoked_at = GREATEST(now(), k.created_at),
				        revoked_by_key = `+sqlActorKey+`, revoked_by_principal = `+sqlActorPrincipal+`
				   FROM context_model_consumers c
				  WHERE k.id = $1::uuid AND c.id = k.consumer_id AND `+sqlConsumerAccess+`
				    AND k.revoked_at IS NULL`,
				keyID, admin, tenantArg)
			if err != nil {
				if pgxdb.MalformedUUID(err) {
					return ErrModelKeyNotFound
				}
				return fmt.Errorf("model_keys: revoke: %w", err)
			}
			if tag.RowsAffected() > 0 {
				return nil
			}
			var one int
			if err := tx.QueryRow(ctx,
				`SELECT 1 FROM context_model_keys k JOIN context_model_consumers c ON c.id = k.consumer_id
				  WHERE k.id = $1::uuid AND `+sqlConsumerAccess,
				keyID, admin, tenantArg).Scan(&one); err != nil {
				if pgxdb.AbsentOrMalformed(err) {
					return ErrModelKeyNotFound
				}
				return fmt.Errorf("model_keys: revoke lookup: %w", err)
			}
			return nil
		})
}

// DeleteConsumer is the soft delete of a consumer: active = false, after which
// the migration-161 trigger revokes every key it still has (I-K8). It never
// removes a row (I-K6). Deleting an inactive consumer succeeds and changes
// nothing audited.
func DeleteConsumer(ctx context.Context, pool *pgxpool.Pool, consumerID string, access ModelAccess, stamp AuditStamp) error {
	inactive := false
	_, err := UpdateConsumer(ctx, pool, consumerID, access, ModelConsumerPatch{Active: &inactive}, stamp)
	return err
}

// UpdateConsumer applies patch to a consumer under its FOR UPDATE lock and
// returns the result. A reactivation (active false → true) first locks the
// tenant row and counts against max_model_consumers under that lock — the
// same serialisation point as CreateConsumerWithKey, taken in the same order
// (tenant, then consumer; a consumer's tenant_id is immutable, so it is read
// unlocked first). It revives no key: the next key comes from RotateKey. A
// deactivation lets the trigger revoke the live keys and then records the
// acting principal on them (the trigger records the actor key only).
func UpdateConsumer(ctx context.Context, pool *pgxpool.Pool, consumerID string, access ModelAccess,
	patch ModelConsumerPatch, stamp AuditStamp) (ModelConsumer, error) {

	set, args, err := patch.assignments()
	if err != nil {
		return ModelConsumer{}, err
	}
	var c ModelConsumer
	if err := pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "model_consumers: update begin", Commit: "model_consumers: update commit"},
		func(tx pgx.Tx) error {
			if err := stampAudit(ctx, tx, stamp); err != nil {
				return err
			}
			activating := patch.Active != nil && *patch.Active
			var (
				limit *int
				err   error
			)
			if activating {
				if limit, err = lockTenantOfConsumer(ctx, tx, consumerID, access); err != nil {
					return err
				}
			}
			tenant, wasActive, err := lockConsumer(ctx, tx, consumerID, access)
			if err != nil {
				return err
			}
			if activating && !wasActive {
				if err := checkConsumerCap(ctx, tx, tenant, limit); err != nil {
					return err
				}
			}
			var revoking []string
			if patch.Active != nil && !*patch.Active && wasActive {
				if revoking, err = liveKeyIDsTx(ctx, tx, consumerID); err != nil {
					return err
				}
			}
			if c, err = writeConsumerTx(ctx, tx, consumerID, set, args); err != nil {
				return err
			}
			return stampRevokedPrincipalTx(ctx, tx, revoking)
		}); err != nil {
		return ModelConsumer{}, err
	}
	return c, nil
}

// lockTenantOfConsumer locks the tenant row of a consumer in the caller's
// reach — before the consumer row, the order CreateConsumerWithKey takes (a
// consumer's tenant_id is immutable, so the unlocked read stays true) — and
// returns the tenant's max_model_consumers.
func lockTenantOfConsumer(ctx context.Context, tx pgx.Tx, consumerID string, access ModelAccess) (*int, error) {
	admin, tenantArg := access.args()
	var tenant string
	if err := tx.QueryRow(ctx,
		`SELECT c.tenant_id::text FROM context_model_consumers c WHERE c.id = $1::uuid AND `+sqlConsumerAccess,
		consumerID, admin, tenantArg).Scan(&tenant); err != nil {
		if pgxdb.AbsentOrMalformed(err) {
			return nil, ErrModelConsumerNotFound
		}
		return nil, fmt.Errorf("model_consumers: resolve tenant: %w", err)
	}
	return lockTenantRow(ctx, tx, tenant, ErrModelConsumerNotFound)
}

// liveKeyIDsTx lists the keys the deactivation trigger is about to revoke.
func liveKeyIDsTx(ctx context.Context, tx pgx.Tx, consumerID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text FROM context_model_keys WHERE consumer_id = $1::uuid AND revoked_at IS NULL`, consumerID)
	if err != nil {
		return nil, fmt.Errorf("model_keys: live ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("model_keys: live ids: %w", err)
	}
	return ids, nil
}

// stampRevokedPrincipalTx records the stamped actor's principal on keys the
// deactivation trigger just revoked. revoked_by_principal is outside the
// audit trigger's column list, so this writes no audit row.
func stampRevokedPrincipalTx(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE context_model_keys SET revoked_by_principal = `+sqlActorPrincipal+`
		  WHERE id = ANY($1::uuid[]) AND revoked_at IS NOT NULL AND revoked_by_principal IS NULL`, ids); err != nil {
		return fmt.Errorf("model_keys: revoke principal: %w", err)
	}
	return nil
}

// writeConsumerTx applies the assignments (or, with none, reads the row) and
// returns the consumer.
func writeConsumerTx(ctx context.Context, tx pgx.Tx, consumerID string, set []string, args []any) (ModelConsumer, error) {
	var c ModelConsumer
	if len(set) == 0 {
		if err := tx.QueryRow(ctx, `SELECT `+sqlConsumerCols+` FROM context_model_consumers c WHERE c.id = $1::uuid`,
			consumerID).Scan(consumerDest(&c)...); err != nil {
			return ModelConsumer{}, fmt.Errorf("model_consumers: read: %w", err)
		}
		return c, nil
	}
	sql := `UPDATE context_model_consumers c SET ` + strings.Join(set, ", ") + `, updated_at = now()
	         WHERE c.id = $1::uuid RETURNING ` + sqlConsumerCols
	if err := tx.QueryRow(ctx, sql, append([]any{consumerID}, args...)...).Scan(consumerDest(&c)...); err != nil {
		if pgxdb.UniqueViolation(err) {
			return ModelConsumer{}, ErrModelConsumerLabelTaken
		}
		return ModelConsumer{}, fmt.Errorf("model_consumers: update: %w", err)
	}
	return c, nil
}

// assignments turns the patch into SET items with their arguments, numbered
// from $2 ($1 is the consumer id).
func (p ModelConsumerPatch) assignments() ([]string, []any, error) {
	if p.ClearTenantScope && p.TenantScope != nil {
		return nil, nil, errors.New("store: model consumer patch: tenant_scope set and cleared")
	}
	if p.ClearAllowedCIDRs && p.AllowedCIDRs != nil {
		return nil, nil, errors.New("store: model consumer patch: allowed_cidrs set and cleared")
	}
	if p.Label != nil && strings.TrimSpace(*p.Label) == "" {
		return nil, nil, errors.New("store: model consumer patch: label is required")
	}
	var (
		set  []string
		args []any
	)
	add := func(col, cast string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf("%s = $%d%s", col, len(args)+1, cast))
	}
	if p.Label != nil {
		add("label", "", *p.Label)
	}
	if p.Description != nil {
		add("description", "", *p.Description)
	}
	if p.TenantScope != nil || p.ClearTenantScope {
		add("tenant_scope", "::varchar", p.TenantScope)
	}
	if p.AllModels != nil {
		add("all_models", "", *p.AllModels)
	}
	if p.PromptSensitivity != nil {
		add("prompt_sensitivity", "", *p.PromptSensitivity)
	}
	if p.FairWeight != nil {
		add("fair_weight", "::smallint", *p.FairWeight)
	}
	if p.AdmissionLane != nil {
		add("admission_lane", "", *p.AdmissionLane)
	}
	if p.AllowedCIDRs != nil || p.ClearAllowedCIDRs {
		add("allowed_cidrs", "::cidr[]", p.AllowedCIDRs)
	}
	if p.AllowPlaintext != nil {
		add("allow_plaintext", "", *p.AllowPlaintext)
	}
	if p.Active != nil {
		add("active", "", *p.Active)
	}
	return set, args, nil
}

// ListConsumers lists the consumers in the caller's reach, newest first, in
// ONE statement: a LEFT JOIN LATERAL aggregates each consumer's keys
// (idx_model_keys_consumer_created) into its live keys (json_agg of the
// whitelisted key fields — no hash, no plaintext), the chain length and the
// latest use. Inactive consumers only with opts.IncludeInactive.
func ListConsumers(ctx context.Context, pool *pgxpool.Pool, access ModelAccess, opts ListConsumersOptions) ([]ModelConsumer, error) {
	admin, tenantArg := access.args()
	rows, err := pool.Query(ctx,
		`SELECT `+sqlConsumerCols+`, kk.live, kk.total, kk.last_used
		   FROM context_model_consumers c
		   LEFT JOIN LATERAL (
		        SELECT COALESCE(json_agg(json_build_object(
		                   'id', k.id, 'consumer_id', k.consumer_id, 'hint', k.key_hint,
		                   'created_at', k.created_at, 'expires_at', k.expires_at,
		                   'revoked_at', k.revoked_at, 'last_used_at', k.last_used_at,
		                   'rotated_from', k.rotated_from)
		                   ORDER BY k.created_at DESC, k.id DESC) FILTER (WHERE `+sqlKeyLive+`), '[]') AS live,
		               count(*)::int AS total,
		               max(k.last_used_at) AS last_used
		          FROM context_model_keys k WHERE k.consumer_id = c.id) kk ON true
		  WHERE `+sqlListAccess+`
		    AND ($3::boolean OR c.active)
		  ORDER BY c.created_at DESC, c.id DESC`,
		admin, tenantArg, opts.IncludeInactive)
	if err != nil {
		if pgxdb.MalformedUUID(err) {
			return []ModelConsumer{}, nil
		}
		return nil, fmt.Errorf("model_consumers: list: %w", err)
	}
	defer rows.Close()
	out := []ModelConsumer{}
	for rows.Next() {
		var (
			c    ModelConsumer
			live []byte
		)
		if err := rows.Scan(append(consumerDest(&c), &live, &c.KeysTotal, &c.LastUsedAt)...); err != nil {
			return nil, fmt.Errorf("model_consumers: list scan: %w", err)
		}
		if err := json.Unmarshal(live, &c.Keys); err != nil {
			return nil, fmt.Errorf("model_consumers: list keys: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		if pgxdb.MalformedUUID(err) {
			return []ModelConsumer{}, nil
		}
		return nil, fmt.Errorf("model_consumers: list: %w", err)
	}
	return out, nil
}

// KeyHistory pages through a consumer's keys, newest first, by the keyset
// (created_at, id) — revoked and expired ones included. before is the last
// key of the previous page (nil: the first page); limit lies in [1,
// MaxKeyHistoryLimit]. A consumer outside the caller's reach is
// ErrModelConsumerNotFound — one statement answers both questions.
func KeyHistory(ctx context.Context, pool *pgxpool.Pool, consumerID string, access ModelAccess,
	before *KeyCursor, limit int) ([]ModelKeyInfo, error) {

	if limit < 1 || limit > MaxKeyHistoryLimit {
		return nil, fmt.Errorf("store: key history: limit %d outside 1..%d", limit, MaxKeyHistoryLimit)
	}
	admin, tenantArg := access.args()
	var (
		curAt *time.Time
		curID *string
	)
	if before != nil {
		curAt, curID = &before.CreatedAt, &before.ID
	}
	rows, err := pool.Query(ctx,
		`SELECT kk.id, kk.consumer_id, kk.key_hint, kk.created_at, kk.expires_at,
		        kk.revoked_at, kk.last_used_at, kk.rotated_from
		   FROM context_model_consumers c
		   LEFT JOIN LATERAL (
		        SELECT `+sqlKeyCols+` FROM context_model_keys k
		         WHERE k.consumer_id = c.id
		           AND ($4::timestamptz IS NULL OR (k.created_at, k.id) < ($4::timestamptz, $5::uuid))
		         ORDER BY k.created_at DESC, k.id DESC
		         LIMIT $6) kk ON true
		  WHERE c.id = $1::uuid AND `+sqlConsumerAccess+`
		  ORDER BY kk.created_at DESC, kk.id DESC`,
		consumerID, admin, tenantArg, curAt, curID, limit)
	if err != nil {
		if pgxdb.MalformedUUID(err) {
			return nil, ErrModelConsumerNotFound
		}
		return nil, fmt.Errorf("model_keys: history: %w", err)
	}
	defer rows.Close()
	found := false
	out := []ModelKeyInfo{}
	for rows.Next() {
		found = true
		var (
			k                  ModelKeyInfo
			id, consumer, hint *string
			createdAt          *time.Time
		)
		if err := rows.Scan(&id, &consumer, &hint, &createdAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &k.RotatedFrom); err != nil {
			return nil, fmt.Errorf("model_keys: history scan: %w", err)
		}
		if id == nil {
			continue // the consumer exists and has no key on this page
		}
		k.ID, k.ConsumerID, k.Hint, k.CreatedAt = *id, *consumer, *hint, *createdAt
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		if pgxdb.MalformedUUID(err) {
			return nil, ErrModelConsumerNotFound
		}
		return nil, fmt.Errorf("model_keys: history: %w", err)
	}
	if !found {
		return nil, ErrModelConsumerNotFound
	}
	return out, nil
}

// TenantConsumerIDs returns the id of EVERY consumer of tenantID, active and
// inactive (its usage history includes calls of since-deactivated consumers) —
// the consumer half of TenantAPIKeyIDs, resolved to a literal uuid[] first so
// a filter reads consumer_id = ANY($ids) on its index. An empty tenantID
// yields an empty slice (fail-closed).
func TenantConsumerIDs(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]string, error) {
	if tenantID == "" {
		return []string{}, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT id::text FROM context_model_consumers WHERE tenant_id = $1::uuid`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("model_consumers: tenant consumer ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("model_consumers: tenant consumer ids: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// usageExecer is the handle the usage flush writes through: a pool.
type usageExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ModelKeyUsage batches the last_used_at stamp of model keys (design/01 §4.2):
// Touch records a use in memory — no statement, no row lock on the request
// path — and Flush writes every pending stamp in ONE statement. The flush is
// unstamped, and migration 161's UPDATE OF list leaves last_used_at out, so it
// writes no audit row. Safe for concurrent use; create it with
// NewModelKeyUsage (the zero value has no handle and no map).
type ModelKeyUsage struct {
	db       usageExecer
	now      func() time.Time
	interval time.Duration

	mu      sync.Mutex
	pending map[uuid.UUID]time.Time
}

// NewModelKeyUsage returns a usage batcher writing through pool.
func NewModelKeyUsage(pool *pgxpool.Pool) *ModelKeyUsage {
	return &ModelKeyUsage{db: pool, now: time.Now, interval: ModelKeyUsageFlushInterval,
		pending: map[uuid.UUID]time.Time{}}
}

// Touch records that keyID was used now. It keeps the latest time per key; a
// key id that is not a UUID is dropped (it names no row, and one malformed
// element would fail the whole batch statement).
func (u *ModelKeyUsage) Touch(keyID string) {
	id, err := uuid.Parse(keyID)
	if err != nil {
		return
	}
	at := u.now()
	u.mu.Lock()
	defer u.mu.Unlock()
	if prev, ok := u.pending[id]; !ok || at.After(prev) {
		u.pending[id] = at
	}
}

// Flush writes the pending stamps in one statement. The predicate keeps
// last_used_at monotonic: a stamp older than the stored one changes nothing,
// whatever order flushes of several processes land in. On failure the batch
// is merged back (the later time wins), so the next flush retries it.
func (u *ModelKeyUsage) Flush(ctx context.Context) error {
	u.mu.Lock()
	batch := u.pending
	u.pending = map[uuid.UUID]time.Time{}
	u.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(batch))
	for id := range batch {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	keys := make([]string, len(ids))
	stamps := make([]time.Time, len(ids))
	for i, id := range ids {
		keys[i], stamps[i] = id.String(), batch[id]
	}
	if _, err := u.db.Exec(ctx,
		`UPDATE context_model_keys k SET last_used_at = v.ts
		   FROM unnest($1::uuid[], $2::timestamptz[]) AS v(id, ts)
		  WHERE k.id = v.id AND (k.last_used_at IS NULL OR k.last_used_at < v.ts)`,
		keys, stamps); err != nil {
		u.mu.Lock()
		for id, at := range batch {
			if prev, ok := u.pending[id]; !ok || at.After(prev) {
				u.pending[id] = at
			}
		}
		u.mu.Unlock()
		return fmt.Errorf("model_keys: usage flush: %w", err)
	}
	return nil
}

// Run flushes every ModelKeyUsageFlushInterval until ctx ends, then once more
// on a context of its own bounded by modelKeyUsageFinalFlushTimeout, so an
// orderly shutdown loses no stamp. A failed flush is logged and retried on
// the next tick.
func (u *ModelKeyUsage) Run(ctx context.Context) {
	t := time.NewTicker(u.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelKeyUsageFinalFlushTimeout)
			if err := u.Flush(fctx); err != nil {
				slog.Warn("store: final model key usage flush failed", "err", err)
			}
			cancel()
			return
		case <-t.C:
			if err := u.Flush(ctx); err != nil {
				slog.Warn("store: model key usage flush failed", "err", err)
			}
		}
	}
}
