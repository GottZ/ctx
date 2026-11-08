package backends

// Capability probe (FOLGE-Z Z1): what each pool row's engine says it can do,
// learned from the engine itself instead of a policy key an operator has to
// know about. Today one capability: vision (image input). The result lives
// in memory beside the snapshot (like health: it survives reloads of an
// unchanged row and dies with the process) — no SQL, no migration, the row's
// metadata stays untouched.
//
// Fail-closed by construction: a probe that times out, answers non-200, an
// unparsable body, a missing field or a protocol without a probe source all
// leave vision unknown (nil) — and unknown never counts as image-capable
// (Pool.ImageCapable). Nothing is ever guessed true.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
)

// Capability sources: the wire value of Capabilities.Source names where the
// answer came from (endpoint and field), CapSourceUnknown when nothing
// answered.
const (
	CapSourceSGLang     = "sglang:/get_model_info has_image_understanding"
	CapSourceLlamaCpp   = "llamacpp:/props modalities.vision"
	CapSourceOpenRouter = "openrouter:/api/v1/models architecture.input_modalities"
	CapSourceUnknown    = "unknown"
)

// CapabilityProbeTimeout bounds one row's probe (every GET of it together).
// A fixed, named bound (FOLGE-Z lead decision): the probe never runs in a
// request path, rows are probed in parallel, so a hung engine costs one
// goroutine for at most this long and reads unknown.
const CapabilityProbeTimeout = 3 * time.Second

// Body caps of the probe reads — memory guards, not policy (Fremd-Review 1
// R1-2: a 16 MiB cap read in parallel over every row let 20 rows on a
// tenant's host reach a 912 MiB heap peak). An engine answer
// (/get_model_info ~2 KB on SGLang, /props with its chat template tens of KB
// on llama.cpp) stays far below 1 MiB; the OpenRouter model list was 829 KB
// for 461 models on 2026-11-08 — 4 MiB is ~5× headroom. Past a cap the probe
// reads unknown (body_too_large), never a truncated parse.
const (
	capEngineBodyCap    = 1 << 20
	capModelListBodyCap = 4 << 20
)

// capProbeParallel bounds the rows probed at once: with the caps above a
// round holds at most 8 × 1 MiB of engine answers (plus one model list per
// URL) whatever the pool's size; 50 hung rows then cost ⌈50/8⌉ timeouts.
const capProbeParallel = 8

// Capabilities is what the probe learned about one row.
type Capabilities struct {
	// Vision: true/false as the engine reported it; nil = unknown.
	Vision *bool
	// ProbedAt is the time of the last finished probe of the row; zero =
	// never probed.
	ProbedAt time.Time
	// Source is one of the CapSource* constants.
	Source string
	// Reason is the value-free class of an unknown answer (timeout,
	// unreachable, http_status_<n>, no_field, bad_body, not_listed, no_model,
	// not_probed, api_key_ref_unresolved); empty when Vision is known. Never
	// a URL, header or body excerpt.
	Reason string
}

// capEntry is one row's stored result plus the fingerprint of the row it
// was probed for (a reload that changes the fingerprint drops the entry).
type capEntry struct {
	fp   string
	caps Capabilities
}

// capabilitiesWire is the JSON form (FOLGE-Z §3): vision true|false|null,
// probed_at RFC3339|null, source, reason (omitted when empty).
type capabilitiesWire struct {
	Vision   *bool   `json:"vision"`
	ProbedAt *string `json:"probed_at"`
	Source   string  `json:"source"`
	Reason   string  `json:"reason,omitempty"`
}

// MarshalJSON renders the wire form.
func (c Capabilities) MarshalJSON() ([]byte, error) {
	w := capabilitiesWire{Vision: c.Vision, Source: c.Source}
	if w.Source == "" {
		w.Source = CapSourceUnknown
	}
	if !c.ProbedAt.IsZero() {
		s := c.ProbedAt.UTC().Format(time.RFC3339)
		w.ProbedAt = &s
	}
	w.Reason = c.Reason
	return json.Marshal(w)
}

