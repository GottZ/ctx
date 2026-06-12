package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the minimal pgx surface the pool reads rows through.
// *pgxpool.Pool satisfies it; tests inject fakes.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SecretResolver resolves an F2 secret name to its plaintext. Errors must be
// value-free (store.ResolveSecret contract). The pool never persists the
// resolved value — it lives in the in-memory snapshot only.
type SecretResolver func(ctx context.Context, name string) (string, error)

// GamingState is the chain-time exclusion input (P6 wires it to the F2
// settings keys gaming.active / gaming.disabled_backends; until then callers
// pass the zero value = inactive).
type GamingState struct {
	Active            bool
	DisabledBackends  []string
}

func (g GamingState) disables(name string) bool {
	if !g.Active {
		return false
	}
	for _, n := range g.DisabledBackends {
		if n == name {
			return true
		}
	}
	return false
}

// ExclusionReason names why one backend did not make a chain. Reasons go to
// slog and the admin-gated status surface ONLY — client errors stay generic
// (topology/presence disclosure, design 03 §2.4.3).
type ExclusionReason struct {
	Backend string `json:"backend"`
	Reason  string `json:"reason"`
}

// ErrNoEligibleBackend is returned when a chain comes up empty. Trust beats
// availability ALWAYS — there is no silent escalation across trust borders.
type ErrNoEligibleBackend struct {
	Role     string
	Required Sensitivity
	Excluded []ExclusionReason
}

func (e *ErrNoEligibleBackend) Error() string {
	return fmt.Sprintf("backends: no eligible backend for role %q (required sensitivity %q)", e.Role, e.Required)
}

type healthState struct {
	cooldownUntil    time.Time
	consecutiveFails int
	lastErrClass     ErrClass
	lastErrAt        time.Time
	lastOK           time.Time
}

type snapshot struct {
	backends []Backend
	version  int64
	loadedAt time.Time
}

// Pool is the declarative backend pool: an immutable snapshot of
// context_backends (atomic pointer swap, F1 store pattern) plus in-memory
// per-backend health that survives reloads and dies with the process.
type Pool struct {
	q       Querier
	secrets SecretResolver
	snap    atomic.Pointer[snapshot]
	version atomic.Int64

	healthM sync.Mutex
	health  map[string]*healthState // key = Backend.ID
}

// NewPool creates an empty pool bound to its row source and secret resolver.
// Call Reload to publish the first snapshot. secrets may be nil (keyless
// stub, P1–P4): api_key_refs then resolve to empty keys with a warning.
func NewPool(q Querier, secrets SecretResolver) *Pool {
	p := &Pool{q: q, secrets: secrets, health: make(map[string]*healthState)}
	p.snap.Store(&snapshot{})
	return p
}

const loadBackendsSQL = `
SELECT id, name, base_url, protocol, provider_class, api_key_ref, trust,
       locality, roles, model_map, timeouts, num_ctx, priority, enabled,
       extra_headers, extra_body, limits, metadata
  FROM context_backends
 ORDER BY name`

// Reload loads context_backends, resolves api_key_refs in-memory and swaps
// the snapshot atomically. Triggers: boot, NOTIFY (entity=context_backends),
// synchronous after every backend-* mutation, and ClassAuth self-heal (P2).
// A failed reload keeps the previous snapshot active (settings.Reload
// doctrine); a failed secret resolution keeps the BACKEND (keyless) so the
// 401→ClassAuth→Reload self-heal path stays reachable.
func (p *Pool) Reload(ctx context.Context) error {
	rows, err := p.q.Query(ctx, loadBackendsSQL)
	if err != nil {
		return fmt.Errorf("backends: load: %w", err)
	}
	defer rows.Close()

	var loaded []Backend
	for rows.Next() {
		b, err := scanBackend(rows)
		if err != nil {
			return fmt.Errorf("backends: scan: %w", err)
		}
		if b.APIKeyRef != "" {
			if p.secrets == nil {
				slog.Warn("backends: no secret resolver configured — backend stays keyless",
					"backend", b.Name, "api_key_ref", b.APIKeyRef)
			} else if key, err := p.secrets(ctx, b.APIKeyRef); err != nil {
				// Error is value-free by the resolver contract. Keep the
				// backend: a 401 at call time triggers the reload self-heal.
				slog.Error("backends: secret resolution failed — backend stays keyless",
					"backend", b.Name, "api_key_ref", b.APIKeyRef, "error", err)
			} else {
				b.APIKey = key
			}
		}
		loaded = append(loaded, b)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backends: load: %w", err)
	}

	v := p.version.Add(1)
	p.snap.Store(&snapshot{backends: loaded, version: v, loadedAt: time.Now()})
	slog.Info("backends: snapshot reloaded", "version", v, "backends", len(loaded))
	return nil
}

