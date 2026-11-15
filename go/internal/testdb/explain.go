//go:build integration

package testdb

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PlanCost is what EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) reports for one
// execution of a statement.
type PlanCost struct {
	// Buffers is shared hit + shared read of the whole execution (the root
	// node counts its children, init plans and sub plans included);
	// planning is not part of it.
	Buffers int64
	ExecMs  float64
	// Plan is the JSON plan as the server returned it.
	Plan string
}

// paramRef matches a reference to a bind parameter in a plan ($1, $12).
var paramRef = regexp.MustCompile(`\$[0-9]+`)

// ExplainGeneric executes sql once as a prepared statement under
// plan_cache_mode = force_generic_plan and returns its cost: the plan a
// driver-cached statement switches to after its fifth execution (pgx keeps
// one prepared statement per SQL text and connection), which is the plan a
// long-lived pool runs. The arguments are rendered as literals of the
// EXECUTE, so the plan cannot fold them in; a statement with arguments whose
// plan references none of them fails the test, as that would not be the
// generic plan. Supported argument types: string, []string, bool, int,
// int64, float64, time.Time.
func ExplainGeneric(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) PlanCost {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("explain generic: acquire: %v", err)
	}
	defer conn.Release()
	lits := make([]string, len(args))
	for i, a := range args {
		lits[i] = sqlLiteral(t, a)
	}
	for _, st := range []string{
		"SET plan_cache_mode = force_generic_plan",
		"PREPARE testdb_explain_generic AS " + sql,
	} {
		if _, err := conn.Exec(ctx, st); err != nil {
			t.Fatalf("explain generic: %s: %v\n%s", strings.SplitN(st, " ", 2)[0], err, sql)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, "DEALLOCATE testdb_explain_generic")
		_, _ = conn.Exec(ctx, "RESET plan_cache_mode")
	}()
	exec := "EXECUTE testdb_explain_generic"
	if len(lits) > 0 {
		exec += "(" + strings.Join(lits, ", ") + ")"
	}
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+exec).Scan(&raw); err != nil {
		t.Fatalf("explain generic: %v\n%s", err, sql)
	}
	pc := parsePlanCost(t, raw)
	if len(args) > 0 && !paramRef.MatchString(pc.Plan) {
		t.Fatalf("explain generic: the plan references no parameter, so it is not the generic plan:\n%s", pc.Plan)
	}
	return pc
}

// ExplainCustom executes sql once with its arguments bound, as the first five
// executions of a driver-cached statement are planned (custom plan).
func ExplainCustom(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) PlanCost {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain custom: %v\n%s", err, sql)
	}
	return parsePlanCost(t, raw)
}

func parsePlanCost(t *testing.T, raw []byte) PlanCost {
	t.Helper()
	var doc []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("explain json: %v (%d documents)", err, len(doc))
	}
	num := func(k string) int64 {
		v, _ := doc[0].Plan[k].(float64)
		return int64(v)
	}
	return PlanCost{
		Buffers: num("Shared Hit Blocks") + num("Shared Read Blocks"),
		ExecMs:  doc[0].ExecutionTime,
		Plan:    string(raw),
	}
}

// sqlLiteral renders one argument as a SQL literal for EXECUTE.
func sqlLiteral(t *testing.T, a any) string {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	switch v := a.(type) {
	case string:
		return quote(v)
	case []string:
		elems := make([]string, len(v))
		for i, e := range v {
			elems[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(e) + `"`
		}
		return quote("{" + strings.Join(elems, ",") + "}")
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case time.Time:
		return quote(v.UTC().Format("2006-01-02 15:04:05.999999") + "+00")
	default:
		t.Fatalf("explain generic: argument of type %T has no literal form", a)
		return ""
	}
}

// String renders the cost for a test log line.
func (p PlanCost) String() string {
	return fmt.Sprintf("%d buffers, %.3f ms", p.Buffers, p.ExecMs)
}