// capClients is the probe's client pair from the wire clients' constructor
// (the llm/embed/rerank posture): the same transport and dial path the row's
// inference calls take, keyed on the same EgressGuarded predicate — a
// tenant row's probe dials through the SSRF guard exactly like its
// inference. No client timeout: every probe carries its own deadline.
var capClients = httpx.NewClients()

// Capabilities returns the probe result of the row with id; an unprobed or
// unknown row reads Vision nil, Source CapSourceUnknown.
func (p *Pool) Capabilities(id string) Capabilities {
	snap := p.snap.Load()
	fp, known := "", false
	for i := range snap.backends {
		if snap.backends[i].ID == id {
			fp, known = capFingerprint(&snap.backends[i]), true
			break
		}
	}
	p.capM.Lock()
	e, ok := p.caps[id]
	p.capM.Unlock()
	// An answer for another fingerprint (the row moved while a round was in
	// flight) is not this row's answer (R1-7).
	if !ok || !known || e.fp != fp {
		return Capabilities{Source: CapSourceUnknown, Reason: "not_probed"}
	}
	return e.caps
}

// RowImageCapable reports whether the probe answered vision=true for row b
// as it stands (fingerprint-checked) — the per-row test of the /v1 door's
// image routing (oai: an image request on an alias whose vision comes from
// the backends walks only such rows).
func (p *Pool) RowImageCapable(b *Backend) bool {
	p.capM.Lock()
	e, ok := p.caps[b.ID]
	p.capM.Unlock()
	return ok && e.fp == capFingerprint(b) && e.caps.Vision != nil && *e.caps.Vision
}

// RowAnswered reports whether row b holds an answer for its current
// fingerprint (any answer, unknown included).
func (p *Pool) RowAnswered(b *Backend) bool {
	p.capM.Lock()
	e, ok := p.caps[b.ID]
	p.capM.Unlock()
	return ok && e.fp == capFingerprint(b)
}

// ChainVisionView is the pool as the alias registry's vision view: Pool's
// chain filter plus the export rule of the /v1 chain build (oai.BuildChain
// step 4) — a tenant alias whose role is not in ExportableRoles
// (gateway.exportable_roles, read per call) reaches no operator row, so an
// operator row's capability does not count for it (Fremd-Review 1 R1-1).
type ChainVisionView struct {
	Pool            *Pool
	ExportableRoles func() []string
}

// ImageCapable is Pool.ImageCapable under the export rule.
func (v ChainVisionView) ImageCapable(role string, sens Sensitivity, scope string) bool {
	operatorRows := scope == "" || scope == GlobalScope
	if !operatorRows && v.ExportableRoles != nil {
		for _, r := range v.ExportableRoles() {
			if r == role {
				operatorRows = true
				break
			}
		}
	}
	return v.Pool.imageCapable(role, sens, scope, operatorRows)
}

// ImageCapable reports whether the chain of role for an alias of scope at
// sensitivity floor sens holds at least one row whose probe reported
// vision=true: enabled, not disabled by an active profile, visible to scope
// (VisibleTo — a _global alias sees shared rows only), carrying role and
// allowed by trust for sens — the Chain filter without cooldown (cooldown
// reorders, it never removes). Unknown counts as not capable (fail-closed).
func (p *Pool) ImageCapable(role string, sens Sensitivity, scope string) bool {
	return p.imageCapable(role, sens, scope, true)
}