// scanBackend maps one context_backends row onto the Backend type,
// normalizing model_map short forms ("model-id" → ModelSpec{Model:…}).
func scanBackend(rows pgx.Rows) (Backend, error) {
	var (
		b                                   Backend
		apiKeyRef                           *string
		numCtx                              *int
		modelMap, timeouts                  []byte
		extraHeaders, extraBody             []byte
		limits, metadata                    []byte
		trust, locality                     string
		protocol                            string
	)
	if err := rows.Scan(&b.ID, &b.Name, &b.Host, &protocol, &b.ProviderClass,
		&apiKeyRef, &trust, &locality, &b.Roles, &modelMap, &timeouts, &numCtx,
		&b.Priority, &b.Enabled, &extraHeaders, &extraBody, &limits, &metadata); err != nil {
		return b, err
	}
	b.Protocol = Protocol(protocol)
	b.Trust = Trust(trust)
	b.Locality = locality
	if apiKeyRef != nil {
		b.APIKeyRef = *apiKeyRef
	}
	if numCtx != nil {
		b.NumCtx = *numCtx
	}
	var err error
	if b.ModelMap, err = ParseModelMap(modelMap); err != nil {
		return b, fmt.Errorf("backend %s: model_map: %w", b.Name, err)
	}
	if err = unmarshalInto(timeouts, &b.Timeouts); err != nil {
		return b, fmt.Errorf("backend %s: timeouts: %w", b.Name, err)
	}
	if err = unmarshalInto(extraHeaders, &b.ExtraHeaders); err != nil {
		return b, fmt.Errorf("backend %s: extra_headers: %w", b.Name, err)
	}
	if err = unmarshalInto(extraBody, &b.ExtraBody); err != nil {
		return b, fmt.Errorf("backend %s: extra_body: %w", b.Name, err)
	}
	if err = unmarshalInto(limits, &b.Limits); err != nil {
		return b, fmt.Errorf("backend %s: limits: %w", b.Name, err)
	}
	if err = unmarshalInto(metadata, &b.Metadata); err != nil {
		return b, fmt.Errorf("backend %s: metadata: %w", b.Name, err)
	}
	return b, nil
}

func unmarshalInto[T any](raw []byte, dst *T) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// ParseModelMap normalizes the model_map JSONB: values are either the string
// short form "model-id" or the ModelSpec object form. Exported because the
// API validation parses candidate payloads through the same code path.
func ParseModelMap(raw []byte) (map[string]ModelSpec, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rough map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rough); err != nil {
		return nil, err
	}
	out := make(map[string]ModelSpec, len(rough))
	for role, v := range rough {
		var short string
		if err := json.Unmarshal(v, &short); err == nil {
			out[role] = ModelSpec{Model: short}
			continue
		}
		var spec ModelSpec
		if err := json.Unmarshal(v, &spec); err != nil {
			return nil, fmt.Errorf("role %q: neither string nor {model,params} object", role)
		}
		out[role] = spec
	}
	return out, nil
}

