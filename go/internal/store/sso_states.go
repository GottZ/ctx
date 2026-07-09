package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SSO state purposes (101, design 04 §3.3). The value list is closed HERE,
// not by a DB CHECK — StoreSSOState rejects anything else and ConsumeSSOState
// matches the purpose exactly, so a login state can never authorize an
// account link and vice versa (cross-use guard, design 04 F1).
const (
	SSOPurposeLogin = "login"
	SSOPurposeLink  = "link"
)

// SSOStateData is the sealed per-login payload of one context_sso_states row
// (design 04 §3.3). State is the OAuth `state` URL param — a SECOND uuid,
// deliberately distinct from the row id (double-UUID principle, 101 header):
// the row id lives only in the httpOnly cookie and is the consume key; the
// callback then compares the URL `state` against this field. ProviderSlug
// binds the callback to the provider chosen at login (mix-up defense, F2).
type SSOStateData struct {
	ProviderSlug string `json:"provider_slug"`
	State        string `json:"state"`
	PKCEVerifier string `json:"pkce_verifier"`
	Nonce        string `json:"nonce"`
	ReturnTo     string `json:"return_to"`
}

// StoreSSOState persists one single-use SSO state row and returns the opaque
// row id that becomes the httpOnly cookie value. The id is generated HERE
// (random v4) and is NEVER the OAuth `state` param — collapsing the two would
// put the consume key into IdP logs/referrers (101 header). Port of the
// serviceportal storeSSOState pattern onto context_sso_states.
func StoreSSOState(ctx context.Context, pool *pgxpool.Pool, purpose string, data SSOStateData, ttl time.Duration) (string, error) {
	if purpose != SSOPurposeLogin && purpose != SSOPurposeLink {
		return "", fmt.Errorf("sso states: invalid purpose %q", purpose)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("sso states: marshal: %w", err)
	}
	id := uuid.NewString()
	_, err = pool.Exec(ctx,
		`INSERT INTO context_sso_states (id, purpose, state_data, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		id, purpose, payload, time.Now().Add(ttl),
	)
	if err != nil {
		return "", fmt.Errorf("sso states: put: %w", err)
	}
	return id, nil
}

// ConsumeSSOState atomically consumes one state row (single use): DELETE …
// RETURNING guarantees that of two racing callbacks exactly one gets the row
// — same HA-safe pattern as TakeOAuthCode. id is the COOKIE value (row id),
// never the URL `state` param. A miss returns (nil, nil) and deliberately
// does not distinguish unknown/replayed id, wrong purpose, or expiry — the
// caller answers with one generic reject, no oracle for which check failed.
func ConsumeSSOState(ctx context.Context, pool *pgxpool.Pool, id, purpose string) (*SSOStateData, error) {
	// A non-UUID cookie value would raise 22P02 on the uuid column — a
	// distinguishable error path. Fold it into the same silent miss.
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil //nolint:nilerr // deliberate: malformed id folds into the same silent miss (no oracle)
	}
	var payload []byte
	err := pool.QueryRow(ctx,
		`DELETE FROM context_sso_states
		  WHERE id = $1 AND purpose = $2 AND expires_at > now()
		 RETURNING state_data`,
		id, purpose,
	).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sso states: consume: %w", err)
	}
	var data SSOStateData
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, fmt.Errorf("sso states: unmarshal: %w", err)
	}
	return &data, nil
}

// EvictExpiredSSOStates deletes states past their expiry plus a grace hour
// (scheduler janitor tick, analog EvictExpiredOAuthCodes). The grace keeps
// very recent expiries around for debugging; correctness never depends on
// this sweep — ConsumeSSOState checks expires_at itself, an expired state
// stays a miss regardless.
func EvictExpiredSSOStates(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_sso_states WHERE expires_at < now() - interval '1 hour'`)
	if err != nil {
		return 0, fmt.Errorf("sso states: evict: %w", err)
	}
	return tag.RowsAffected(), nil
}
