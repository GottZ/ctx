package oai

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelalias"
)

// modelNotFoundMessage is the one text of every 404 model_not_found (B3): it
// names no model, so "does not exist", "foreign tenant", "not granted",
// "disabled" and "broken policy" answer byte-identically and nothing of the
// request is reflected.
const modelNotFoundMessage = "The model does not exist or you do not have access to it."

// modelOwner is the owned_by of every listed model (design/02 §4.4.3): the
// models are ctx's aliases, never a backend's.
const modelOwner = "ctx"

// WriteModelNotFound is the gateway's one 404 for a model name a consumer
// cannot use (design/02 §4.4 step 5, B3).
func WriteModelNotFound(w http.ResponseWriter) {
	WriteCode(w, CodeModelNotFound, modelNotFoundMessage, false)
}

// aliasScope is the scope a consumer resolves model names in: its tenant
// scope ("" ⇒ _global only), and _global only for a ctx-peer, which never
// resolves against a tenant alias (design/05 V-02-c — the rule the admin
// actions apply when they write a grant, handler offerScope).
func aliasScope(ca *auth.ConsumerAuth) string {
	if ca.Kind == auth.KindCtxPeer {
		return ""
	}
	return ca.TenantScope
}

// noAliasID is the alias id of the grant probe for a name that resolves to
// no alias: the nil UUID, which no alias carries (ids are uuidv7), so the
// probe finds nothing — it only makes that 404 cost what the 404 of an alias
// the consumer holds no grant on costs (B3 beyond the bytes: no timing
// difference between "does not exist" and "not released"; Fremd-Review 1
// L-1).
const noAliasID = "00000000-0000-0000-0000-000000000000"

// Resolve is the alias resolution of every gateway endpoint (design/02 §4.4
// steps 1–5, design/01 §3.8): the alias name resolves to in the consumer's
// scope (aliasScope; a tenant alias shadows the _global one; a disabled
// alias or one whose row failed validation is not there — modelalias.Lookup),
// released when the consumer holds all_models or a grant on the alias's ID
// (masterplan K11: an alias created later under the same name is not
// granted). found=false is the one 404 (WriteModelNotFound) for every
// reason; err is a failed grant read — an A-internal fault, 503
// internal_error (K5). The kind check against the endpoint (400
// model_not_supported) comes after this, in the inference handlers.
//
// Without all_models every name costs exactly one grant probe, whether it
// resolves or not (noAliasID), so the time of the 404 does not tell an
// existing alias the consumer may not use from a name that is no alias.
//
// ca must have passed the invariant check (Deps.admit).
func (d Deps) Resolve(ctx context.Context, ca *auth.ConsumerAuth, name string) (modelalias.Alias, bool, error) {
	a, ok := modelalias.Lookup(d.Aliases.Snapshot(), name, aliasScope(ca))
	if ca.AllModels {
		return a, ok, nil
	}
	probe := noAliasID
	if ok {
		probe = a.ID
	}
	granted, err := d.HasGrant(ctx, ca.ConsumerID, probe)
	if err != nil || !ok || !granted {
		return modelalias.Alias{}, false, err
	}
	return a, true, nil
}

