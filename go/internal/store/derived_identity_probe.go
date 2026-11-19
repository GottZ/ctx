package store

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// derivedIdentityIndexReadySQL tells whether idx_context_derived_identity
// stands as migration 187 builds it (valid, ready, the deparsed definition the
// migration adopts). Migration 187 does not build it above 500 000 rows, nor
// over an INVALID or foreign index of the name. Without it the C-collated
// title comparison of the provenance probes has no title index left, and the
// probes send their form before 187 (provenanceTitleEq).
const derivedIdentityIndexReadySQL = `SELECT EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass('public.idx_context_derived_identity') AND indisvalid AND indisready
		  AND pg_get_indexdef(indexrelid) = 'CREATE INDEX idx_context_derived_identity ON public.context_blocks USING btree (category, title COLLATE "C", scope) WHERE (metadata ? ''provenance''::text)')`

// derivedIdentityProbeInterval is how long a process trusts its last reading
// of derivedIdentityIndexReadySQL for one database. The provenance probes run
// on every fresh insert and identity move, the reading is a catalog query of
// its own (more buffers than the probe it gates, issue #386 follow-up), and
// the index changes only by a migration or an operator's out-of-band build or
// drop: once a minute is enough to follow either, and in that minute a probe
// answers the same in both forms, only at the other form's cost.
const derivedIdentityProbeInterval = time.Minute

// derivedIdentityReading is one reading of derivedIdentityIndexReadySQL.
type derivedIdentityReading struct {
	ready bool
	at    time.Time
}

// derivedIdentityReadings holds the last reading per database the process
// talks to (databaseKey); a process serves one database, the tests many.
var derivedIdentityReadings = struct {
	sync.Mutex
	m map[string]derivedIdentityReading
}{m: map[string]derivedIdentityReading{}}

// databaseKey names the database behind q (host, port, database of its
// connection config), for the handles the store passes to the provenance
// probes: a transaction, a pool, a pooled or a plain connection. ok is false
// for any other handle; its reading is then not kept.
func databaseKey(q pgxdb.Rower) (string, bool) {
	var cfg *pgx.ConnConfig
	switch h := q.(type) {
	case pgx.Tx:
		cfg = h.Conn().Config()
	case *pgxpool.Pool:
		cfg = h.Config().ConnConfig
	case *pgxpool.Conn:
		cfg = h.Conn().Config()
	case *pgx.Conn:
		cfg = h.Config()
	default:
		return "", false
	}
	if cfg == nil {
		return "", false
	}
	return cfg.Host + ":" + strconv.Itoa(int(cfg.Port)) + "/" + cfg.Database, true
}

// derivedIdentityIndexReady reports whether idx_context_derived_identity
// stands, from the process's reading of the last derivedIdentityProbeInterval
// for the database behind q, else from a new reading on q.
func derivedIdentityIndexReady(ctx context.Context, q pgxdb.Rower) (bool, error) {
	key, keep := databaseKey(q)
	if keep {
		derivedIdentityReadings.Lock()
		r, ok := derivedIdentityReadings.m[key]
		derivedIdentityReadings.Unlock()
		if ok && time.Since(r.at) < derivedIdentityProbeInterval {
			return r.ready, nil
		}
	}
	var ready bool
	if err := q.QueryRow(ctx, derivedIdentityIndexReadySQL).Scan(&ready); err != nil {
		return false, fmt.Errorf("store: derived identity index probe: %w", err)
	}
	if keep {
		derivedIdentityReadings.Lock()
		derivedIdentityReadings.m[key] = derivedIdentityReading{ready: ready, at: time.Now()}
		derivedIdentityReadings.Unlock()
	}
	return ready, nil
}

// provenanceTitleEq is the title test of the provenance probes ($2 = the
// title). While idx_context_derived_identity stands it compares under the C
// collation, which that index (title COLLATE "C") serves and the trigram
// indexes (default collation) cannot: one btree descent, whatever the size
// of the archive. Otherwise it is the comparison before migration 187, which
// the planner serves from idx_trgm_title_gist. Both answer alike: equality
// under a deterministic collation is byte equality, and a database's default
// collation is deterministic.
func provenanceTitleEq(ctx context.Context, q pgxdb.Rower) (string, error) {
	ready, err := derivedIdentityIndexReady(ctx, q)
	if err != nil {
		return "", err
	}
	if ready {
		return `title COLLATE "C" = $2`, nil
	}
	return `title = $2`, nil
}
