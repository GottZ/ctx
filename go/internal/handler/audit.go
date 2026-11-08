package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/usageevents"
)

// AuditHandler serves GET /api/audit — the read surface of
// context_settings_audit for the entity types store.AuditEntities names
// (design/06 §4.10). Mounted behind RequireAdminOrTenantAdmin; the handler
// scopes the rows to the caller's tenant and redacts foreign actors.
type AuditHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
}

// NewAuditHandler creates the GET /api/audit handler.
func NewAuditHandler(pool *pgxpool.Pool, cfg ConfigStore) *AuditHandler {
	return &AuditHandler{pool: pool, cfg: cfg}
}

const (
	// auditRedactedActorLabel replaces the label of an actor a tenant-admin
	// may not see (design/06 §4.10, B-06-23). It is a mask, not a claim: a
	// foreign tenant's actor reads the same.
	auditRedactedActorLabel = "server-admin"
	// auditActorMetaPrefix marks the actor-identity keys of an audit row's
	// metadata (audit_actor_meta, migration 160: actor_principal,
	// actor_tenant_id, actor_server_admin). A reader below server-admin gets
	// none of them except actor_principal of an actor of its own tenant —
	// an identity field a later writer adds under this prefix is withheld
	// from tenant-admins without a change here. Matched case-insensitively.
	auditActorMetaPrefix = "actor_"
)

// auditDefaultMaxLimit is the registry default of audit.max_limit, read once:
// the fallback when the configured cap is 0.
var auditDefaultMaxLimit = sync.OnceValue(func() int { return config.Defaults().Audit.MaxLimit })

// auditRowJSON is one row of the GET /api/audit response.
type auditRowJSON struct {
	ID         string                     `json:"id"`
	EntityKey  string                     `json:"entity_key"`
	Scope      string                     `json:"scope"`
	Action     string                     `json:"action"`
	OldValue   json.RawMessage            `json:"old_value"`
	NewValue   json.RawMessage            `json:"new_value"`
	APIKeyID   *string                    `json:"api_key_id"`
	ActorLabel *string                    `json:"actor_label"`
	Metadata   map[string]json.RawMessage `json:"metadata"`
	CreatedAt  time.Time                  `json:"created_at"`
}

// HandleAudit serves GET /api/audit?entity=…&id=…&since=…&limit=….
//
//   - entity (required): one of store.AuditEntities(); missing, unknown, or a
//     type with its own surface (setting, secret, backend) ⇒ 400.
//   - id: exact entity_key (a key id, a quota scope, …); not valid UTF-8 or
//     carrying NUL ⇒ 400.
//   - since: RFC 3339; rows strictly newer. Malformed ⇒ 400.
//   - limit: positive integer, clamped to audit.max_limit, which is also the
//     page size when absent. Non-numeric or < 1 ⇒ 400.
//
// A server-admin reads every row as stored. A tenant-admin reads the rows of
// its own tenant (metadata.tenant_id, filtered in SQL) with foreign actors
// masked (redactActor), the server-admin bit and person of a key left out of
// its state (redactKeyState), and an update whose only change lay in those
// fields left out entirely (auditInvisibleUpdate — so a page may hold fewer
// than limit rows); a foreign or unknown id reads as rows: [] (B-06-3),
// and so does the server-admin-only entity instance — the group's convention
// is a reduced view, not a role-specific error (HandleStatus, HandleLLMLog).
// A server-admin reads a model_consumer row of the TOFU peer binding with the
// action "peer_bound" instead of the trigger's "update" (auditPeerBoundLabel,
// W06-18); a tenant-admin never gets that row (auditInvisibleUpdate).
func (h *AuditHandler) HandleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entity := q.Get("entity")
	if entity == "" {
		auditBadRequest(w, "entity is required (one of: "+strings.Join(store.AuditEntities(), ", ")+")")
		return
	}
	cfg := h.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: audit.max_limit is a global-only read cap of a gated endpoint, not tenant-scoped.
	maxLimit := cfg.Audit.MaxLimit
	if maxLimit < 1 {
		// 0 is not a cap: fall back to the registry default (V35 refuses
		// negatives at boot and on the settings write).
		maxLimit = auditDefaultMaxLimit()
	}
	limit := maxLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			auditBadRequest(w, "limit must be a positive integer")
			return
		}
		limit = min(n, maxLimit)
	}
	var since time.Time
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			auditBadRequest(w, "since must be an RFC 3339 timestamp")
			return
		}
		since = t
	}

	ar := AuthResultFromContext(r.Context())
	readerIsServerAdmin := ar.IsServerAdmin()
	readerTenant := ""
	if ar != nil {
		readerTenant = ar.TenantID
	}
	id := q.Get("id")
	if !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		// The database refuses such text (22021); answer it as the caller's
		// error, the same for every role, instead of a logged 500.
		auditBadRequest(w, "id must be valid UTF-8 text without NUL")
		return
	}
	aq := store.AuditQuery{Entity: entity, EntityKey: id, Since: since, Limit: limit}
	if readerIsServerAdmin {
		aq.AllTenants = true
	} else {
		aq.TenantID = readerTenant
	}

	rows, err := store.ListAudit(r.Context(), h.pool, aq)
	if errors.Is(err, store.ErrAuditEntity) {
		auditBadRequest(w, "entity is not served here (one of: "+strings.Join(store.AuditEntities(), ", ")+"; settings history: GET /api/settings/{key})")
		return
	}
	if err != nil {
		slog.Error("audit: list failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	out := make([]auditRowJSON, 0, len(rows))
	for _, row := range rows {
		jr := auditRowJSON{
			ID: row.ID, EntityKey: row.EntityKey, Scope: row.Scope, Action: row.Action,
			OldValue: row.OldValue, NewValue: row.NewValue,
			APIKeyID: row.APIKeyID, ActorLabel: row.ActorLabel, CreatedAt: row.CreatedAt,
		}
		if err := json.Unmarshal(row.Metadata, &jr.Metadata); err != nil || jr.Metadata == nil {
			// Not an object (only a hand-written row can do that): serve it
			// as {} — redactActor then cannot prove an own actor and masks.
			slog.Warn("audit: metadata is not a JSON object", "id", row.ID, "error", err)
			jr.Metadata = map[string]json.RawMessage{}
		}
		jr = redactKeyState(entity, redactActor(jr, readerTenant, readerIsServerAdmin), readerIsServerAdmin)
		if auditInvisibleUpdate(entity, jr, readerIsServerAdmin) {
			// Left out AFTER the SQL LIMIT: such a page can hold fewer than
			// limit rows while older rows exist (documented in docs/api.md).
			continue
		}
		// The label goes on after the visibility decision: a relabelled
		// binding must never leave auditInvisibleUpdate's "update" class.
		out = append(out, auditPeerBoundLabel(entity, jr, readerIsServerAdmin))
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "entity": entity, "rows": out})
}

func auditBadRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": msg})
}

// redactActor applies the actor redaction of design/06 §4.10 (B-06-23) to one
// row for one reader. Pure: it never mutates row and reads no state.
//
// A server-admin reader gets the row unchanged. For every other reader:
//
//   - the actor_* metadata keys are dropped — actor_tenant_id and
//     actor_server_admin are the inputs of this decision, not output;
//   - an actor is kept only on proof that it belongs to the reader's own
//     tenant: actor_server_admin is exactly false AND actor_tenant_id names
//     the reader's tenant. Then actor_label, api_key_id and
//     metadata.actor_principal stay;
//   - any other actor — a server-admin (wherever its key lives), a foreign
//     tenant, or an actor whose identity fields are missing or malformed —
//     is masked: actor_label "server-admin", api_key_id null, no principal;
//   - a row without an actor (via sql/migration/bootstrap) stays without one.
func redactActor(row auditRowJSON, readerTenant string, readerIsServerAdmin bool) auditRowJSON {
	if readerIsServerAdmin {
		return row
	}
	out := row
	out.Metadata = make(map[string]json.RawMessage, len(row.Metadata))
	hasActor := row.APIKeyID != nil || row.ActorLabel != nil
	for k, v := range row.Metadata {
		if strings.HasPrefix(strings.ToLower(k), auditActorMetaPrefix) {
			hasActor = true
			continue
		}
		out.Metadata[k] = v
	}
	if !hasActor {
		return out
	}
	if auditActorIsOwn(row.Metadata, readerTenant) {
		if p, ok := row.Metadata["actor_principal"]; ok {
			out.Metadata["actor_principal"] = p
		}
		return out
	}
	label := auditRedactedActorLabel
	out.ActorLabel = &label
	out.APIKeyID = nil
	return out
}

// auditActorIsOwn: the row's actor provably is a non-server-admin key of the
// reader's tenant. Everything short of that proof is false (fail-closed).
func auditActorIsOwn(md map[string]json.RawMessage, readerTenant string) bool {
	reader, err := uuid.Parse(readerTenant)
	if err != nil {
		return false
	}
	// Exactly the JSON literal false: a missing key, null (which would decode
	// to false), a string or true all fail the proof.
	if raw, ok := md["actor_server_admin"]; !ok || strings.TrimSpace(string(raw)) != "false" {
		return false
	}
	var actorTenant string
	if raw, ok := md["actor_tenant_id"]; !ok || json.Unmarshal(raw, &actorTenant) != nil {
		return false
	}
	actor, err := uuid.Parse(actorTenant)
	return err == nil && actor == reader
}

