package store

import (
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CustomPlanMode is the query mode of a statement that is planned with its
// parameters on every execution (issue #536): an unnamed statement, which
// PostgreSQL plans as a custom plan each time, where pgx's statement cache
// would switch a named one to the generic plan from its sixth execution on a
// connection. QueryExecModeCacheDescribe does it in one round trip but needs
// the connection's description cache; a connection string can turn that cache
// off (description_cache_capacity=0, through the sslmode setting the DSN
// carries as written or a libpq service file), and pgx then refuses the mode
// on every call. Such a pool gets QueryExecModeExec: the same unnamed
// statement in one round trip, its parameters sent as text with the types pgx
// derives from the Go values. (QueryExecModeDescribeExec describes and
// executes in two round trips, which on a pool outside a transaction may
// reach two server connections behind a connection pooler.)
func CustomPlanMode(pool *pgxpool.Pool) pgx.QueryExecMode {
	if pool.Config().ConnConfig.DescriptionCacheCapacity > 0 {
		return pgx.QueryExecModeCacheDescribe
	}
	return pgx.QueryExecModeExec
}
