package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ConsumerKind is the kind of a model consumer (context_model_consumers.kind).
type ConsumerKind string

const (
	// KindApp is an external program; every unknown kind counts as app.
	KindApp ConsumerKind = "app"
	// KindCtxPeer is another ctx instance; what that allows beyond an app is
	// decided by the admission and federation layers (design/03, design/04).
	KindCtxPeer ConsumerKind = "ctx-peer"
)

// ErrCredentialRevoked is the cancel cause of a running /v1 request whose
// model key was revoked or expired (W01-8, K-02-7).
var ErrCredentialRevoked = errors.New("auth: model credential revoked or expired")

// ErrNoModelCredential: ReauthenticateModelKey got a ConsumerAuth that
// AuthenticateModelKey did not produce.
var ErrNoModelCredential = errors.New("auth: consumer auth carries no model credential")

// ConsumerAuth is the outcome of a model-key check (design/01 §4.4): every
// column of ctx_model_auth (migration 161, K20). It is NOT an AuthResult and
// never turns into one (masterplan I-K0): no scope, no admin tier, no
// person. No exported field carries the key, its hint or its hash.
type ConsumerAuth struct {
	ModelKeyID        string       // context_model_keys.id — the credential (forensics, rotation)
	ConsumerID        string       // context_model_consumers.id — the stable caller identity
	TenantID          string       // attribution, quota key (K-06-2), external tenant ceiling (03)
	TenantScope       string       // "" ⇒ only _global backends (backends.VisibleTo fails closed)
	Kind              ConsumerKind // unknown values map to KindApp
	AllModels         bool         // true ⇒ every visible alias; otherwise grants (W01-9)
	PromptSensitivity string       // empty/unknown ⇒ credentials at the consumer (trust.go)
	FairWeight        int          // 1..100
	KeyExpiresAt      *time.Time   // per-key expiry timer (W01-8); nil = no expiry
	IsValid           bool
	AdmissionLane     string // 'urgent' | 'bulk' — the external lane ceiling (design/03)
	PeerInstanceID    string // a ctx-peer's bound instance id; "" = not bound

	// cred is what ReauthenticateModelKey re-checks: the key hash and the
	// address and transport of the request that authenticated. A closure
	// on purpose: fmt formats an unexported field by reflection, and a
	// pointer to a struct is dereferenced on fmt's bad-verb path (%s of a
	// ConsumerAuth printed the hash in the first draft of this wave), while
	// a func value prints as an address under every verb and
	// reflection-based dumpers cannot look inside it; encoding/json skips
	// the field.
	cred func() modelCredential
}

type modelCredential struct {
	hash   string
	ip     netip.Addr
	secure bool
}

// credentialOf captures the re-check input in a closure (see
// ConsumerAuth.cred).
func credentialOf(hash string, ip netip.Addr, secure bool) func() modelCredential {
	return func() modelCredential { return modelCredential{hash: hash, ip: ip, secure: secure} }
}

// consumerKind maps the kind column; anything but 'ctx-peer' is an app.
func consumerKind(s string) ConsumerKind {
	if s == string(KindCtxPeer) {
		return KindCtxPeer
	}
	return KindApp
}

// addrArg is the INET argument of ctx_model_auth: an invalid address binds
// SQL NULL (gate 5 then passes only a consumer without allowlist), an
// IPv4-mapped IPv6 address binds as IPv4 (inet '::ffff:10.1.2.3' is not
// inside cidr '10.0.0.0/8'), and a zone is dropped (inet has none).
func addrArg(ip netip.Addr) any {
	if !ip.IsValid() {
		return nil
	}
	return ip.Unmap().WithZone("")
}

// AuthenticateModelKey checks a model key by its hash against
// ctx_model_auth($1,$2,$3) — the one database touch of the /v1 door
// (design/01 §3.4, §4.2 stage 7). ip is the client address as the door
// determined it (invalid ⇒ SQL NULL), secure whether the request arrived
// over TLS. An empty hash and every refusing gate yield a plain invalid
// result with no other field set. The function is read-only (STABLE): it
// stamps no last_used_at — that is the caller's batched Touch.
func AuthenticateModelKey(ctx context.Context, pool *pgxpool.Pool, keyHash string, ip netip.Addr, secure bool) (*ConsumerAuth, error) {
	if keyHash == "" {
		return &ConsumerAuth{IsValid: false}, nil
	}
	var (
		keyID, consumerID, tenantID, scope, peer *string
		kind, sens, lane                         string
		all, valid                               bool
		weight                                   int16
		expires                                  *time.Time
	)
	// Columns by name and in the function's order (K20: new columns go last).
	err := pool.QueryRow(ctx,
		`SELECT model_key_id, consumer_id, tenant_id, tenant_scope, kind, all_models, prompt_sensitivity, fair_weight, key_expires_at, is_valid, admission_lane, peer_instance_id FROM ctx_model_auth($1, $2, $3)`,
		keyHash, addrArg(ip), secure,
	).Scan(&keyID, &consumerID, &tenantID, &scope, &kind, &all, &sens, &weight, &expires, &valid, &lane, &peer)
	if err != nil {
		return nil, fmt.Errorf("auth: query ctx_model_auth: %w", err)
	}
	if !valid {
		return &ConsumerAuth{IsValid: false}, nil
	}
	if keyID == nil || consumerID == nil || tenantID == nil {
		// A valid row always names key, consumer and tenant (NOT NULL
		// columns); anything else is a broken function, not a credential.
		return nil, errors.New("auth: ctx_model_auth returned a valid row without key, consumer or tenant id")
	}
	ca := &ConsumerAuth{
		ModelKeyID:        *keyID,
		ConsumerID:        *consumerID,
		TenantID:          *tenantID,
		Kind:              consumerKind(kind),
		AllModels:         all,
		PromptSensitivity: sens,
		FairWeight:        int(weight),
		KeyExpiresAt:      expires,
		IsValid:           true,
		AdmissionLane:     lane,
		cred:              credentialOf(keyHash, ip, secure),
	}
	if scope != nil {
		ca.TenantScope = *scope
	}
	if peer != nil {
		ca.PeerInstanceID = *peer
	}
	return ca, nil
}

// ReauthenticateModelKey runs the same check again for the credential
// behind ca, with the address and transport of the request that produced it
// (K22: the stream watcher's re-check until W01-8's revocation registry
// replaces it). ca must come from AuthenticateModelKey; anything else is
// ErrNoModelCredential. The caller bounds the database share (the door's
// semaphore, handler.ReauthConsumer).
func ReauthenticateModelKey(ctx context.Context, pool *pgxpool.Pool, ca *ConsumerAuth) (*ConsumerAuth, error) {
	if ca == nil || ca.cred == nil {
		return nil, ErrNoModelCredential
	}
	c := ca.cred()
	return AuthenticateModelKey(ctx, pool, c.hash, c.ip, c.secure)
}