// auditHiddenStateFields are the fields of an entity's old_value/new_value a
// reader below server-admin never gets — data, keyed by entity type. For
// api_key: api-key-list shows a tenant-admin neither the server-admin bit nor
// the key's person, and a server-admin key living in the reader's tenant would
// otherwise be named by both (B-06-23 on the state side, Fremd-Review #1).
// model_consumer: the peer binding (peer_instance_id, peer_bound_at) — the
// consumer view shows it to a server-admin only (design/05 §3.2, W01-5), so a
// tenant-admin must not read it here or in the audit trail of
// model-consumer-get either; a binding-only update is then invisible to it.
// The whitelist of migration 161 leaves out every actor and person column
// (created_by_*, revoked_by_*) already, which is why model_key needs no entry.
// model_alias needs none either: its whitelist is the alias
// definition itself (scope, name, kind, role, sensitivity, policy,
// description, enabled), which a tenant-admin manages for its own scope.
var auditHiddenStateFields = map[string][]string{
	"api_key":        {"is_admin", "principal_id"},
	"model_consumer": {"peer_instance_id", "peer_bound_at"},
}

// redactKeyState drops auditHiddenStateFields[entity] from old_value and
// new_value for a reader below server-admin. Pure. The fields go on every row
// of the entity, not only where they would reveal something — a gap only on
// admin keys would itself be the signal. A state that is not a JSON object
// (only a hand-written row) cannot be filtered and reads as null.
func redactKeyState(entity string, row auditRowJSON, readerIsServerAdmin bool) auditRowJSON {
	hidden := auditHiddenStateFields[entity]
	if readerIsServerAdmin || len(hidden) == 0 {
		return row
	}
	out := row
	out.OldValue = auditDropFields(row.OldValue, hidden)
	out.NewValue = auditDropFields(row.NewValue, hidden)
	return out
}

func auditDropFields(v json.RawMessage, fields []string) json.RawMessage {
	if v == nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(v, &m); err != nil || m == nil {
		return nil
	}
	for _, f := range fields {
		delete(m, f)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// auditInvisibleUpdate reports whether a reader below server-admin must not
// get this row at all: an update of an entity with hidden state fields whose
// redacted old_value and new_value are equal. Its change lay only in fields
// redactKeyState removed (a psql promotion to server-admin, a principal
// swap), so the row would itself say "a hidden field changed". Pure. Equality
// is semantic — decoded JSON compared as values, numbers exactly — not byte
// equality. Only "update": every other action carries its own meaning.
func auditInvisibleUpdate(entity string, row auditRowJSON, readerIsServerAdmin bool) bool {
	if readerIsServerAdmin || row.Action != "update" || len(auditHiddenStateFields[entity]) == 0 {
		return false
	}
	return auditJSONEqual(row.OldValue, row.NewValue)
}

// auditPeerBoundLabel relabels, for a server-admin reader only, the
// model_consumer "update" row of the federation's TOFU binding (cluster C3,
// F-W12) as usageevents.KindPeerBound: old_value.peer_instance_id null or
// absent, new_value.peer_instance_id set. The migration-161 trigger writes the
// binding as a plain update in the binding's own statement and the
// usageevents peer_bound hook adds no second row (design/06 §4.10), so the
// label is the read side's. Pure. Any other reader, entity, action or state
// returns row unchanged — for a tenant-admin the binding is a hidden-field
// change and the row is not served at all (auditInvisibleUpdate).
func auditPeerBoundLabel(entity string, row auditRowJSON, readerIsServerAdmin bool) auditRowJSON {
	if !readerIsServerAdmin || entity != "model_consumer" || row.Action != "update" {
		return row
	}
	if auditStatePeerID(row.OldValue) != "" || auditStatePeerID(row.NewValue) == "" {
		return row
	}
	out := row
	out.Action = string(usageevents.KindPeerBound)
	return out
}

// auditStatePeerID reads peer_instance_id from a model_consumer state; ""
// when the state is absent, not an object, or the field is absent, null or
// not a string.
func auditStatePeerID(state json.RawMessage) string {
	var m struct {
		PeerInstanceID *string `json:"peer_instance_id"`
	}
	if len(state) == 0 || json.Unmarshal(state, &m) != nil || m.PeerInstanceID == nil {
		return ""
	}
	return *m.PeerInstanceID
}

// auditJSONEqual compares two JSON documents as values; two absent (null)
// documents are equal, an undecodable one equals nothing.
func auditJSONEqual(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, okA := auditDecode(a)
	vb, okB := auditDecode(b)
	return okA && okB && reflect.DeepEqual(va, vb)
}

func auditDecode(raw json.RawMessage) (any, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, false
	}
	return auditNormNumbers(v), true
}

// auditNormNumbers turns json.Number into a canonical decimal string, so 1 and
// 1.0 compare equal without the precision loss of float64.
func auditNormNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if r, ok := new(big.Rat).SetString(string(x)); ok {
			return r.RatString()
		}
		return string(x)
	case map[string]any:
		for k, e := range x {
			x[k] = auditNormNumbers(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = auditNormNumbers(e)
		}
		return x
	}
	return v
}
