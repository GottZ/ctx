// Package modelalias is the public model namespace of the OpenAI-compatible
// API (design/02 §3.1, §4.4; migration 162): an alias — the "model" id a
// consumer names — maps to a pool role, a sensitivity floor and a policy of
// request caps. The Registry keeps all rows of context_model_aliases as one
// atomic snapshot, reloaded at boot and on every ctx_settings_write NOTIFY of
// the table; Offers and Lookup are the one rule for what a consumer may see
// (V-0102-h): the consumer-model validation, the /v1 resolution and the
// web-UI picker all call them with the consumer's tenant scope.
//
// Leaf-like by contract (design/02 §4.1, pinned by TestPackageStaysLeaf): it
// imports backends (the sensitivity type and the visibility predicate) and
// pgxdb, never config, handler, store or events — those read the registry.
// Grants, the ctx-peer rule (V-02-c) and the kind check against the endpoint
// are the resolving endpoint's job, on top of Offers.
package modelalias

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// Kind is the endpoint family of an alias.
type Kind string

const (
	KindChat      Kind = "chat"
	KindEmbedding Kind = "embedding"
	KindRerank    Kind = "rerank"
)

// Alias is one row of context_model_aliases as loaded. Invalid is non-nil
// when the row failed validation at load (its policy above all, see
// ParsePolicy): such an alias is never offered — but it still shadows a
// _global alias of the same name for its tenant, so a broken tenant row
// exposes nothing instead of handing its name back to the operator's alias.
type Alias struct {
	ID          string
	Scope       string // backends.GlobalScope or a tenant scope
	Name        string
	Kind        Kind
	Role        string
	Sensitivity backends.Sensitivity
	Policy      Policy
	Description string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Invalid     error
}

func (a *Alias) offerable() bool { return a.Enabled && a.Invalid == nil }

// validScope: _global or a tenant scope — never "" (which the pool reads as
// _global) nor another reserved scope. Migration 162's CHECK holds the same.
func validScope(scope string) bool {
	return scope == backends.GlobalScope || (scope != "" && !strings.HasPrefix(scope, "_"))
}

// validateRow checks what the load flags beyond the policy; the table's
// CHECKs make each case unreachable, the check keeps the Go side fail-closed
// on its own.
func validateRow(scope string, kind Kind, sens backends.Sensitivity) error {
	switch {
	case !validScope(scope):
		return fmt.Errorf("scope %q is neither _global nor a tenant scope", scope)
	case !forAll.has(kind):
		return fmt.Errorf("unknown kind %q", kind)
	}
	switch sens {
	case backends.SensPublic, backends.SensInternal, backends.SensPersonal, backends.SensCredentials:
		return nil
	}
	return fmt.Errorf("unknown sensitivity %q", sens)
}

// Snapshot is one immutable generation of the registry.
type Snapshot struct {
	gen     uint64
	aliases []Alias
	global  map[string]int            // name → index of the _global alias
	tenant  map[string]map[string]int // tenant scope → name → index
}

// newSnapshot indexes aliases (rows with a malformed scope stay in the list,
// outside the index: they take no part in visibility or shadowing).
func newSnapshot(gen uint64, aliases []Alias) *Snapshot {
	s := &Snapshot{gen: gen, aliases: aliases, global: map[string]int{}, tenant: map[string]map[string]int{}}
	for i := range aliases {
		a := &aliases[i]
		switch {
		case a.Scope == backends.GlobalScope:
			s.global[a.Name] = i
		case validScope(a.Scope):
			if s.tenant[a.Scope] == nil {
				s.tenant[a.Scope] = map[string]int{}
			}
			s.tenant[a.Scope][a.Name] = i
		}
	}
	return s
}

// Generation is the reload count that produced the snapshot (0 = nothing
// loaded yet).
func (s *Snapshot) Generation() uint64 { return s.gen }

// Aliases returns a copy of every loaded row, invalid and disabled ones
// included, ordered by scope and name.
func (s *Snapshot) Aliases() []Alias { return append([]Alias(nil), s.aliases...) }

