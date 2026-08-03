package handler

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/store"
)

// Embed-equivalence probe (backend-test probe:"embed-equivalence"): embeds a
// fixed canonical sample set on the LOCAL reference embed backend and on the
// candidate, compares the DB-relevant vectors (1024-truncated + L2-normalized
// — embed.Embed's own output) per sample and verdicts against
// backends.EmbedEquivalenceThreshold. This is the ONE sanctioned way to earn
// metadata.embed_equivalence_verified for an external embed backend (the
// validate.go hard 422). The candidate may be UNSAVED: the dialog sends its
// full spec and the probe builds an ephemeral Backend from it — validation
// would reject persisting it first, so test-before-create is the only order
// that works.

// embedEquivProbeTimeout bounds ONE embed wire call; it doubles as the
// admission-anchored deadline hint of that call's lease (same doctrine as
// probeChatTimeout).
const embedEquivProbeTimeout = 60 * time.Second

// embedEquivSamples is the canonical probe corpus: both asymmetric prefixes,
// Umlaute/ß, code, symbol noise and a long input — the axes along which
// tokenizer or quantization differences separate vector spaces first.
var embedEquivSamples = []struct {
	label  string
	prefix embed.Prefix
	text   string
}{
	{"doc-de", embed.PrefixDocument, "Die Kalibrierung der Vektorräume erfordert höchste Präzision — Äquivalenz heißt gleiche Richtung, keine stille Drift über Quantisierungsgrenzen hinweg. Schlüsselwörter: Übergabe, Größenordnung, weiß."},
	{"doc-en", embed.PrefixDocument, "PostgreSQL with pgvector stores 1024-dimensional embeddings, Matryoshka-truncated from the native dimensionality and L2-normalized before insertion into the shared corpus."},
	{"doc-code", embed.PrefixDocument, "func l2Normalize(raw []float64) []float32 { var sum float64; for _, x := range raw { sum += x * x }; norm := math.Sqrt(sum); /* divide each component */ }"},
	{"doc-symbols", embed.PrefixDocument, "π ≈ 3.14159 · λ→∞ · SELECT count(*) FROM context_blocks WHERE scope = '_global'; -- 「日本語」 √2 ≠ ¾"},
	{"query-de", embed.PrefixQuery, "Wie funktioniert das Embed-Failover auf ein externes Backend?"},
	{"query-en", embed.PrefixQuery, "vector space equivalence proof for external embedding backends"},
}

// embedEquivProbe is the wire seam (chatProbe doctrine): tests replace it to
// return deterministic vectors without a live backend.
var embedEquivProbe = func(ctx context.Context, b backends.Backend, text string, prefix embed.Prefix) ([]float32, error) {
	vec, _, err := embed.Embed(ctx, b, text, prefix)
	return vec, err
}

