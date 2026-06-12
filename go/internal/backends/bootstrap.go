package backends

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ExecQuerier adds the write surface the bootstrap needs on top of Querier.
// *pgxpool.Pool satisfies it.
type ExecQuerier interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// BootstrapInput carries the effective config tuples (X1: read from the
// settings>env snapshot via config accessors, NEVER raw os.Getenv — the
// masterplan K6 resolution). backends cannot import config (config imports
// backends), so main.go assembles this struct from the snapshot.
type BootstrapInput struct {
	Chat       Backend  // cfg.ChatBackend()
	Fallback   *Backend // cfg.ChatFallbackBackend(), nil = no second leg
	Embed      Backend  // cfg.EmbedBackend()
	Dream      Backend  // cfg.DreamBackend() (inheritance resolved)
	DreamEmbed Backend  // cfg.DreamEmbedBackend() (inheritance resolved)
	RerankHost string   // cfg.Rerank.Host ("" = no rerank backend)
	RerankKey  string
	RerankModel string
	RerankTimeoutS int // seconds, the rerank package constant
}

// Bootstrap seeds context_backends from the env-era tuples exactly once:
// when the table is empty. After it ran, the TABLE is the source of truth —
// the CTX_*_HOST env vars only feed this path (bridge until P7). ON CONFLICT
// keeps a double-start race idempotent (risk 6.10).
func Bootstrap(ctx context.Context, q ExecQuerier, in BootstrapInput) (int, error) {
	rows, err := q.Query(ctx, `SELECT EXISTS (SELECT 1 FROM context_backends)`)
	if err != nil {
		return 0, fmt.Errorf("backends: bootstrap probe: %w", err)
	}
	var exists bool
	for rows.Next() {
		if err := rows.Scan(&exists); err != nil {
			rows.Close()
			return 0, fmt.Errorf("backends: bootstrap probe: %w", err)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("backends: bootstrap probe: %w", err)
	}
	if exists {
		return 0, nil
	}

	seed, warnings := BootstrapRows(in)
	for _, w := range warnings {
		slog.Warn("backends: bootstrap", "warning", w)
	}

	inserted := 0
	for _, b := range seed {
		timeouts := b.Timeouts
		if timeouts == nil {
			timeouts = map[string]int{}
		}
		tag, err := q.Exec(ctx, `
			INSERT INTO context_backends
			    (name, base_url, protocol, provider_class, trust, locality,
			     roles, model_map, timeouts, num_ctx, priority, enabled)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,true)
			ON CONFLICT (name) DO NOTHING`,
			b.Name, b.Host, string(b.Protocol), b.ProviderClass, string(b.Trust),
			b.Locality, b.Roles, marshalModelMap(b.ModelMap), timeouts,
			nullableInt(b.NumCtx), b.Priority)
		if err != nil {
			return inserted, fmt.Errorf("backends: bootstrap insert %s: %w", b.Name, err)
		}
		inserted += int(tag.RowsAffected())
	}
	slog.Info("backends: bootstrapped from env-era config snapshot", "rows", inserted)
	return inserted, nil
}

func nullableInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func marshalModelMap(m map[string]ModelSpec) map[string]ModelSpec {
	if m == nil {
		return map[string]ModelSpec{}
	}
	return m
}

// BootstrapRows derives the initial pool rows from the env-era tuples —
// pure and unit-testable. Dedup rules (design 03 §3.2): the dream host
// matching the chat host merges into one row (role + per-role model_map
// entry instead of a second row — structurally enforcing the historical
// chat==dream num_ctx invariant); a diverging dream host becomes
// 'herbert-dream'. Same for dream-embed vs embed. Priorities are INITIAL
// DATA for today's backends, not code constants (E2).
func BootstrapRows(in BootstrapInput) (rows []Backend, warnings []string) {
	chat := Backend{
		Name:          "herbert-chat",
		Host:          in.Chat.Host,
		Protocol:      in.Chat.Protocol,
		ProviderClass: ProviderLlamaCpp,
		Trust:         TrustFull,
		Locality:      deriveOrLAN(in.Chat.Host),
		Roles:         []string{RoleSynthesis, RoleTranslate, RoleChat, RoleDigest},
		ModelMap:      map[string]ModelSpec{"default": specFor(in.Chat)},
		NumCtx:        in.Chat.NumCtx,
		Priority:      100,
		Enabled:       true,
	}
	warnings = append(warnings, keyWarning(in.Chat, "herbert-chat")...)

	if sameHost(in.Dream.Host, in.Chat.Host) {
		chat.Roles = append(chat.Roles, RoleDream)
		// Per-role override only when the dream tuple actually diverges.
		if in.Dream.Model != in.Chat.Model || in.Dream.Think != in.Chat.Think {
			chat.ModelMap[RoleDream] = specFor(in.Dream)
		}
	} else if in.Dream.Host != "" {
		rows = append(rows, Backend{
			Name:          "herbert-dream",
			Host:          in.Dream.Host,
			Protocol:      in.Dream.Protocol,
			ProviderClass: ProviderLlamaCpp,
			Trust:         TrustFull,
			Locality:      deriveOrLAN(in.Dream.Host),
			Roles:         []string{RoleDream},
			ModelMap:      map[string]ModelSpec{"default": specFor(in.Dream)},
			NumCtx:        in.Dream.NumCtx,
			Priority:      100,
			Enabled:       true,
		})
		warnings = append(warnings, keyWarning(in.Dream, "herbert-dream")...)
	}
	rows = append([]Backend{chat}, rows...)

	if in.Fallback != nil && in.Fallback.Host != "" {
		fb := *in.Fallback
		// Empty model/think inherit the primary (chatWithFallback semantics).
		// The pool model has no cross-row inheritance — materialize it.
		if fb.Model == "" {
			fb.Model = in.Chat.Model
		}
		if fb.Think == "" {
			fb.Think = in.Chat.Think
		}
		row := Backend{
			Name:          "llama-cpu",
			Host:          fb.Host,
			Protocol:      fb.Protocol,
			ProviderClass: ProviderLlamaCpp,
			Trust:         TrustFull,
			Locality:      deriveOrLAN(fb.Host),
			Roles:         []string{RoleSynthesis}, // deliberately NO dream (risk 6.5: CPU tears DreamTimeout)
			ModelMap:      map[string]ModelSpec{"default": specFor(fb)},
			NumCtx:        in.Chat.NumCtx,
			Priority:      10,
			Enabled:       true,
		}
		if secs := int(fb.Timeout.Seconds()); secs > 0 {
			row.Timeouts = map[string]int{RoleSynthesis: secs}
		}
		rows = append(rows, row)
		warnings = append(warnings, keyWarning(fb, "llama-cpu")...)
	}

	embed := Backend{
		Name:          "llama-embed",
		Host:          in.Embed.Host,
		Protocol:      in.Embed.Protocol,
		ProviderClass: ProviderLlamaCpp,
		Trust:         TrustFull,
		Locality:      deriveOrLAN(in.Embed.Host),
		Roles:         []string{RoleEmbed},
		ModelMap:      map[string]ModelSpec{"default": {Model: in.Embed.Model}},
		NumCtx:        in.Embed.NumCtx,
		Priority:      100,
		Enabled:       true,
	}
	if sameHost(in.DreamEmbed.Host, in.Embed.Host) && in.DreamEmbed.Model == in.Embed.Model {
		embed.Roles = append(embed.Roles, RoleDreamEmbed)
	} else if in.DreamEmbed.Host != "" {
		// Diverging dream-embed keeps its OWN role instead of a second row in
		// the embed chain — a second embed row would change the role's
		// ordering semantics; the dedicated role preserves the historical
		// query-embed vs dream-embed split losslessly.
		rows = append(rows, Backend{
			Name:          "dream-embed",
			Host:          in.DreamEmbed.Host,
			Protocol:      in.DreamEmbed.Protocol,
			ProviderClass: ProviderLlamaCpp,
			Trust:         TrustFull,
			Locality:      deriveOrLAN(in.DreamEmbed.Host),
			Roles:         []string{RoleDreamEmbed},
			ModelMap:      map[string]ModelSpec{"default": {Model: in.DreamEmbed.Model}},
			NumCtx:        in.DreamEmbed.NumCtx,
			Priority:      100,
			Enabled:       true,
		})
		warnings = append(warnings, keyWarning(in.DreamEmbed, "dream-embed")...)
	}
	rows = append(rows, embed)
	warnings = append(warnings, keyWarning(in.Embed, "llama-embed")...)

	if in.RerankHost != "" {
		row := Backend{
			Name:          "herbert-rerank",
			Host:          in.RerankHost,
			Protocol:      "rerank",
			ProviderClass: ProviderLlamaCpp,
			Trust:         TrustFull,
			Locality:      deriveOrLAN(in.RerankHost),
			Roles:         []string{RoleRerank},
			ModelMap:      map[string]ModelSpec{"default": {Model: in.RerankModel}},
			Priority:      100,
			Enabled:       true,
		}
		if in.RerankTimeoutS > 0 {
			row.Timeouts = map[string]int{RoleRerank: in.RerankTimeoutS}
		}
		rows = append(rows, row)
		if in.RerankKey != "" {
			warnings = append(warnings, "herbert-rerank: env api key cannot be bootstrapped — create an F2 secret and set api_key_ref")
		}
	}

	return rows, warnings
}

// keyWarning flags env api keys that cannot travel into the table (plaintext
// keys never live in context_backends — F2 secrets are the only carrier).
func keyWarning(b Backend, name string) []string {
	if b.APIKey == "" {
		return nil
	}
	return []string{name + ": env api key cannot be bootstrapped — create an F2 secret and set api_key_ref"}
}

// specFor builds the model_map entry of one env tuple. The think toggle
// travels as params.think: per-(backend, role) model behavior is exactly
// what ModelSpec.Params exists for.
func specFor(b Backend) ModelSpec {
	spec := ModelSpec{Model: b.Model}
	switch b.Think {
	case "true":
		spec.Params = map[string]any{"think": true}
	case "false":
		spec.Params = map[string]any{"think": false}
	}
	return spec
}

func sameHost(a, b string) bool {
	return a != "" && strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// deriveOrLAN classifies the host, defaulting to lan when the URL does not
// parse (bootstrap must not fail on a tuple the config layer already
// validated; lan is the conservative middle for the audit dimension).
func deriveOrLAN(baseURL string) string {
	loc, err := DeriveLocality(baseURL)
	if err != nil {
		return LocalityLAN
	}
	return loc
}
