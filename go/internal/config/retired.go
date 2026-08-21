package config

import (
	"sort"
	"strings"
)

// retiredSettingKeys maps every settings key whose living home moved to the F3
// backend pool onto the pool location that owns the value now. It is the ONE
// list of this retirement: the boot WARN over still-set env vars, the tombstone
// tripwire and the docs/runbook sections take their names from here instead of
// each carrying its own transcript of 29 strings (K4 — one tripwire, three
// ingredients).
//
// The map is DATA, never an API surface. Per decision E13 the retired keys
// answer 404 through the ordinary unknownKey path once the registry cut lands —
// no tombstone status, no hint field, nothing on the wire that separates them
// from a typo. The move targets live here so the places that DO carry the
// message (BREAKING tag annotation, runbook, README upgrade-hop paragraph, the
// delete migration's notice) can name one destination, spelled once.
//
// gaming.active / gaming.disabled_backends are deliberately NOT in here. They
// are the older retirement vintage (U01-W5, see the closing comment of Config's
// pool group): never env-sourced, no pool destination, unregistered since their
// cutover — they already answer 404 today and contribute no env name to sweep.
// Two vintages, two behaviours; retired_test.go pins the separation.
var retiredSettingKeys = map[string]string{
	"chat.host":     "context_backends.base_url (chat-role row)",
	"chat.api_key":  "context_backends.api_key_ref (F2 secret, chat-role row)",
	"chat.protocol": "context_backends.protocol (chat-role row)",
	"chat.model":    "context_backends.model_map (chat role)",
	"chat.num_ctx":  "context_backends.num_ctx (chat-role row)",
	"chat.think":    "context_backends.model_map params.think (chat role)",

	"chat_fallback.host":     "context_backends.base_url (lower-priority chat-role row)",
	"chat_fallback.api_key":  "context_backends.api_key_ref (F2 secret, lower-priority chat-role row)",
	"chat_fallback.protocol": "context_backends.protocol (lower-priority chat-role row)",
	"chat_fallback.timeout":  "context_backends.timeouts (chat role, seconds)",

	"embed.host":     "context_backends.base_url (embed-role row)",
	"embed.api_key":  "context_backends.api_key_ref (F2 secret, embed-role row)",
	"embed.protocol": "context_backends.protocol (embed-role row)",
	"embed.model":    "context_backends.model_map (embed role)",
	"embed.num_ctx":  "context_backends.num_ctx (embed-role row)",

	"dream.host":     "context_backends.base_url (dream-role row)",
	"dream.api_key":  "context_backends.api_key_ref (F2 secret, dream-role row)",
	"dream.protocol": "context_backends.protocol (dream-role row)",
	"dream.model":    "context_backends.model_map (dream role)",
	"dream.num_ctx":  "context_backends.num_ctx (dream-role row)",
	"dream.think":    "context_backends.model_map params.think (dream role)",

	"dream_embed.host":     "context_backends.base_url (dream-embed-role row)",
	"dream_embed.api_key":  "context_backends.api_key_ref (F2 secret, dream-embed-role row)",
	"dream_embed.protocol": "context_backends.protocol (dream-embed-role row)",
	"dream_embed.model":    "context_backends.model_map (dream-embed role)",
	"dream_embed.num_ctx":  "context_backends.num_ctx (dream-embed-role row)",

	"rerank.host":    "context_backends.base_url (rerank-role row)",
	"rerank.api_key": "context_backends.api_key_ref (F2 secret, rerank-role row)",
	"rerank.model":   "context_backends.model_map (rerank role)",
}

// RetiredEnvNames returns the env var names of the retired keys, sorted — the
// single name source for the boot WARN over still-set legacy vars and for the
// tombstone tripwire. Sorted rather than registry-ordered because the map has no
// order to inherit and a sweep that logs in a stable order is diffable.
func RetiredEnvNames() []string {
	out := make([]string, 0, len(retiredSettingKeys))
	for key := range retiredSettingKeys {
		out = append(out, retiredEnvName(key))
	}
	sort.Strings(out)
	return out
}

// RetiredKeyNames returns the canonical settings keys of the retirement,
// sorted — the name source of the boot-time row-shadow sweep (A06-A1,
// design/06 §3.4 #2), which looks for context_settings rows on exactly these
// keys across every scope. RetiredEnvNames() cannot serve that sweep: the
// context_settings rows are keyed by the canonical key, not by the env name,
// and deriving one from the other outside this file would plant the second
// transcript the whole file exists to prevent. Same sorted-for-diffability
// contract as RetiredEnvNames().
func RetiredKeyNames() []string {
	out := make([]string, 0, len(retiredSettingKeys))
	for key := range retiredSettingKeys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// retiredEnvName derives the env var of a retired key: "CTX_" + the upper-cased
// key with '.' replaced by '_'. The derivation is mechanical for all 29 keys —
// retired_test.go pins it against the live registry entry for as long as the
// key has one, and against the ABSENCE of the name from EnvVars() once its
// wave cut it, so a hand-written second list can never drift in from either
// side of the cut.
func retiredEnvName(key string) string {
	return "CTX_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}