// imageCapable is ImageCapable; operatorRows false leaves _global and
// unscoped rows out (the export rule, ChainVisionView).
func (p *Pool) imageCapable(role string, sens Sensitivity, scope string, operatorRows bool) bool {
	snap := p.snap.Load()
	p.capM.Lock()
	defer p.capM.Unlock()
	for i := range snap.backends {
		b := &snap.backends[i]
		if !VisibleTo(b.Scope, scope) || !b.HasRole(role) || !b.Enabled ||
			snap.disabledBy[b.ID] != "" || !b.Trust.Allows(sens) {
			continue
		}
		if !operatorRows && (b.Scope == "" || b.Scope == GlobalScope) {
			continue
		}
		if e, ok := p.caps[b.ID]; ok && e.fp == capFingerprint(b) && e.caps.Vision != nil && *e.caps.Vision {
			return true
		}
	}
	return false
}

// OnChange registers fn to run after every successful Reload and after every
// probe round that changed a row's capabilities. fn must not block (the
// model-alias registry's Trigger is the intended hook).
func (p *Pool) OnChange(fn func()) {
	p.capM.Lock()
	defer p.capM.Unlock()
	p.capHooks = append(p.capHooks, fn)
}

// runHooks calls the change hooks outside capM.
func (p *Pool) runHooks() {
	p.capM.Lock()
	hooks := append([]func(){}, p.capHooks...)
	p.capM.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// afterReload is Reload's capability step on the freshly published snapshot:
// drop the results of vanished rows and of rows whose fingerprint moved
// (they read unknown until the next round), kick a probe round
// (RunCapabilityProbe; a pending kick absorbs this one) and run the hooks
// (enabled, roles, profiles or scope may have changed the effective vision
// of an alias even when no capability did).
func (p *Pool) afterReload(rows []Backend) {
	fps := make(map[string]string, len(rows))
	for i := range rows {
		fps[rows[i].ID] = capFingerprint(&rows[i])
	}
	p.capM.Lock()
	for id, e := range p.caps {
		if fp, ok := fps[id]; !ok || fp != e.fp {
			delete(p.caps, id)
		}
	}
	p.capM.Unlock()
	select {
	case p.capKick <- struct{}{}:
	default:
	}
	p.runHooks()
}

// capFingerprint is what a probe result was learned for: a reload that moves
// any of these drops the stored result (unknown until the next round).
func capFingerprint(b *Backend) string {
	parts := []string{b.Host, string(b.Protocol), b.ProviderClass, b.ModelFor("default").Model}
	return strings.Join(append(parts, capModels(b)...), "\x00")
}

// capModels are the models the chain sends for the row's roles
// (ModelFor(role), in role order; R1-4) — the models an OpenRouter row's
// answer is learned for.
func capModels(b *Backend) []string {
	out := make([]string, 0, len(b.Roles))
	for _, r := range b.Roles {
		out = append(out, b.ModelFor(r).Model)
	}
	return out
}

// capProbeKind is the probe source of a row: "engine" (SGLang
// /get_model_info, then llama.cpp /props), "openrouter" (the model list), or
// "" (no probe source: ollama, anthropic, rerank, a ctx peer).
func capProbeKind(b *Backend) string {
	if b.Protocol != ProtocolOpenAI {
		return ""
	}
	switch b.ProviderClass {
	case ProviderOpenRouter:
		return "openrouter"
	case ProviderGeneric, ProviderLlamaCpp, "":
		return "engine"
	}
	return ""
}

// ProbeCapabilities runs one probe round over the current snapshot: every
// enabled, not profile-disabled row, in parallel, each under
// CapabilityProbeTimeout. It returns the number of rows whose capabilities
// changed (each change is logged at INFO); OnChange hooks run when it is > 0.
func (p *Pool) ProbeCapabilities(ctx context.Context) int {
	return p.probeRound(ctx, false)
}

// probeRound is one round; onlyMissing (the round a reload kicks, R1-6)
// asks only rows without a valid answer — new rows and rows whose
// fingerprint moved — instead of every row on every secret or settings
// write. The cadence round asks all.
func (p *Pool) probeRound(ctx context.Context, onlyMissing bool) int {
	snap := p.snap.Load()
	type result struct {
		id, name, fp string
		caps         Capabilities
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []result
		lists   = &modelLists{m: map[string]*modelList{}}
	)
	sem := make(chan struct{}, capProbeParallel)
	for i := range snap.backends {
		b := snap.backends[i]
		if !b.Enabled || snap.disabledBy[b.ID] != "" {
			continue
		}
		if onlyMissing && p.RowAnswered(&b) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c := p.probeRow(ctx, &b, lists)
			mu.Lock()
			results = append(results, result{id: b.ID, name: b.Name, fp: capFingerprint(&b), caps: c})
			mu.Unlock()
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		// A round cut short by shutdown records nothing.
		return 0
	}
	changed := 0
	p.capM.Lock()
	for _, r := range results {
		old, had := p.caps[r.id]
		p.caps[r.id] = capEntry{fp: r.fp, caps: r.caps}
		if had && old.fp == r.fp && sameCapabilities(old.caps, r.caps) {
			continue
		}
		if !had && r.caps.Vision == nil {
			// unprobed → unknown is no change of what anyone reads.
			continue
		}
		changed++
		slog.Info("backends: capability changed", "backend", r.name,
			"vision", visionString(r.caps.Vision), "source", r.caps.Source, "reason", r.caps.Reason)
	}
	p.capM.Unlock()
	if changed > 0 {
		p.runHooks()
	}
	return changed
}

func sameCapabilities(a, b Capabilities) bool {
	return visionString(a.Vision) == visionString(b.Vision) && a.Source == b.Source && a.Reason == b.Reason
}

func visionString(v *bool) string {
	switch {
	case v == nil:
		return "unknown"
	case *v:
		return "true"
	}
	return "false"
}

// RunCapabilityProbe is the probe's own cadence: one full round at start
// (boot, after the first Reload), one full round every interval()
// (pool.capability_probe_interval, read per wait — hot) measured from the
// last full round, and in between one round per Reload kick over the rows
// without an answer. A kick never moves the full round's deadline (PR #178
// review): with reloads more frequent than the interval the full round
// still runs, so an answer that changed behind an unchanged fingerprint (a
// model swapped behind the same base_url) is seen within one interval.
// Blocks until ctx ends; the round in flight is cancelled with it.
func (p *Pool) RunCapabilityProbe(ctx context.Context, interval func() time.Duration) {
	slog.Info("backends: capability probe started")
	full := true
	var lastFull time.Time
	for {
		if full {
			lastFull = time.Now()
		}
		p.probeRound(ctx, !full)
		iv := interval()
		if iv <= 0 {
			iv = 600 * time.Second // V59 refuses it; a hand-built snapshot must not spin
		}
		t := time.NewTimer(max(time.Until(lastFull.Add(iv)), 0))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-p.capKick:
			t.Stop()
			full = !time.Now().Before(lastFull.Add(iv))
		case <-t.C:
			full = true
		}
	}
}

