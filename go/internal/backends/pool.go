package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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

// Profile is one row of context_disable_profiles (092, Web-UX U01-W1): a named,
// scope-filtered set of backends that a disable-toggle takes out of every chain.
// W1 only LOADS profiles into the snapshot — the chain-time exclusion arm is
// W2. The slice in the snapshot is ORDER BY name (load-fixed, never Go-map
// order) so the status-payload diffKey stays byte-stable tick over tick.
type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Active      bool   `json:"active"`
	Reserved    bool   `json:"reserved"`
	Scope       string `json:"scope"`
}

type snapshot struct {
	backends []Backend
	// profiles is ORDER BY name (loadProfilesSQL) — deterministic, so any
	// consumer that marshals it (status frame) produces a stable diffKey.
	profiles []Profile
	// disabledBy maps backend_id → the comma-joined, SORTED names of the ACTIVE
	// profiles that contain it (precomputed at reload). W2's chain arm pays a
	// single map lookup; W1 only builds it. Empty string / absent = no active
	// profile disables the backend.
	disabledBy map[string]string
	// memberOf maps backend_id → the SORTED names of ALL profiles it is a member
	// of, REGARDLESS of active (092, U01-W4). This is the membership truth the
	// backend-config surface renders (disable_profiles field + the W6 checkbox
	// dialog needs inactive membership too); disabledBy only carries the ACTIVE
	// subset. Precomputed in the same atomic swap so the backend-list view is
	// consistent with the chain's snapshot without a second query. Absent = the
	// backend belongs to no profile.
	memberOf map[string][]string
	// memberCount maps profile ID → its total membership count (active or not),
	// precomputed per reload (092, U01-W7). Keyed by ID — NOT name — so two
	// same-named profiles in different scopes (possible under AM-5 tenant
	// scoping, UNIQUE(scope,name)) never cross-count each other's members. The
	// status frame's member_count reads this exact figure.
	memberCount map[string]int
	version     int64
	loadedAt    time.Time
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
       extra_headers, extra_body, limits, metadata, scope
  FROM context_backends
 ORDER BY scope, name`

// loadProfilesSQL loads the disable-profile registry (092). ORDER BY name is
// load-fixed on purpose (analog loadBackendsSQL ORDER BY scope,name): the
// profiles slice feeds the status payload, whose diffKey marshals the whole
// event every tick — an unstable order would re-broadcast without a state
// change (§4.1/§4.5.5 fan-out storm).
const loadProfilesSQL = `
SELECT id, name, label, description, active, reserved, scope
  FROM context_disable_profiles
 ORDER BY name`

// loadProfileMembershipsSQL loads the profile↔backend join (092). ORDER BY
// profile_id, backend_id is load-fixed; disabledBy sorts the joined names by
// name independently, so the map value is stable regardless of row order.
const loadProfileMembershipsSQL = `
SELECT profile_id, backend_id
  FROM context_disable_profile_backends
 ORDER BY profile_id, backend_id`

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

	// Disable profiles + memberships (092, U01-W1) go into the SAME atomic swap
	// as the backends — chain-time consumers (W2) always see backends and
	// profile state consistent to each other. A failed load keeps the previous
	// snapshot (settings.Reload doctrine), same as a failed backend load.
	profiles, memberships, err := p.loadProfiles(ctx)
	if err != nil {
		return err
	}

	v := p.version.Add(1)
	p.snap.Store(&snapshot{
		backends:    loaded,
		profiles:    profiles,
		disabledBy:  buildDisabledBy(profiles, memberships),
		memberOf:    buildMemberOf(profiles, memberships),
		memberCount: buildMemberCounts(memberships),
		version:     v,
		loadedAt:    time.Now(),
	})
	slog.Info("backends: snapshot reloaded", "version", v,
		"backends", len(loaded), "profiles", len(profiles))
	return nil
}

// profileMembership is one context_disable_profile_backends row (092).
type profileMembership struct {
	profileID string
	backendID string
}

// loadProfiles reads the disable-profile registry and its memberships (092).
// Both queries carry a load-fixed ORDER BY (see the SQL consts). Errors bubble
// up so Reload keeps the previous snapshot rather than publishing a partial one.
func (p *Pool) loadProfiles(ctx context.Context) ([]Profile, []profileMembership, error) {
	prows, err := p.q.Query(ctx, loadProfilesSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("backends: load profiles: %w", err)
	}
	defer prows.Close()
	var profiles []Profile
	for prows.Next() {
		var pr Profile
		if err := prows.Scan(&pr.ID, &pr.Name, &pr.Label, &pr.Description,
			&pr.Active, &pr.Reserved, &pr.Scope); err != nil {
			return nil, nil, fmt.Errorf("backends: scan profile: %w", err)
		}
		profiles = append(profiles, pr)
	}
	if err := prows.Err(); err != nil {
		return nil, nil, fmt.Errorf("backends: load profiles: %w", err)
	}

	mrows, err := p.q.Query(ctx, loadProfileMembershipsSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("backends: load memberships: %w", err)
	}
	defer mrows.Close()
	var memberships []profileMembership
	for mrows.Next() {
		var m profileMembership
		if err := mrows.Scan(&m.profileID, &m.backendID); err != nil {
			return nil, nil, fmt.Errorf("backends: scan membership: %w", err)
		}
		memberships = append(memberships, m)
	}
	if err := mrows.Err(); err != nil {
		return nil, nil, fmt.Errorf("backends: load memberships: %w", err)
	}
	return profiles, memberships, nil
}

// buildDisabledBy precomputes backend_id → comma-joined SORTED names of the
// ACTIVE profiles containing it. Names are sorted explicitly (not left to the
// membership row order) so the value is byte-stable — the constructive proof
// against a diffKey-instability fan-out storm (§4.1). Backends not disabled by
// any active profile are absent from the map (Go zero value "" on lookup).
func buildDisabledBy(profiles []Profile, memberships []profileMembership) map[string]string {
	activeName := make(map[string]string, len(profiles))
	for _, pr := range profiles {
		if pr.Active {
			activeName[pr.ID] = pr.Name
		}
	}
	names := make(map[string][]string)
	for _, m := range memberships {
		if n, ok := activeName[m.profileID]; ok {
			names[m.backendID] = append(names[m.backendID], n)
		}
	}
	out := make(map[string]string, len(names))
	for bid, ns := range names {
		sort.Strings(ns)
		out[bid] = strings.Join(ns, ",")
	}
	return out
}

// buildMemberOf precomputes backend_id → SORTED names of ALL profiles containing
// it, regardless of active (092, U01-W4). Unlike buildDisabledBy (active only,
// chain-facing) this is the full membership truth the backend-config surface
// renders. Names are sorted explicitly so the value is byte-stable (no Go-map
// iteration order leaks outward). A backend in no profile is absent from the map.
func buildMemberOf(profiles []Profile, memberships []profileMembership) map[string][]string {
	name := make(map[string]string, len(profiles))
	for _, pr := range profiles {
		name[pr.ID] = pr.Name
	}
	out := make(map[string][]string)
	for _, m := range memberships {
		if n, ok := name[m.profileID]; ok {
			out[m.backendID] = append(out[m.backendID], n)
		}
	}
	for bid := range out {
		sort.Strings(out[bid])
	}
	return out
}

// buildMemberCounts precomputes profile ID → total membership count, active or
// not (092, U01-W7). ID-keyed on purpose: name-keyed aggregation would let two
// same-named profiles in different scopes (legal under AM-5, UNIQUE(scope,name))
// cross-count each other's members and report a wrong figure in the status
// frame. A membership row whose profile vanished mid-load cannot exist (FK), so
// no existence filter is needed.
func buildMemberCounts(memberships []profileMembership) map[string]int {
	out := make(map[string]int)
	for _, m := range memberships {
		out[m.profileID]++
	}
	return out
}

// Profiles returns the current disable-profile registry snapshot (092), ORDER
// BY name. Readers dereference once per operation, like Snapshot.
func (p *Pool) Profiles() []Profile {
	return p.snap.Load().profiles
}

// MemberCounts returns the precomputed profile-ID → membership-count map from
// the current snapshot (092, U01-W7). The status frame's member_count reads it
// (buildStatusProfiles); ID-keyed, see buildMemberCounts. Snapshot-owned —
// read-only.
func (p *Pool) MemberCounts() map[string]int {
	return p.snap.Load().memberCount
}

// MemberOf returns the precomputed backend_id → ALL-profile-names map from the
// current snapshot (092, U01-W4). The backend-list/create/update view reads it
// to render disable_profiles (full membership, incl. profiles that are inactive
// right now). The returned map + its slices are snapshot-owned — read-only.
func (p *Pool) MemberOf() map[string][]string {
	return p.snap.Load().memberOf
}

// DisabledBy returns the precomputed backend_id → active-profile-names map from
// the current snapshot (092). W2's chain arm reads it; W1 exposes it for the
// snapshot probe. The returned map is snapshot-owned — treat it as read-only.
func (p *Pool) DisabledBy() map[string]string {
	return p.snap.Load().disabledBy
}

// scanBackend maps one context_backends row onto the Backend type,
// normalizing model_map short forms ("model-id" → ModelSpec{Model:…}).
func scanBackend(rows pgx.Rows) (Backend, error) {
	var (
		b                       Backend
		apiKeyRef               *string
		numCtx                  *int
		modelMap, timeouts      []byte
		extraHeaders, extraBody []byte
		limits, metadata        []byte
		trust, locality         string
		protocol                string
	)
	if err := rows.Scan(&b.ID, &b.Name, &b.Host, &protocol, &b.ProviderClass,
		&apiKeyRef, &trust, &locality, &b.Roles, &modelMap, &timeouts, &numCtx,
		&b.Priority, &b.Enabled, &extraHeaders, &extraBody, &limits, &metadata,
		&b.Scope); err != nil {
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


// VisibleTo reports whether a backend in scope bScope may be reached by caller
// tenant (04-W2/T34, egress isolation). Exported so the admin surface (T37,
// backend-list filter + backend-update/delete pre-check) rests on the exact
// same predicate as Chain's egress gate — one definition of "visible". A
// '_global' backend — or an unscoped
// row (Scope == "", pre-062 / test-seeded; the DB enforces NOT NULL DEFAULT
// '_global', so "" is test-only) — is shared and visible to everyone. A
// tenant-private backend is visible only to its own tenant. An empty or
// '_'-reserved caller tenant (the __UNAUTHORIZED__ sentinel, 003:48/052:83, or
// any reserved value) sees ONLY shared backends — never a same-named
// tenant-private one (fail-closed, design/04 §5.7). The '_'-prefix guard is
// defense-in-depth: handlers already reject !ar.IsValid before Chain
// (middleware.go:179), but a security axis names its sentinel explicitly rather
// than trust two non-local layers.
func VisibleTo(bScope, tenant string) bool {
	if bScope == "" || bScope == GlobalScope {
		return true
	}
	if tenant == "" || strings.HasPrefix(tenant, "_") {
		return false
	}
	return bScope == tenant
}

// Chain returns the eligible backends for one operation, in attempt order:
// filter (visibleTo(tenant) && enabled && role && !profile-disabled &&
// trust.Allows(required)), sort (inCooldown ASC, priority DESC, name ASC).
// Cooldown never REMOVES — it sorts to the end, so a single-backend role stays
// reachable through its cooldown (order hint, not circuit breaker). tenant is
// the caller's scope (ar.HomeScope / sess.Scope); a foreign tenant-private
// backend is not in the chain BY CONSTRUCTION (no ExclusionReason — no topology
// disclosure, §4.1). Empty chain returns *ErrNoEligibleBackend with per-backend
// reasons for slog/admin status; the client-facing error stays generic at the
// handler.
func (p *Pool) Chain(role string, required Sensitivity, tenant string) ([]Backend, error) {
	snap := p.snap.Load()
	now := time.Now()

	var eligible []Backend
	var excluded []ExclusionReason
	for i := range snap.backends {
		b := &snap.backends[i]
		switch {
		case !VisibleTo(b.Scope, tenant):
			// Not visible to this tenant — no reason entry: a foreign
			// tenant-private backend is non-existent to this caller (egress
			// isolation, no topology disclosure §5.3). Checked FIRST so the
			// tenant boundary is the outermost gate.
		case !b.HasRole(role):
			// Not part of this role's routing table — no reason entry, the
			// row was never a candidate. So disabled backends of unrelated
			// roles don't spam the exclusion reasons.
		case !b.Enabled:
			excluded = append(excluded, ExclusionReason{b.Name, "disabled"})
		case snap.disabledBy[b.ID] != "":
			// An ACTIVE disable-profile (092, U01-W2) contains this backend. The
			// reason names the profile(s) (comma-joined, sorted at reload). Placed
			// AFTER !Enabled (that reason stays the more specific one) and BEFORE
			// trust. Since U01-W5 this is the ONLY exclusion mechanism — the legacy
			// gaming arm (parallel double-write) is gone; the eject profile is the
			// single source of truth.
			excluded = append(excluded, ExclusionReason{b.Name,
				"disabled by profile " + snap.disabledBy[b.ID]})
		case !b.Trust.Allows(required):
			excluded = append(excluded, ExclusionReason{b.Name,
				fmt.Sprintf("trust %s < required %s", b.Trust, required)})
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
// configuration alternative) vs "chain empty by trust/profile/disabled"
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
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Trust          Trust    `json:"trust"`
	Locality       string   `json:"locality"`
	Roles          []string `json:"roles"`
	Priority       int      `json:"priority"`
	Enabled        bool     `json:"enabled"`
	EffectiveState string   `json:"effective_state"` // active|disabled|profile-disabled|cooldown
	// DisabledByProfiles names the ACTIVE disable-profiles that contain this
	// backend (092, U01-W2), sorted; empty/omitted when none. Independent of
	// EffectiveState — it is populated even when `disabled` (enabled=false) wins
	// the precedence, so the admin surface still sees the profile membership.
	DisabledByProfiles []string `json:"disabled_by_profiles,omitempty"`
	CooldownRemaining  int      `json:"cooldown_remaining_s"`
	ConsecutiveFails   int      `json:"consecutive_fails"`
	LastErrorClass     string   `json:"last_error_class,omitempty"`
	LastOK             string   `json:"last_ok,omitempty"`
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
		// Precedence: disabled > profile-disabled > cooldown > active (092,
		// U01-W2 §4.2). profile-disabled is set first, then `disabled`
		// (enabled=false) overrides it; cooldown below only applies while the
		// state is still "active", so both stronger states suppress it.
		if names := snap.disabledBy[b.ID]; names != "" {
			s.DisabledByProfiles = strings.Split(names, ",")
			s.EffectiveState = "profile-disabled"
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
			if b.Enabled && s.EffectiveState == "active" && h.cooldownUntil.After(now) {
				s.EffectiveState = "cooldown"
				s.CooldownRemaining = int(time.Until(h.cooldownUntil).Seconds())
			}
		}
		out = append(out, s)
	}
	return out
}