// winner is the alias that decides name for tenantScope: a visible tenant
// alias when one exists — enabled or not, valid or not (shadowing is decided
// by presence) — else the _global one.
func (s *Snapshot) winner(name, tenantScope string) (*Alias, bool) {
	if i, ok := s.tenant[tenantScope][name]; ok && backends.VisibleTo(s.aliases[i].Scope, tenantScope) {
		return &s.aliases[i], true
	}
	if i, ok := s.global[name]; ok {
		return &s.aliases[i], true
	}
	return nil, false
}

// Offers returns the aliases a consumer whose pool visibility is tenantScope
// may be offered, sorted by name: the aliases visible to the scope
// (backends.VisibleTo — an empty or reserved scope sees _global only), a
// tenant alias in place of the _global alias of the same name, and of those
// only the enabled ones whose row is valid. nil snapshot ⇒ nil.
func Offers(s *Snapshot, tenantScope string) []Alias {
	if s == nil {
		return nil
	}
	own := s.tenant[tenantScope]
	out := make([]Alias, 0, len(s.global)+len(own))
	add := func(name string) {
		if a, ok := s.winner(name, tenantScope); ok && a.offerable() {
			out = append(out, *a)
		}
	}
	for name := range own {
		add(name)
	}
	for name := range s.global {
		if _, shadowed := own[name]; !shadowed {
			add(name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns the alias name resolves to for tenantScope — exactly the
// entry Offers lists under that name, or false.
func Lookup(s *Snapshot, name, tenantScope string) (Alias, bool) {
	if s == nil {
		return Alias{}, false
	}
	a, ok := s.winner(name, tenantScope)
	if !ok || !a.offerable() {
		return Alias{}, false
	}
	return *a, true
}

// Registry holds the current snapshot. The zero generation is empty: until
// the first successful Reload nothing is offered.
type Registry struct {
	q    pgxdb.Querier
	mu   sync.Mutex // serializes reloads: generation order = publication order
	snap atomic.Pointer[Snapshot]
	log  *slog.Logger // nil = slog.Default(); tests capture it
	// ceilings is the setting-bound cap source (SetCeilings): every Reload
	// reads it once and clamps tenant aliases to its Tenant part. nil = no
	// clamp (tests that do not exercise it); cmd/ctxd always sets it.
	ceilings func() Ceilings

	// Trigger's single flight: running while a drain goroutine reloads,
	// pending when a Trigger arrived during that reload.
	flightMu sync.Mutex
	running  bool
	pending  bool
}

// NewRegistry returns an empty registry that loads through q.
func NewRegistry(q pgxdb.Querier) *Registry {
	r := &Registry{q: q}
	r.snap.Store(newSnapshot(0, nil))
	return r
}

// SetCeilings installs the getter of the setting-bound ceilings the reload
// clamps tenant aliases to (design/02 §3.3: a tenant alias above
// gateway.tenant_alias_ceiling — a psql edit, or a ceiling lowered after the
// write — is clamped, with a WARN, instead of served above it). Boot calls it
// before the first Reload; it takes effect from the next reload on.
func (r *Registry) SetCeilings(fn func() Ceilings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ceilings = fn
}

// Snapshot returns the current generation; never nil.
func (r *Registry) Snapshot() *Snapshot { return r.snap.Load() }

// Generation returns the generation of the current snapshot.
func (r *Registry) Generation() uint64 { return r.Snapshot().Generation() }

func (r *Registry) logger() *slog.Logger {
	if r.log != nil {
		return r.log
	}
	return slog.Default()
}

// Trigger asks for a reload and returns at once — the hot-reload entry of
// the NOTIFY listener, whose single loop serves every channel and must never
// wait for a table read. Single flight with a pending bit: if no reload runs,
// one starts in its own goroutine; if one runs, the Trigger only marks a
// rerun, and the running reload reads the table exactly once more after it
// finishes. A burst of N writes therefore costs at most the running reload
// plus one, never N; no write is lost, because every reload reads the whole
// table and a write committed during a reload brings its own NOTIFY, which
// marks the rerun. A failed reload keeps the previous snapshot (Reload) and
// is logged here; nothing retries it until the next Trigger (a NOTIFY, a
// listener backlog) — the Pool.Reload doctrine. ctx bounds the goroutine: a
// cancelled ctx ends the drain after the reload it is in.
func (r *Registry) Trigger(ctx context.Context) {
	r.flightMu.Lock()
	defer r.flightMu.Unlock()
	if r.running {
		r.pending = true
		return
	}
	r.running = true
	go r.drain(ctx)
}

// drain reloads until no Trigger arrived during the last reload.
func (r *Registry) drain(ctx context.Context) {
	for {
		if err := r.Reload(ctx); err != nil {
			r.logger().Warn("modelalias: registry reload failed — previous snapshot stays active", "error", err)
		}
		r.flightMu.Lock()
		if !r.pending || ctx.Err() != nil {
			r.running, r.pending = false, false
			r.flightMu.Unlock()
			return
		}
		r.pending = false
		r.flightMu.Unlock()
	}
}

// loadSQL reads every alias; Reload scans the columns in this order.
const loadSQL = `
SELECT id::text, scope, name, kind, role, sensitivity, policy, description, enabled, created_at, updated_at
  FROM context_model_aliases
 ORDER BY scope COLLATE "C", name COLLATE "C"`

// Reload reads context_model_aliases and publishes it as the next
// generation, synchronously (boot, tests; the listener uses Trigger). A failed read keeps the previous snapshot and its
// generation (the Pool.Reload doctrine) and returns the error. A row that
// fails validation is loaded with Invalid set and named by one WARN per
// reload; it is never offered. A valid tenant alias above the tenant ceiling
// (SetCeilings) is loaded clamped to it, with one WARN per reload.
func (r *Registry) Reload(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	loaded, err := r.load(ctx)
	if err != nil {
		return err
	}
	invalid, clamped := 0, 0
	var tenantCeiling *TenantCeiling
	if r.ceilings != nil {
		t := r.ceilings().Tenant
		tenantCeiling = &t
	}
	for i := range loaded {
		a := &loaded[i]
		if a.Invalid != nil {
			invalid++
			r.logger().Warn("modelalias: alias is not offered — its row failed validation",
				"alias_id", a.ID, "scope", a.Scope, "name", a.Name, "error", a.Invalid)
			continue
		}
		if tenantCeiling == nil || a.Scope == backends.GlobalScope {
			continue
		}
		var notes []string
		if a.Policy, notes = ClampTenant(a.Policy, *tenantCeiling); len(notes) > 0 {
			clamped++
			r.logger().Warn("modelalias: tenant alias clamped to gateway.tenant_alias_ceiling",
				"alias_id", a.ID, "scope", a.Scope, "name", a.Name, "clamped", strings.Join(notes, ", "))
		}
	}
	gen := r.Snapshot().Generation() + 1
	r.snap.Store(newSnapshot(gen, loaded))
	r.logger().Info("modelalias: snapshot reloaded", "generation", gen, "aliases", len(loaded), "invalid", invalid, "clamped", clamped)
	return nil
}

func (r *Registry) load(ctx context.Context) ([]Alias, error) {
	rows, err := r.q.Query(ctx, loadSQL)
	if err != nil {
		return nil, fmt.Errorf("modelalias: load: %w", err)
	}
	defer rows.Close()
	var loaded []Alias
	for rows.Next() {
		var (
			a          Alias
			kind, sens string
			policy     []byte
		)
		if err := rows.Scan(&a.ID, &a.Scope, &a.Name, &kind, &a.Role, &sens, &policy,
			&a.Description, &a.Enabled, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("modelalias: scan: %w", err)
		}
		a.Kind, a.Sensitivity = Kind(kind), backends.Sensitivity(sens)
		if err := validateRow(a.Scope, a.Kind, a.Sensitivity); err != nil {
			a.Invalid = err
		} else if a.Policy, err = ParsePolicy(a.Kind, policy); err != nil {
			a.Invalid = err
		}
		loaded = append(loaded, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("modelalias: load: %w", err)
	}
	return loaded, nil
}