// modelLists shares one OpenRouter model-list read per (URL, guarded) and
// round between every row that needs it.
type modelLists struct {
	mu sync.Mutex
	m  map[string]*modelList
}

type modelList struct {
	once   sync.Once
	models map[string][]string // model id → input_modalities
	reason string              // non-empty: the list could not be read
}

func (l *modelLists) get(key string) *modelList {
	l.mu.Lock()
	defer l.mu.Unlock()
	ml := l.m[key]
	if ml == nil {
		ml = &modelList{}
		l.m[key] = ml
	}
	return ml
}

// probeRow is one row's probe under the per-row bound.
func (p *Pool) probeRow(ctx context.Context, b *Backend, lists *modelLists) Capabilities {
	unknown := func(reason string) Capabilities {
		return Capabilities{Source: CapSourceUnknown, Reason: reason, ProbedAt: time.Now()}
	}
	kind := capProbeKind(b)
	if kind == "" {
		return unknown("not_probed")
	}
	if b.APIKeyRef != "" && b.APIKey == "" {
		return unknown("api_key_ref_unresolved")
	}
	ctx, cancel := context.WithTimeout(ctx, p.capTimeout)
	defer cancel()
	base := strings.TrimRight(b.Host, "/")
	if kind == "openrouter" {
		// Every model the chain sends for the row's roles must take images
		// (R1-4): one text model ⇒ false, one unlisted ⇒ unknown.
		models := capModels(b)
		if len(models) == 0 {
			models = []string{b.ModelFor("default").Model}
		}
		url := base + "/v1/models"
		ml := lists.get(url + "|" + strconv.FormatBool(b.EgressGuarded()))
		ml.once.Do(func() { ml.models, ml.reason = fetchModelList(ctx, b, url) })
		if ml.reason != "" {
			return unknown(ml.reason)
		}
		all := true
		for _, model := range models {
			if model == "" {
				return unknown("no_model")
			}
			mods, ok := ml.models[model]
			if !ok {
				return unknown("not_listed")
			}
			if !slices.Contains(mods, "image") {
				all = false
			}
		}
		return Capabilities{Vision: &all, Source: CapSourceOpenRouter, ProbedAt: time.Now()}
	}
	// An engine row: SGLang answers /get_model_info, llama.cpp /props. The
	// first endpoint that carries its field decides.
	var info struct {
		HasImage *bool `json:"has_image_understanding"`
	}
	reason := ""
	switch r := capGET(ctx, b, base+"/get_model_info", capEngineBodyCap, &info); {
	case r == "" && info.HasImage != nil:
		return Capabilities{Vision: info.HasImage, Source: CapSourceSGLang, ProbedAt: time.Now()}
	case r == "":
		reason = "no_field"
	default:
		reason = r
	}
	if reason == "timeout" {
		return unknown(reason)
	}
	var props struct {
		Modalities *struct {
			Vision *bool `json:"vision"`
		} `json:"modalities"`
	}
	r := capGET(ctx, b, base+"/props", capEngineBodyCap, &props)
	switch {
	case r == "" && props.Modalities != nil && props.Modalities.Vision != nil:
		return Capabilities{Vision: props.Modalities.Vision, Source: CapSourceLlamaCpp, ProbedAt: time.Now()}
	case r == "":
		return unknown("no_field")
	case r == "http_status_404" && reason == "http_status_404":
		return unknown("no_endpoint")
	case r == "http_status_404":
		// Not a llama.cpp engine: the first endpoint's answer says more.
		return unknown(reason)
	}
	return unknown(r)
}