// Snapshot returns the current immutable backend list. Readers dereference
// ONCE per operation — never mix backends from two generations.
func (p *Pool) Snapshot() []Backend {
	return p.snap.Load().backends
}

// SeedSnapshotForTest publishes a static snapshot without a database. Test
// seam only (handler confirm/validation paths) — production snapshots come
// exclusively from Reload.
func (p *Pool) SeedSnapshotForTest(bs []Backend) {
	p.snap.Store(&snapshot{backends: bs, version: -1, loadedAt: time.Now()})
}

// Chain returns the eligible backends for one operation, in attempt order:
// filter (enabled && role && trust.Allows(required) && !gaming-disabled),
// sort (inCooldown ASC, priority DESC, name ASC). Cooldown never REMOVES —
// it sorts to the end, so a single-backend role stays reachable through its
// cooldown (order hint, not circuit breaker). Empty chain returns
// *ErrNoEligibleBackend with per-backend reasons for slog/admin status; the
// client-facing error stays generic at the handler.
func (p *Pool) Chain(role string, required Sensitivity, gaming GamingState) ([]Backend, error) {
	snap := p.snap.Load()
	now := time.Now()

	var eligible []Backend
	var excluded []ExclusionReason
	for i := range snap.backends {
		b := &snap.backends[i]
		switch {
		case !b.HasRole(role):
			// Not part of this role's routing table — no reason entry, the
			// row was never a candidate. Checked FIRST so disabled backends
			// of unrelated roles don't spam the exclusion reasons.
		case !b.Enabled:
			excluded = append(excluded, ExclusionReason{b.Name, "disabled"})
		case !b.Trust.Allows(required):
			excluded = append(excluded, ExclusionReason{b.Name,
				fmt.Sprintf("trust %s < required %s", b.Trust, required)})
		case gaming.disables(b.Name):
			excluded = append(excluded, ExclusionReason{b.Name, "disabled by gaming"})
		default:
			eligible = append(eligible, *b)
		}
	}
	if len(eligible) == 0 {
		err := &ErrNoEligibleBackend{Role: role, Required: required, Excluded: excluded}
		slog.Warn("backends: empty chain", "role", role, "required", string(required),
			"excluded", fmt.Sprintf("%+v", excluded))
		return nil, err
	}

	cooled := p.cooldownSet(eligible, now)
	sort.SliceStable(eligible, func(i, j int) bool {
		ci, cj := cooled[eligible[i].ID], cooled[eligible[j].ID]
		if ci != cj {
			return !ci // not-cooled first
		}
		if eligible[i].Priority != eligible[j].Priority {
			return eligible[i].Priority > eligible[j].Priority
		}
		return eligible[i].Name < eligible[j].Name
	})
	return eligible, nil
}

// RoleConfigured reports whether ANY row carries the role, regardless of
// enabled/trust/cooldown. The rerank dispatch needs the distinction
// "role absent from the routing table" (⇒ LLM-judge substitute, a
// configuration alternative) vs "chain empty by trust/gaming/disabled"
// (⇒ fail-open to RRF order, design 03 §2.5).
func (p *Pool) RoleConfigured(role string) bool {
	snap := p.snap.Load()
	for i := range snap.backends {
		if snap.backends[i].HasRole(role) {
			return true
		}
	}
	return false
}

// PrimaryModel names the model of the highest-priority enabled backend for a
// role — "the model that WOULD answer". Used for the response model field
// when the LLM step is skipped (score filter); empty when none qualifies.
func (p *Pool) PrimaryModel(role string) string {
	snap := p.snap.Load()
	best := -1
	model := ""
	for i := range snap.backends {
		b := &snap.backends[i]
		if !b.Enabled || !b.HasRole(role) {
			continue
		}
		if b.Priority > best {
			if m := b.ModelFor(role).Model; m != "" {
				best = b.Priority
				model = m
			}
		}
	}
	return model
}