// handleEmbedEquivalence runs the probe. Wire/config failures answer HTTP 200
// with verified:false + failure:"…" — the verdict lives in the body (the
// backend-test contract), so the dialog renders every outcome in one path.
func (h *ManageHandler) handleEmbedEquivalence(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest, spec *backendSpec) {
	ctx := r.Context()

	cand, failure := h.equivCandidate(ctx, req, spec)
	if failure == "" && cand.Model == "" {
		failure = "candidate has no embed model (model_map needs an embed or default entry)"
	}

	var ref *backends.Backend
	if failure == "" {
		ref = h.equivReference(ar, cand)
		if ref == nil {
			failure = "no local reference embed backend (enabled, role embed, locality local/lan) in the pool"
		}
	}
	if failure != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "probe": "embed-equivalence", "verified": false, "failure": failure,
		})
		return
	}

	samples := make([]map[string]any, 0, len(embedEquivSamples))
	minCos := 1.0
	for _, s := range embedEquivSamples {
		refVec, err := h.admittedEmbed(ctx, *ref, s.text, s.prefix)
		if err != nil {
			failure = "reference " + ref.Name + ": " + backends.Classify(err, ref.ProviderClass).String()
			break
		}
		candVec, err := h.admittedEmbed(ctx, *cand, s.text, s.prefix)
		if err != nil {
			failure = "candidate: " + backends.Classify(err, cand.ProviderClass).String()
			break
		}
		cos := cosine32(refVec, candVec)
		if cos < minCos {
			minCos = cos
		}
		samples = append(samples, map[string]any{"label": s.label, "cosine": cos})
	}

	result := map[string]any{
		"success": true, "probe": "embed-equivalence",
		"reference": map[string]any{"id": ref.ID, "name": ref.Name, "model": ref.Model},
		"candidate": map[string]any{"name": cand.Name, "model": cand.Model},
		"samples":   samples,
		"threshold": backends.EmbedEquivalenceThreshold,
		"verified":  false,
	}
	if failure != "" {
		result["failure"] = failure
		writeJSON(w, http.StatusOK, result)
		return
	}
	result["min_cosine"] = minCos
	verified := minCos >= backends.EmbedEquivalenceThreshold
	result["verified"] = verified
	if verified {
		// Ready-to-merge metadata patch: the dialog folds it into the save
		// payload verbatim — ONE definition of the proof shape, server-side.
		result["metadata_patch"] = map[string]any{
			"embed_equivalence_verified": true,
			"embed_equivalence_proof": map[string]any{
				"reference":  ref.Name,
				"ref_model":  ref.Model,
				"model":      cand.Model,
				"min_cosine": minCos,
				"samples":    len(samples),
				"checked_at": time.Now().UTC().Format(time.RFC3339),
			},
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// equivCandidate builds the candidate Backend: from the submitted spec when it
// carries a base_url (dialog case, possibly unsaved), else from the pool row
// (table case). Secret resolution failure keeps the candidate keyless — the
// 401 then surfaces as the probe's wire verdict (pool Reload doctrine).
func (h *ManageHandler) equivCandidate(ctx context.Context, req manageRequest, spec *backendSpec) (*backends.Backend, string) {
	if spec.BaseURL == nil || *spec.BaseURL == "" {
		if req.ID == "" {
			return nil, "id or a spec with base_url required"
		}
		b := h.poolBackendByID(req.ID)
		if b == nil {
			return nil, "backend not found"
		}
		cand := *b
		cand.Model = embedModelOf(&cand)
		return &cand, ""
	}

	cand := backends.Backend{
		Name:     "(unsaved)",
		Host:     *spec.BaseURL,
		Protocol: backends.ProtocolOpenAI,
	}
	if spec.Name != nil && *spec.Name != "" {
		cand.Name = *spec.Name
	}
	if spec.Protocol != nil {
		cand.Protocol = backends.Protocol(*spec.Protocol)
	}
	if spec.ProviderClass != nil {
		cand.ProviderClass = *spec.ProviderClass
	}
	if spec.NumCtx != nil {
		cand.NumCtx = *spec.NumCtx
	}
	if len(spec.ModelMap) > 0 {
		mm, err := backends.ParseModelMap(spec.ModelMap)
		if err != nil {
			return nil, "model_map: " + err.Error()
		}
		cand.ModelMap = mm
	}
	cand.Model = embedModelOf(&cand)
	if spec.APIKeyRef != nil && *spec.APIKeyRef != "" {
		if box := h.sealboxOrNil(); box != nil {
			if plaintext, err := store.ResolveSecret(ctx, h.pool, box, *spec.APIKeyRef, store.GlobalScope); err == nil {
				cand.APIKey = string(plaintext)
			}
		}
	}
	return &cand, ""
}

// equivReference picks the local vector-space authority: the
// highest-priority enabled embed backend that is NOT external and NOT the
// candidate itself, restricted to what the caller may see (T37 visibility).
func (h *ManageHandler) equivReference(ar *auth.AuthResult, cand *backends.Backend) *backends.Backend {
	var refs []backends.Backend
	for _, b := range h.backendPool.Snapshot() {
		if !b.Enabled || !b.HasRole(backends.RoleEmbed) || b.Locality == backends.LocalityExternal {
			continue
		}
		if b.ID != "" && b.ID == cand.ID || b.Name == cand.Name {
			continue
		}
		if !backendVisibleToCaller(ar, b.Scope) {
			continue
		}
		refs = append(refs, b)
	}
	if len(refs) == 0 {
		return nil
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Priority != refs[j].Priority {
			return refs[i].Priority > refs[j].Priority
		}
		return refs[i].Name < refs[j].Name
	})
	ref := refs[0]
	ref.Model = embedModelOf(&ref)
	return &ref
}

// embedModelOf resolves the model an embed call on b would use (RoleEmbed →
// "default" → F1 Model field, the ModelFor chain).
func embedModelOf(b *backends.Backend) string {
	if spec := b.ModelFor(backends.RoleEmbed); spec.Model != "" {
		return spec.Model
	}
	return b.ModelFor("default").Model
}

// admittedEmbed runs ONE embed wire call through the dispatch admission layer
// (I-D1 knows no exception — probeChat doctrine, same lease shape).
func (h *ManageHandler) admittedEmbed(ctx context.Context, b backends.Backend, text string, prefix embed.Prefix) ([]float32, error) {
	if h.admitter == nil {
		return nil, errAdmitterNotWired
	}
	lease, runCtx, err := h.admitter.Acquire(ctx, dispatch.Request{
		Target:     dispatch.Target{Origin: b.Host},
		Class:      dispatch.ClassInteractive,
		Role:       backends.RoleEmbed,
		DeadlineIn: embedEquivProbeTimeout,
	})
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	return embedEquivProbe(runCtx, b, text, prefix)
}

// cosine32 is the sample verdict metric. Inputs are embed.Embed outputs
// (already L2-normalized); the explicit norms keep the value exact against
// float32 rounding of the normalization.
func cosine32(a, b []float32) float64 {
	n := min(len(a), len(b))
	var dot, na, nb float64
	for i := range n {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

var errAdmitterNotWired = errNoAdmitter{}

type errNoAdmitter struct{}

func (errNoAdmitter) Error() string { return "dispatch admitter not wired" }

// equivSpecFromRequest parses the request data for the probe dispatch — the
// probe rides backend-test whose spec parse tolerates absence.
func equivSpecFromRequest(data json.RawMessage) *backendSpec {
	var spec backendSpec
	if len(data) > 0 {
		_ = json.Unmarshal(data, &spec)
	}
	return &spec
}