// released returns the offers whose id is in ids (the consumer's grants).
func released(offers []modelalias.Alias, ids []string) []modelalias.Alias {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	out := offers[:0]
	for _, a := range offers {
		if set[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

// modelObject is one element of the model list (design/02 §4.4.3): the
// OpenAI Model fields plus the ctx extension, which both SDKs keep as an
// extra field. Nothing of the backend — no role, host or backend model name.
type modelObject struct {
	ID      string    `json:"id"`
	Object  string    `json:"object"`
	Created int64     `json:"created"`
	OwnedBy string    `json:"owned_by"`
	Ctx     modelInfo `json:"ctx"`
}

// modelInfo is the ctx extension: the alias kind and, from the alias policy,
// what a client sizes its requests by — context_length and
// max_output_tokens for chat, dimensions, matryoshka_min and max_inputs for
// embedding. Values the policy does not set are left out (the endpoint's
// default applies, which is no fixed number).
type modelInfo struct {
	Kind            modelalias.Kind `json:"kind"`
	ContextLength   *int            `json:"context_length,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Dimensions      *int            `json:"dimensions,omitempty"`
	MatryoshkaMin   *int            `json:"matryoshka_min,omitempty"`
	MaxInputs       *int            `json:"max_inputs,omitempty"`
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

func optInt(o modelalias.Opt[int]) *int {
	if !o.Set {
		return nil
	}
	v := o.V
	return &v
}

func newModelObject(a modelalias.Alias) modelObject {
	info := modelInfo{Kind: a.Kind}
	switch a.Kind {
	case modelalias.KindChat:
		info.ContextLength, info.MaxOutputTokens = optInt(a.Policy.ContextLength), optInt(a.Policy.MaxOutputTokens)
	case modelalias.KindEmbedding:
		info.Dimensions = optInt(a.Policy.EmbeddingNativeDims)
		info.MatryoshkaMin = optInt(a.Policy.EmbeddingMatryoshkaMin)
		info.MaxInputs = optInt(a.Policy.EmbeddingMaxInputs)
	}
	return modelObject{ID: a.Name, Object: "model", Created: a.CreatedAt.Unix(), OwnedBy: modelOwner, Ctx: info}
}

// writeJSON sends v as a 200 JSON body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("gateway: failed to encode response", "error", err)
	}
}

// grantReadFailed logs a failed grant read and answers it as the A-internal
// fault it is (masterplan K5: 503 internal_error, x-should-retry: false).
func grantReadFailed(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("gateway: grant read failed", "error", err, "path", r.URL.Path, "request_id", w.Header().Get("X-Request-ID"))
	refuseInternal(w, r)
}

// ListModels is GET /v1/models (design/02 §4.4.3): the aliases offered in
// the consumer's scope (modelalias.Offers, sorted by name = id) that it may
// use — all of them with all_models, else those whose id it holds a grant
// on (one read of its grants). Nothing released ⇒ {"object":"list","data":[]}.
func ListModels(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ca, ok := d.admit(w, r)
		if !ok {
			return
		}
		offers := modelalias.Offers(d.Aliases.Snapshot(), aliasScope(ca))
		if !ca.AllModels {
			ids, err := d.GrantIDs(r.Context(), ca.ConsumerID)
			if err != nil {
				grantReadFailed(w, r, err)
				return
			}
			offers = released(offers, ids)
		}
		list := modelList{Object: "list", Data: make([]modelObject, 0, len(offers))}
		for _, a := range offers {
			list.Data = append(list.Data, newModelObject(a))
		}
		writeJSON(w, list)
	})
}

// GetModel is GET /v1/models/* — one element or the one 404. The route is a
// wildcard because alias names may contain "/" (design/02 §3.1).
func GetModel(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ca, ok := d.admit(w, r)
		if !ok {
			return
		}
		name, ok := modelName(r)
		if !ok {
			refuseModelNotFound(w, r)
			return
		}
		a, found, err := d.Resolve(r.Context(), ca, name)
		switch {
		case err != nil:
			grantReadFailed(w, r, err)
		case !found:
			refuseModelNotFound(w, r)
		default:
			writeJSON(w, newModelObject(a))
		}
	})
}

// refuseModelNotFound answers WriteModelNotFound and names the code as the
// access record's refusal reason.
func refuseModelNotFound(w http.ResponseWriter, r *http.Request) {
	if rec := AccessRecordFrom(r.Context()); rec != nil {
		rec.Reason = CodeModelNotFound
	}
	WriteModelNotFound(w)
}

// modelName is the alias name of GET /v1/models/*: the route's wildcard
// ("*", set as a path value by the router). When the request path carried an
// escape the router keeps — the SDKs send qwen/x as qwen%2Fx, and chi routes
// on the raw path then — the wildcard is decoded once.
func modelName(r *http.Request) (string, bool) {
	name := r.PathValue("*")
	if r.URL.RawPath != "" {
		dec, err := url.PathUnescape(name)
		if err != nil {
			return "", false
		}
		name = dec
	}
	return name, name != ""
}