// cooldownSet snapshots which of the given backends are in cooldown at now.
func (p *Pool) cooldownSet(list []Backend, now time.Time) map[string]bool {
	out := make(map[string]bool, len(list))
	p.healthM.Lock()
	defer p.healthM.Unlock()
	for i := range list {
		if h, ok := p.health[list[i].ID]; ok && h.cooldownUntil.After(now) {
			out[list[i].ID] = true
		}
	}
	return out
}

// ReportFailure records one failed attempt: class cooldown (doubled after
// ≥3 consecutive fails, capped at 10 minutes — cheap backoff, no circuit
// breaker state machine) and the sanitized error class. Full error strings
// (URLs, provider bodies) exist in slog only — never here, never in status.
func (p *Pool) ReportFailure(backendID string, class ErrClass, retryAfter time.Duration) {
	p.healthM.Lock()
	defer p.healthM.Unlock()
	h, ok := p.health[backendID]
	if !ok {
		h = &healthState{}
		p.health[backendID] = h
	}
	h.consecutiveFails++
	h.lastErrClass = class
	h.lastErrAt = time.Now()
	cd := class.Cooldown(retryAfter)
	if h.consecutiveFails >= 3 && cd > 0 {
		cd *= 2
		if cd > 10*time.Minute {
			cd = 10 * time.Minute
		}
	}
	if cd > 0 {
		h.cooldownUntil = time.Now().Add(cd)
	}
}

// ReportSuccess clears the failure streak and stamps lastOK.
func (p *Pool) ReportSuccess(backendID string) {
	p.healthM.Lock()
	defer p.healthM.Unlock()
	h, ok := p.health[backendID]
	if !ok {
		h = &healthState{}
		p.health[backendID] = h
	}
	h.consecutiveFails = 0
	h.cooldownUntil = time.Time{}
	h.lastOK = time.Now()
}

// BackendStatus is the per-backend detail for the ADMIN-GATED backend-list
// and the anonymous /health aggregation (which only ever consumes the role
// reachability, never names). LastErrorClass is the sanitized ErrClass
// string — URLs and provider bodies never reach this struct.
type BackendStatus struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Trust             Trust    `json:"trust"`
	Locality          string   `json:"locality"`
	Roles             []string `json:"roles"`
	Priority          int      `json:"priority"`
	Enabled           bool     `json:"enabled"`
	EffectiveState    string   `json:"effective_state"` // active|disabled|cooldown
	CooldownRemaining int      `json:"cooldown_remaining_s"`
	ConsecutiveFails  int      `json:"consecutive_fails"`
	LastErrorClass    string   `json:"last_error_class,omitempty"`
	LastOK            string   `json:"last_ok,omitempty"`
}

// Status merges the current snapshot with live health for the admin surface.
func (p *Pool) Status() []BackendStatus {
	snap := p.snap.Load()
	now := time.Now()
	out := make([]BackendStatus, 0, len(snap.backends))
	p.healthM.Lock()
	defer p.healthM.Unlock()
	for i := range snap.backends {
		b := &snap.backends[i]
		s := BackendStatus{
			ID: b.ID, Name: b.Name, Trust: b.Trust, Locality: b.Locality,
			Roles: b.Roles, Priority: b.Priority, Enabled: b.Enabled,
			EffectiveState: "active",
		}
		if !b.Enabled {
			s.EffectiveState = "disabled"
		}
		if h, ok := p.health[b.ID]; ok {
			s.ConsecutiveFails = h.consecutiveFails
			if h.lastErrClass != ClassOK && !h.lastErrAt.IsZero() {
				s.LastErrorClass = h.lastErrClass.String()
			}
			if !h.lastOK.IsZero() {
				s.LastOK = h.lastOK.UTC().Format(time.RFC3339)
			}
			if b.Enabled && h.cooldownUntil.After(now) {
				s.EffectiveState = "cooldown"
				s.CooldownRemaining = int(time.Until(h.cooldownUntil).Seconds())
			}
		}
		out = append(out, s)
	}
	return out
}
