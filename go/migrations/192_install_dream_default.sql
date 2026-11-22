-- =============================================================================
-- 192_install_dream_default.sql — a new installation runs Dream by default,
-- an upgraded one keeps what it had (context_install.dream_default, issue #492)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- What. One row per database: dream_default, the value dream.enabled takes
-- when CTX_DREAM_ENABLED is not set. true on a database this release installs,
-- false on every database it upgrades. ctxd reads the row at boot, after the
-- migrations (store.LoadDreamInstallDefault), and hands it to the config
-- loader as the installation's default (config.SetDreamInstallDefault): the
-- effective value then reports the source "install". CTX_DREAM_ENABLED keeps
-- deciding whenever it is set, in both directions; the built-in default of
-- the key stays false.
--
-- Why a row and not a new built-in default. Dream calls the chat model in the
-- background for as long as it runs. An installation that never set
-- CTX_DREAM_ENABLED has run without Dream; switching the built-in default
-- would switch Dream on for it with the upgrade — cost on a metered backend
-- nobody ordered. Whether a database is new is known to exactly one party:
-- the migration runner, at the run that finds no migration history
-- (store.checkBaselinePrecondition). The runner sets the transaction-local
-- setting ctx.fresh_install for every file it applies (store.freshInstallSetting):
-- 'on' on that run, 'off' on every other, whatever the session brings along.
-- This file reads it; on a database with history the row says false.
--
-- What counts as an upgrade. Everything with a migration history: an
-- installation of an earlier release, and a dump of one restored into a new
-- instance (the dump carries its _migrations rows). A dump
-- restored over a database this release had already installed, without
-- dropping it first, arrives without this file in its history: the next boot
-- applies the file again, and ON CONFLICT DO UPDATE writes false over the
-- true the first boot left — the restored data is the old installation's, so
-- is its default. A dump of a database this release installed carries its row
-- and its _migrations entry: it keeps true.
--
-- A first boot that stops between the baseline and this file (a crash, a
-- killed container) leaves a history behind; the next run is no fresh run and
-- the row says false. So does a second ctxd instance that starts on the same
-- empty database after the first committed the baseline and applies this file
-- itself (issue #398, several instances booting together); one that started
-- before that commit read the empty history too and writes true. Both
-- failures fall toward the old default; CTX_DREAM_ENABLED=true switches Dream
-- on.
--
-- Cost and locks. One new table of one row; nothing else is read or locked.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS; the INSERT ... ON CONFLICT DO
-- UPDATE leaves one row.
--
-- Way back: DROP TABLE context_install; DELETE FROM _migrations WHERE
-- version = 192. An older binary ignores the table.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_install (
    singleton     BOOLEAN     PRIMARY KEY DEFAULT true
                  CONSTRAINT ck_install_singleton CHECK (singleton),
    -- dream.enabled when CTX_DREAM_ENABLED is not set.
    dream_default BOOLEAN     NOT NULL,
    decided_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO context_install (singleton, dream_default)
VALUES (true, COALESCE(current_setting('ctx.fresh_install', true) = 'on', false))
ON CONFLICT (singleton) DO UPDATE
    SET dream_default = EXCLUDED.dream_default, decided_at = EXCLUDED.decided_at;