// fetchModelList reads an OpenRouter-shaped model list: data[].id with
// architecture.input_modalities.
func fetchModelList(ctx context.Context, b *Backend, url string) (map[string][]string, string) {
	var list struct {
		Data []struct {
			ID           string `json:"id"`
			Architecture struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if r := capGET(ctx, b, url, capModelListBodyCap, &list); r != "" {
		return nil, r
	}
	out := make(map[string][]string, len(list.Data))
	for _, m := range list.Data {
		out[m.ID] = m.Architecture.InputModalities
	}
	return out, ""
}

// capGET is one probe GET decoded into dst. The answer is "" on success or a
// value-free reason: timeout, unreachable, http_status_<n>, body_too_large,
// bad_body. The row's key goes as Bearer and its extra_headers ride along —
// the request an inference call of the row would carry, to the row's own
// origin only.
func capGET(ctx context.Context, b *Backend, url string, bodyCap int64, dst any) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "bad_url"
	}
	for k, v := range b.ExtraHeaders {
		req.Header.Set(k, v)
	}
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := capClients.For(b.EgressGuarded()).Do(req)
	if err != nil {
		return capTransportReason(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "http_status_" + strconv.Itoa(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyCap+1))
	if err != nil {
		return capTransportReason(ctx, err)
	}
	if int64(len(body)) > bodyCap {
		return "body_too_large"
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return "bad_body"
	}
	return ""
}

func capTransportReason(ctx context.Context, err error) string {
	var ne net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	return "unreachable"
}
