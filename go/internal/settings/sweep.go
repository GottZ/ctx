package settings

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
)

// ReencryptSweep is the §3.6 master-key rotation step 4: while
// CTX_SECRETS_KEY_PREV is set, every secret that opens only with the previous
// key is re-sealed with the current key (fresh nonce, key_version+1, one log
// line per name). Secrets that open with NEITHER key are left untouched with
// a WARN — no data loss, no boot abort (W17: nothing in this layer is fatal).
//
// Runs ONLY at boot (cmd/ctxd, between migrations and Bootstrap), never from
// the NOTIFY reload path: the sweep WRITES context_secrets, and the reload
// must stay read-only by contract (§6.5 — the no-notify-loop anchor). The
// sweep's own UPDATEs fire the 051 triggers (audit action='rotate',
// via='sql' + NOTIFY) — the listener is not running yet at boot, and a
// post-boot reload is idempotent anyway.
//
// Without CTX_SECRETS_KEY_PREV the sweep is a silent no-op: the README
// rotation procedure ends with removing _PREV from .env, which disarms it.
func ReencryptSweep(ctx context.Context, pool *pgxpool.Pool) {
	if strings.TrimSpace(os.Getenv(sealbox.EnvKeyPrev)) == "" {
		return
	}
	box, err := sealbox.FromEnv()
	if err != nil {
		// Error names the env var, never a value (sealbox contract).
		slog.Warn("settings: re-encrypt sweep skipped — master keys unusable", "error", err)
		return
	}

	rows, err := store.LoadSealedSecrets(ctx, pool)
	if err != nil {
		// Pre-051 table, DB hiccup: the sweep retries on the next boot.
		slog.Warn("settings: re-encrypt sweep skipped — loading sealed rows failed", "error", err)
		return
	}

	var reencrypted, current, failed int
	for _, row := range rows {
		plaintext, usedPrev, err := box.Open(row.Name, row.Scope, row.Nonce, row.Ciphertext)
		if err != nil {
			// Neither key opens it (sealed under an older lost key, or a
			// tampered row). The row stays untouched — re-entry is `ctx
			// secrets set`, deletion is a deliberate admin action, never a
			// silent sweep casualty. Name+scope are list-surface metadata,
			// safe to log; the error carries no material.
			failed++
			slog.Warn("settings: secret opens with neither current nor previous master key — left untouched",
				"name", row.Name, "scope", row.Scope, "key_version", row.KeyVersion)
			continue
		}
		if !usedPrev {
			current++
			continue
		}
		nonce, ct, err := box.Seal(row.Name, row.Scope, plaintext)
		if err != nil {
			failed++
			slog.Warn("settings: re-seal failed — row left on previous key", "name", row.Name, "scope", row.Scope, "error", err)
			continue
		}
		if err := reencryptRow(ctx, pool, row, nonce, ct); err != nil {
			failed++
			slog.Warn("settings: re-seal write failed — row left on previous key", "name", row.Name, "scope", row.Scope, "error", err)
			continue
		}
		reencrypted++
		slog.Info("settings: re-encrypted secret", "name", row.Name, "scope", row.Scope, "key_version", row.KeyVersion+1)
	}

	if failed == 0 {
		slog.Info("settings: re-encrypt sweep complete — remove "+sealbox.EnvKeyPrev+" from .env",
			"re_encrypted", reencrypted, "already_current", current)
		return
	}
	slog.Warn("settings: re-encrypt sweep finished with failures — keep "+sealbox.EnvKeyPrev+" set and investigate",
		"re_encrypted", reencrypted, "already_current", current, "failed", failed)
}

// reencryptRow writes one re-seal in its own transaction — a single broken
// row must not roll back the rest of the sweep.
func reencryptRow(ctx context.Context, pool *pgxpool.Pool, row store.SealedSecret, nonce, ct []byte) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if err := store.UpdateSecretSeal(ctx, tx, row.Name, row.Scope, nonce, ct); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
