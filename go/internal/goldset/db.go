package goldset

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DB is the read-only view of the live store used to draw the query sets.
// The connection sets default_transaction_read_only=on as a server-side guard:
// this tool must never be able to write to the corpus it measures.
type DB struct{ conn *pgx.Conn }

// Open dials dsn read-only.
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["application_name"] = "ctx-goldset"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &DB{conn: conn}, nil
}

// Close releases the connection.
func (d *DB) Close(ctx context.Context) error { return d.conn.Close(ctx) }

// Block is a retrievable corpus block.
type Block struct {
	ID       string
	Title    string
	Content  string
	TypeName string
	Language string
}

// retrievableFilter is the corpus definition shared by G-KI and G-Q: not
// archived, and its type policy in context_block_types is not "excluded"
// (design 04 §4.5). Types without an explicit policy default to full-pass,
// matching the registry default.
const retrievableFilter = `
	FROM context_blocks b
	JOIN context_block_types t ON t.name = b.type_name
	WHERE NOT b.is_archived
	  AND coalesce(t.config->'retrieval'->>'policy', 'full-pass') <> 'excluded'`

// RetrievableCount is the size of the drawable corpus, for the stamp.
func (d *DB) RetrievableCount(ctx context.Context) (int, error) {
	var n int
	err := d.conn.QueryRow(ctx, `SELECT count(*)`+retrievableFilter).Scan(&n)
	return n, err
}

// RetrievableBlocks draws blocks ordered by a seeded, stable hash so the same
// seed yields the same sample without an ORDER BY random() that changes on
// every call. minContent skips near-empty blocks, which cannot carry a
// content-answerable question.
func (d *DB) RetrievableBlocks(ctx context.Context, seed int64, limit, minContent int) ([]Block, error) {
	q := `SELECT b.id::text, b.title, b.content, b.type_name, coalesce(b.language, '')` +
		retrievableFilter + `
	  AND length(b.content) >= $2
	ORDER BY md5(b.id::text || $1::text)
	LIMIT $3`
	rows, err := d.conn.Query(ctx, q, fmt.Sprint(seed), minContent, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Block
	for rows.Next() {
		var b Block
		if err := rows.Scan(&b.ID, &b.Title, &b.Content, &b.TypeName, &b.Language); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CorpusMaxCreatedAt is the contamination stamp of §5.3(c): every top-k hit
// created after this instant is flagged as contamination-suspect at score time.
func (d *DB) CorpusMaxCreatedAt(ctx context.Context) (string, error) {
	var ts string
	err := d.conn.QueryRow(ctx,
		`SELECT coalesce(to_char(max(created_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.USZ'), '')
		 FROM context_blocks`).Scan(&ts)
	return ts, err
}

// AccessLogQueries draws distinct real query texts for G-REAL.
//
// The source != 'armsweep' filter is mandatory (§4.5, §5.3d): the driver's own
// arm_ranks requests log their query text into this very table, so without the
// filter every redraw would sample the constructed G-KI/G-Q texts of earlier
// campaigns instead of real user queries.
func (d *DB) AccessLogQueries(ctx context.Context, days, minLen, limit int, seed int64) ([]string, error) {
	q := `SELECT query_text
	      FROM (
	        SELECT DISTINCT query_text
	        FROM context_access_log
	        WHERE action = 'query'
	          AND query_text IS NOT NULL
	          AND length(query_text) > $1
	          AND created_at > now() - make_interval(days => $2)
	          AND metadata->>'source' IS DISTINCT FROM 'armsweep'
	      ) s
	      ORDER BY md5(query_text || $3::text)
	      LIMIT $4`
	rows, err := d.conn.Query(ctx, q, minLen, days, fmt.Sprint(seed), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AccessLogCandidateCount is the size of the G-REAL draw pool before redaction.
func (d *DB) AccessLogCandidateCount(ctx context.Context, days, minLen int) (int, error) {
	var n int
	err := d.conn.QueryRow(ctx, `
		SELECT count(DISTINCT query_text)
		FROM context_access_log
		WHERE action = 'query'
		  AND query_text IS NOT NULL
		  AND length(query_text) > $1
		  AND created_at > now() - make_interval(days => $2)
		  AND metadata->>'source' IS DISTINCT FROM 'armsweep'`, minLen, days).Scan(&n)
	return n, err
}

// LookupBackend reads one context_backends row by name.
func (d *DB) LookupBackend(ctx context.Context, name string) (Backend, error) {
	var b Backend
	err := d.conn.QueryRow(ctx, `
		SELECT name, base_url, locality, trust, roles, enabled, model_map, extra_body
		FROM context_backends WHERE name = $1 ORDER BY scope LIMIT 1`, name).
		Scan(&b.Name, &b.BaseURL, &b.Locality, &b.Trust, &b.Roles, &b.Enabled, &b.ModelMap, &b.ExtraBody)
	if err != nil {
		return b, fmt.Errorf("backend %q: %w", name, err)
	}
	return b, nil
}
