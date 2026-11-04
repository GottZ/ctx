package handler

// The read actions of the model consumer administration (design/05 §3.2,
// wave W05-02 folded into W01-5): model-consumer-list, model-consumer-get and
// model-key-history, and the one response shape they share (consumerView,
// keyView). No type here has a field for a key hash or plaintext — the
// secret is absent by structure, not by a filter (TestConsumerViewGoldenKeys).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// consumerView is one consumer as the administration answers it: the policy
// fields (design/01 K-05-2 — all_models instead of models[] until the grants
// of W01-9), the status (design/05 §3.2), the keys (the list: the live ones;
// get: the chain) and the peer binding, which only a server-admin sees.
type consumerView struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"tenant_id"`
	TenantSlug        string     `json:"tenant_slug"`
	Label             string     `json:"label"`
	Description       string     `json:"description"`
	Kind              string     `json:"kind"`
	TenantScope       *string    `json:"tenant_scope"`
	AllModels         bool       `json:"all_models"`
	PromptSensitivity string     `json:"prompt_sensitivity"`
	FairWeight        int16      `json:"fair_weight"`
	AdmissionLane     string     `json:"admission_lane"`
	AllowedCIDRs      []string   `json:"allowed_cidrs"`
	AllowPlaintext    bool       `json:"allow_plaintext"`
	Active            bool       `json:"active"`
	Status            string     `json:"status"`
	LastUsedAt        *time.Time `json:"last_used_at"`
	CreatedAt         time.Time  `json:"created_at"`
	CreatedByLabel    *string    `json:"created_by_label"`
	Keys              []keyView  `json:"keys"`
	KeysTotal         int        `json:"keys_total"`
	Peer              *peerView  `json:"peer"`
}

// keyView is one model key: its hint and lifecycle, never the secret.
type keyView struct {
	ID          string     `json:"id"`
	Hint        string     `json:"hint"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	RotatedFrom *string    `json:"rotated_from"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}

// peerView is the binding of a ctx-peer consumer to the instance that proved
// itself on first use — server-admin only (design/05 §3.2; the audit side is
// auditHiddenStateFields["model_consumer"]).
type peerView struct {
	InstanceID string    `json:"instance_id"`
	BoundAt    time.Time `json:"bound_at"`
}

// Key statuses (design/05 §3.2), in precedence order.
const (
	keyStatusRevoked    = "revoked"
	keyStatusExpired    = "expired"
	keyStatusSuperseded = "superseded"
	keyStatusExpiring   = "expiring"
	keyStatusActive     = "active"
)

// keyLive is I-K2's "live" at now.
func keyLive(k store.ModelKeyInfo, now time.Time) bool {
	return k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(now))
}

// consumerStatus is the Go form of the status table of design/05 §3.2 — the
// counterpart of store's SQL predicates, held against them at an injected now
// (TestConsumerStatusParity). keys may be any subset of the consumer's keys
// that contains all of its live ones (the list's live keys, get's chain, the
// whole history); liveness is judged here, not taken from the caller.
func consumerStatus(active bool, keys []store.ModelKeyInfo, now time.Time, warn time.Duration) string {
	if !active {
		return store.ConsumerStatusInactive
	}
	var live []store.ModelKeyInfo
	for _, k := range keys {
		if keyLive(k, now) {
			live = append(live, k)
		}
	}
	switch {
	case len(live) == 0:
		return store.ConsumerStatusNoKey
	case len(live) >= 2:
		return store.ConsumerStatusRotating
	case live[0].ExpiresAt != nil && !live[0].ExpiresAt.After(now.Add(warn)):
		return store.ConsumerStatusExpiring
	}
	return store.ConsumerStatusActive
}

// youngestLive returns the id of the newest live key ("" without one): every
// other live key is superseded — its grace is running.
func youngestLive(keys []store.ModelKeyInfo, now time.Time) string {
	best := -1
	for i, k := range keys {
		if !keyLive(k, now) {
			continue
		}
		if best < 0 || k.CreatedAt.After(keys[best].CreatedAt) ||
			(k.CreatedAt.Equal(keys[best].CreatedAt) && k.ID > keys[best].ID) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return keys[best].ID
}

// newKeyView renders one key; youngest is the consumer's newest live key.
func newKeyView(k store.ModelKeyInfo, youngest string, now time.Time, warn time.Duration) keyView {
	status := keyStatusActive
	switch {
	case k.RevokedAt != nil:
		status = keyStatusRevoked
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		status = keyStatusExpired
	case k.ID != youngest:
		status = keyStatusSuperseded
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now.Add(warn)):
		status = keyStatusExpiring
	}
	return keyView{ID: k.ID, Hint: k.Hint, Status: status, CreatedAt: k.CreatedAt,
		ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, RotatedFrom: k.RotatedFrom, LastUsedAt: k.LastUsedAt}
}

// newConsumerView renders c for the reader ar with keys shown (the live keys
// or the chain, newest first). The status and "superseded" come from c.Keys —
// ALL live keys as the store read them — never from the shown keys, which a
// chain cap may cut (review F3). Below server-admin the creator is masked as
// the audit read surface masks an actor (redactActor: only a non-server-admin
// key of the reader's own tenant keeps its label) and the peer binding is
// null.
func newConsumerView(c store.ModelConsumer, keys []store.ModelKeyInfo, ar *auth.AuthResult, now time.Time, warn time.Duration) consumerView {
	youngest := youngestLive(c.Keys, now)
	kv := make([]keyView, 0, len(keys))
	for _, k := range keys {
		kv = append(kv, newKeyView(k, youngest, now, warn))
	}
	v := consumerView{
		ID: c.ID, TenantID: c.TenantID, TenantSlug: c.TenantSlug, Label: c.Label,
		Description: c.Description, Kind: c.Kind, TenantScope: c.TenantScope, AllModels: c.AllModels,
		PromptSensitivity: c.PromptSensitivity, FairWeight: c.FairWeight, AdmissionLane: c.AdmissionLane,
		AllowedCIDRs: c.AllowedCIDRs, AllowPlaintext: c.AllowPlaintext, Active: c.Active,
		Status: consumerStatus(c.Active, c.Keys, now, warn), LastUsedAt: c.LastUsedAt,
		CreatedAt: c.CreatedAt, CreatedByLabel: c.CreatedByLabel, Keys: kv, KeysTotal: c.KeysTotal,
	}
	if ar.IsServerAdmin() {
		if c.PeerInstanceID != nil && c.PeerBoundAt != nil {
			v.Peer = &peerView{InstanceID: *c.PeerInstanceID, BoundAt: *c.PeerBoundAt}
		}
		return v
	}
	ownCreator := !c.CreatedByServerAdmin && c.CreatedByTenantID != nil && *c.CreatedByTenantID == ar.TenantID
	if c.CreatedByLabel != nil && !ownCreator {
		label := auditRedactedActorLabel
		v.CreatedByLabel = &label
	}
	return v
}

// encodeKeysetCursor makes the opaque keyset token of a page: the (created_at, id)
// of its last row.
func encodeKeysetCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

// errBadCursor is the 400 of a cursor the server did not hand out.
var errBadCursor = errors.New("invalid cursor")

func decodeKeysetCursor(s string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", errBadCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", errBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", errBadCursor
	}
	if _, err := uuid.Parse(id); err != nil {
		return time.Time{}, "", errBadCursor
	}
	return at, id, nil
}

// pageLimit resolves a requested page size against the cap: absent = the cap,
// above it = the cap (precedent GET /api/audit), below 1 = an error.
func pageLimit(requested *int, max int) (int, error) {
	if requested == nil {
		return max, nil
	}
	if *requested < 1 {
		return 0, errors.New("limit must be a positive integer")
	}
	return min(*requested, max), nil
}

// consumerListRequest is the payload of model-consumer-list (design/05 §3.2).
// status: the statuses to list, or ["all"]; absent = the four statuses of an
// active consumer. q: label prefix. tenant_id: server-admin only.
type consumerListRequest struct {
	TenantID string   `json:"tenant_id"`
	Status   []string `json:"status"`
	Q        string   `json:"q"`
	Cursor   string   `json:"cursor"`
	Limit    *int     `json:"limit"`
}

// listOptions turns the request into store options (no database).
func (d consumerListRequest) listOptions(pol consumerPolicy, now time.Time) (store.ListConsumersOptions, int, error) {
	opts := store.ListConsumersOptions{Now: now, Warn: pol.expiryWarn}
	known := map[string]bool{}
	for _, s := range store.ConsumerStatuses() {
		known[s] = true
	}
	for _, s := range d.Status {
		switch {
		case s == "all":
			opts.Statuses = store.ConsumerStatuses()
		case known[s]:
			opts.Statuses = append(opts.Statuses, s)
		default:
			return opts, 0, errors.New("status must list values of active, expiring, rotating, no_key, inactive, or all")
		}
	}
	if utf8.RuneCountInString(d.Q) > consumerLabelMaxRunes {
		return opts, 0, errors.New("q is longer than any label")
	}
	opts.LabelPrefix = d.Q
	if d.Cursor != "" {
		at, id, err := decodeKeysetCursor(d.Cursor)
		if err != nil {
			return opts, 0, err
		}
		opts.After = &store.ConsumerCursor{CreatedAt: at, ID: id}
	}
	limit, err := pageLimit(d.Limit, pol.listMax)
	if err != nil {
		return opts, 0, err
	}
	opts.Limit = limit + 1 // one more: is there a next page?
	return opts, limit, nil
}

func (h *ManageHandler) handleModelConsumerList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d consumerListRequest
	if err := decodeConsumerPayload(req.Data, &d); err != nil {
		writeConsumerError(w, http.StatusBadRequest, "invalid data: "+err.Error())
		return
	}
	access := accessOf(ar)
	if d.TenantID != "" {
		if !ar.IsServerAdmin() {
			writeConsumerError(w, http.StatusForbidden, "tenant_id is server-admin only")
			return
		}
		access = store.ModelAccess{TenantID: d.TenantID}
	}
	pol := h.consumerPolicy()
	now := time.Now()
	opts, limit, err := d.listOptions(pol, now)
	if err != nil {
		writeConsumerError(w, http.StatusBadRequest, err.Error())
		return
	}
	list, err := store.ListConsumers(r.Context(), h.pool, access, opts)
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-list")
		return
	}
	var next any // JSON null on the last page
	if len(list) > limit {
		list = list[:limit]
		last := list[len(list)-1]
		next = encodeKeysetCursor(last.CreatedAt, last.ID)
	}
	views := make([]consumerView, 0, len(list))
	for _, c := range list {
		views = append(views, newConsumerView(c, c.Keys, ar, now, pol.expiryWarn))
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "consumers": views, "next_cursor": next})
}

// consumerAuditRowJSON is one row of a consumer's audit trail: the row of
// GET /api/audit plus its entity type (the trail mixes the consumer's and its
// keys' rows).
type consumerAuditRowJSON struct {
	EntityType string `json:"entity_type"`
	auditRowJSON
}

func (h *ManageHandler) handleModelConsumerGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d struct {
		ID string `json:"id"`
	}
	if err := decodeConsumerPayload(req.Data, &d); err != nil || d.ID == "" {
		writeConsumerError(w, http.StatusBadRequest, "id is required")
		return
	}
	pol := h.consumerPolicy()
	access := accessOf(ar)
	now := time.Now()
	c, err := store.GetConsumer(r.Context(), h.pool, d.ID, access, now)
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-get")
		return
	}
	// The chain through the consumer join (store.KeyHistory, V-01-g): the
	// key table has no tenant column, isolation is the join.
	chain, err := store.KeyHistory(r.Context(), h.pool, c.ID, access, nil, pol.chainMax+1)
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-get")
		return
	}
	truncated := len(chain) > pol.chainMax
	if truncated {
		chain = chain[:pol.chainMax]
	}
	trail, err := h.consumerAuditTrail(r, ar, c.ID, chain, pol.auditMax)
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-consumer-get")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"consumer":       newConsumerView(c, chain, ar, now, pol.expiryWarn),
		"keys_truncated": truncated,
		"audit":          trail,
	})
}

// consumerAuditTrail reads the newest audit rows of a consumer and the keys
// of its chain and puts each through the redaction of GET /api/audit — the
// same three functions, per row with the row's entity type: foreign and
// server-admin actors masked (redactActor), hidden state fields dropped
// (redactKeyState), updates whose only change was hidden left out
// (auditInvisibleUpdate; such a trail may hold fewer than the cap).
func (h *ManageHandler) consumerAuditTrail(r *http.Request, ar *auth.AuthResult, consumerID string, chain []store.ModelKeyInfo, limit int) ([]consumerAuditRowJSON, error) {
	ids := make([]string, 0, len(chain)+1)
	ids = append(ids, consumerID)
	for _, k := range chain {
		ids = append(ids, k.ID)
	}
	isAdmin := ar.IsServerAdmin()
	rows, err := store.ConsumerAuditTrail(r.Context(), h.pool, ids, isAdmin, ar.TenantID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]consumerAuditRowJSON, 0, len(rows))
	for _, row := range rows {
		jr := auditRowJSON{
			ID: row.ID, EntityKey: row.EntityKey, Scope: row.Scope, Action: row.Action,
			OldValue: row.OldValue, NewValue: row.NewValue,
			APIKeyID: row.APIKeyID, ActorLabel: row.ActorLabel, CreatedAt: row.CreatedAt,
		}
		if err := json.Unmarshal(row.Metadata, &jr.Metadata); err != nil || jr.Metadata == nil {
			jr.Metadata = map[string]json.RawMessage{}
		}
		jr = redactKeyState(row.EntityType, redactActor(jr, ar.TenantID, isAdmin), isAdmin)
		if auditInvisibleUpdate(row.EntityType, jr, isAdmin) {
			continue
		}
		out = append(out, consumerAuditRowJSON{EntityType: row.EntityType, auditRowJSON: jr})
	}
	return out, nil
}

func (h *ManageHandler) handleModelKeyHistory(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d struct {
		ConsumerID string `json:"consumer_id"`
		Cursor     string `json:"cursor"`
		Limit      *int   `json:"limit"`
	}
	if err := decodeConsumerPayload(req.Data, &d); err != nil {
		writeConsumerError(w, http.StatusBadRequest, "invalid data: "+err.Error())
		return
	}
	if d.ConsumerID == "" {
		writeConsumerError(w, http.StatusBadRequest, "consumer_id is required")
		return
	}
	pol := h.consumerPolicy()
	limit, err := pageLimit(d.Limit, pol.listMax)
	if err != nil {
		writeConsumerError(w, http.StatusBadRequest, err.Error())
		return
	}
	var before *store.KeyCursor
	if d.Cursor != "" {
		at, id, err := decodeKeysetCursor(d.Cursor)
		if err != nil {
			writeConsumerError(w, http.StatusBadRequest, err.Error())
			return
		}
		before = &store.KeyCursor{CreatedAt: at, ID: id}
	}
	access := accessOf(ar)
	now := time.Now()
	// The consumer's live keys decide "superseded" on every page, not only
	// on the page that happens to hold the newest key.
	c, err := store.GetConsumer(r.Context(), h.pool, d.ConsumerID, access, now)
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-key-history")
		return
	}
	keys, err := store.KeyHistory(r.Context(), h.pool, c.ID, access, before, limit+1)
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-key-history")
		return
	}
	var next any
	if len(keys) > limit {
		keys = keys[:limit]
		last := keys[len(keys)-1]
		next = encodeKeysetCursor(last.CreatedAt, last.ID)
	}
	youngest := youngestLive(c.Keys, now)
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, newKeyView(k, youngest, now, pol.expiryWarn))
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "keys": views, "next_cursor": next})
}
