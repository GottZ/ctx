//go:build integration

// Issue #555: a tenant prune that reports success leaves no row in any table
// of the database that points at the tenant or at one of its scopes — except
// the audit and log rows the classification below keeps on purpose, each
// with its reason.
//
// The test reads the schema (information_schema.columns of the public BASE
// TABLEs) for every column whose name holds "scope" or "tenant" and fails on
// a column the classification does not name, so a new table with a scope
// column is red until it is classified. Every table is filled through its
// production writer for the pruned tenant T and for a control tenant C (the
// same fill); rows of the default tenant that point at the scopes of T or C
// (keys, a chat session's read scopes, a stage, grants) come with them.
// Before the prune every counted column must carry a row of T and of C (else
// the fill is missing, not the drain); after it no drained column may carry
// a row of T, and no counted column may have lost a row of C. A tenant
// created with T's slug and assigned T's scope names then reads nothing of T.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestTenantPruneLeavesNoRowOfTheTenant$' ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/store"
)

// tpsClass is what the prune owes a column.
type tpsClass int

const (
	// tpsDrained: after the prune no row of the column points at the tenant
	// or at one of its scopes.
	tpsDrained tpsClass = iota + 1
	// tpsKept: rows stay on purpose (audit, logs); the reason is named.
	tpsKept
	// tpsNotCtxScope: the column's name holds "scope" or "tenant", but its
	// values are not a ctx scope or tenant (OAuth scope strings, counts,
	// hashes, flags).
	tpsNotCtxScope
)

func (c tpsClass) String() string {
	switch c {
	case tpsDrained:
		return "drained"
	case tpsKept:
		return "kept"
	case tpsNotCtxScope:
		return "notCtxScope"
	}
	return "?"
}

type tpsEntry struct {
	class tpsClass
	why   string
}

// tpsColumns classifies every column of the schema whose name holds "scope"
// or "tenant" (the rule covers scope, home_scope, tenant_scope, *_scope,
// *_scopes, scopes, read_scopes, allowed_scopes, write_scopes, scope_set,
// scope_s, scope_t, scope_a, scope_b, tenant_id, grantee_tenant). A column
// the schema has and this map lacks fails the test.
var tpsColumns = map[string]tpsEntry{
	// Keys: the tenant's own, and keys of another tenant that name its scopes.
	"context_api_keys.home_scope":     {tpsDrained, "a key homed in a scope of the tenant reads and writes the next owner of that name"},
	"context_api_keys.allowed_scopes": {tpsDrained, "a key reading a scope of the tenant reads the next owner of that name"},
	"context_api_keys.write_scopes":   {tpsDrained, "a key writing a scope of the tenant writes into the next owner of that name"},
	"context_api_keys.tenant_id":      {tpsDrained, "the tenant's keys"},
	"context_api_keys.tenant_role":    {tpsNotCtxScope, "a role name (owner/member), not a tenant"},
	// Scope-carried data.
	"context_blocks.scope":              {tpsDrained, "block content of the tenant"},
	"context_dream_links.scope":         {tpsDrained, "links between blocks of the tenant"},
	"context_structural_links.scope":    {tpsDrained, "links between blocks of the tenant"},
	"context_blobs.scope":               {tpsDrained, "file content of the tenant"},
	"context_sources.scope":             {tpsDrained, "ingest sources of the tenant"},
	"context_chat_sessions.scope":       {tpsDrained, "chat sessions of the tenant"},
	"context_chat_sessions.read_scopes": {tpsDrained, "a session reading a scope of the tenant reads the next owner of that name"},
	"context_secrets.scope":             {tpsDrained, "sealed secrets of the tenant (forge tokens, backend keys)"},
	"context_pending_writes.scope":      {tpsDrained, "staged block content in a scope of the tenant, whoever's key staged it"},
	"context_block_counts.scope":        {tpsDrained, "per-scope counters; stats read them by scope"},
	"context_block_count_deltas.scope":  {tpsDrained, "per-scope counter deltas; stats read them by scope"},
	// Per-scope configuration.
	"context_settings.scope":            {tpsDrained, "setting overrides of the tenant; the next owner of the name would inherit them"},
	"context_settings_audit.scope":      {tpsKept, "the audit rows of every entity but a setting (backends, types, profiles, hues, quota, aliases, keys): read by tenant id (store.ListAudit, metadata.tenant_id — the deleted tenant's, which never returns) or by the server admin, never by scope; the record of the offboarding. A setting's rows are drained (the extra below)"},
	"context_backends.scope":            {tpsDrained, "inference backends of the tenant"},
	"context_disable_profiles.scope":    {tpsDrained, "disable profiles of the tenant"},
	"context_model_aliases.scope":       {tpsDrained, "model aliases of the tenant"},
	"context_graph_category_hues.scope": {tpsDrained, "graph colours of the tenant"},
	"context_block_types.scope":         {tpsDrained, "block-type overlays of the tenant"},
	"context_tenant_quota.scope":        {tpsDrained, "the tenant's quota"},
	// Tenant registry and grants.
	"context_tenant_scopes.scope":          {tpsDrained, "the tenant's scope mapping"},
	"context_tenant_scopes.tenant_id":      {tpsDrained, "the tenant's scope mapping"},
	"context_tenant_grants.granted_scope":  {tpsDrained, "a grant on a scope of the tenant would read the next owner of that name"},
	"context_tenant_grants.grantee_tenant": {tpsDrained, "grants to the tenant"},
	"context_block_grants.grantee_tenant":  {tpsDrained, "block grants to the tenant"},
	"context_tenants.max_scopes":           {tpsNotCtxScope, "an integer cap"},
	// Model gateway.
	"context_model_consumers.tenant_id":    {tpsDrained, "model consumers of the tenant"},
	"context_model_consumers.tenant_scope": {tpsDrained, "a consumer bound to a scope of the tenant"},
	"context_gateway_limits.tenant_id":     {tpsDrained, "gateway limits of the tenant"},
	// Projects.
	"context_projects.scope":     {tpsDrained, "projects of the tenant"},
	"context_projects.tenant_id": {tpsDrained, "projects of the tenant"},
	// Measurements and logs.
	"context_recall_runs.scope": {tpsDrained, "a recall measurement of a scope of the tenant; /api/status lists the latest per (stratum, scope, k) (handler/status.go:1368), so the next owner of the name would show it"},
	"context_write_log.scope":   {tpsDrained, "the guard's and the dream's decisions with the blocks' titles; the daily report counts a scope's decisions of the day by scope (dream/synthesize_report.go:419), so the next owner's report would count the tenant's"},
	// Graph overview: written only by the overview run.
	"graph_cluster_node.scope":                  {tpsDrained, "graph aggregate of the tenant's blocks"},
	"graph_cluster_member.scope":                {tpsDrained, "graph aggregate of the tenant's blocks"},
	"graph_cluster_edge.scope_s":                {tpsDrained, "graph aggregate of the tenant's blocks"},
	"graph_cluster_edge.scope_t":                {tpsDrained, "graph aggregate of the tenant's blocks"},
	"graph_cluster_centroid.scope":              {tpsDrained, "graph aggregate of the tenant's embeddings"},
	"graph_cluster_topic.scope":                 {tpsDrained, "graph topics of the tenant (labels from its blocks)"},
	"graph_cluster_topic_edge.scope_a":          {tpsDrained, "graph aggregate of the tenant's blocks"},
	"graph_cluster_topic_edge.scope_b":          {tpsDrained, "graph aggregate of the tenant's blocks"},
	"graph_cluster_super.scope":                 {tpsDrained, "graph aggregate of the tenant's blocks"},
	"graph_cluster_super_member.scope":          {tpsDrained, "graph aggregate of the tenant's blocks"},
	"graph_overview_meta.scope":                 {tpsDrained, "graph aggregate stamp of the tenant's partition"},
	"graph_overview_run.scope_set":              {tpsDrained, "run journal of the overview worker for the tenant's partition: the list of its scope names goes with them"},
	"graph_overview_run.scope_key":              {tpsNotCtxScope, "the advisory lock key hashed from the scope set, not a scope"},
	"graph_overview_run.cross_scope_components": {tpsNotCtxScope, "an integer count"},
	// OAuth: scope strings of the OAuth protocol, not ctx scopes.
	"context_access_tokens.scope":                  {tpsNotCtxScope, "OAuth scope string"},
	"context_oauth_codes.scope":                    {tpsNotCtxScope, "OAuth scope string"},
	"context_oauth_clients.scopes":                 {tpsNotCtxScope, "OAuth scope strings"},
	"context_oauth_providers.scopes":               {tpsNotCtxScope, "OAuth scope strings"},
	"context_oauth_providers.single_tenant_issuer": {tpsNotCtxScope, "a boolean flag of the provider"},
}

// tpsExtra is a check outside the column rule: a value that names a scope or
// a tenant inside a text or JSON column, the tenant row itself, and the
// child rows of the tenant's rows (by the ids the fill recorded). sql is the
// predicate on t (the table) and p (p.tps_scopes text[], p.tps_tenant uuid,
// p.tps_ids text[] — the owner's recorded ids under key ids).
type tpsExtra struct {
	name  string
	table string
	class tpsClass
	pred  string
	ids   string
	why   string
}

var tpsExtras = []tpsExtra{
	{"context_embed_excluded_queue.grp", "context_embed_excluded_queue", tpsDrained,
		`t.grp = 't:' || p.tps_tenant::text OR t.grp = ANY (SELECT 's:' || s FROM unnest(p.tps_scopes) s)`, "",
		"reconcile groups t:<tenant> / s:<scope> of the tenant"},
	{"context_access_log.metadata->>'scope'", "context_access_log", tpsKept,
		`t.metadata->>'scope' = ANY (p.tps_scopes)`, "",
		"audit log: TENANT-DECISION(prune-audit-tables), store/tenant.go — kept; no reader of metadata.scope in the binary"},
	{"context_tenants.id", "context_tenants", tpsDrained, `t.id = p.tps_tenant`, "", "the tenant row"},
	{"context_settings_audit.scope (entity_type setting)", "context_settings_audit", tpsDrained,
		`t.entity_type = 'setting' AND t.scope = ANY (p.tps_scopes)`, "",
		"GET /api/settings/{key} lists a setting's audit by the caller's home scope (store.ListSettingAudit, handler/settings.go:307): the next owner's admin would read the tenant's setting history"},
	{"context_chat_messages.session_id", "context_chat_messages", tpsDrained,
		`t.session_id::text = ANY (p.tps_ids)`, "sessions", "messages of the tenant's sessions"},
	{"context_block_grants.block_id", "context_block_grants", tpsDrained,
		`t.block_id::text = ANY (p.tps_ids)`, "grantedBlocks", "grants on the tenant's blocks"},
	{"context_project_sync_runs.project_id", "context_project_sync_runs", tpsDrained,
		`t.project_id::text = ANY (p.tps_ids)`, "projects", "sync runs of the tenant's projects"},
	{"context_project_sync_map.project_id", "context_project_sync_map", tpsDrained,
		`t.project_id::text = ANY (p.tps_ids)`, "projects", "sync map of the tenant's projects"},
	{"context_webhook_events.project_id", "context_webhook_events", tpsDrained,
		`t.project_id::text = ANY (p.tps_ids)`, "projects", "webhook events of the tenant's projects"},
	{"context_model_keys.consumer_id", "context_model_keys", tpsDrained,
		`t.consumer_id::text = ANY (p.tps_ids)`, "consumers", "model keys of the tenant's consumers"},
	{"context_model_consumer_grants.consumer_id", "context_model_consumer_grants", tpsDrained,
		`t.consumer_id::text = ANY (p.tps_ids)`, "consumers", "model grants of the tenant's consumers"},
	{"context_disable_profile_backends.profile_id", "context_disable_profile_backends", tpsDrained,
		`t.profile_id::text = ANY (p.tps_ids)`, "profiles", "members of the tenant's disable profiles"},
}

// tpsColumn is one column the schema rule found.
type tpsColumn struct {
	table, column, udt string
}

func (c tpsColumn) name() string { return c.table + "." + c.column }

// tpsCheck is one counted check: a scanned column or an extra.
type tpsCheck struct {
	name  string
	class tpsClass
	sql   string
	ids   string
	why   string
}

// tpsOwner is a tenant the fill writes for and the ids it recorded.
type tpsOwner struct {
	label  string
	slug   string
	tenant string
	scopes []string
	keyID  string
	ids    map[string][]string
	// what the next-owner readers look for
	hueCategory, profile, alias, blockType, session, foreignHomeKey, foreignWriteKey string
}

func tpsQ(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func tpsTx(t *testing.T, pool *pgxpool.Pool, what string, fn func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("%s: begin: %v", what, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("%s: commit: %v", what, err)
	}
}

// tpsSchemaColumns reads every column of a public base table whose name holds
// "scope" or "tenant".
func tpsSchemaColumns(t *testing.T, pool *pgxpool.Pool) []tpsColumn {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT c.table_name::text, c.column_name::text, c.udt_name::text
		  FROM information_schema.columns c
		  JOIN information_schema.tables tb ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		 WHERE c.table_schema = 'public' AND tb.table_type = 'BASE TABLE'
		   AND (c.column_name LIKE '%scope%' OR c.column_name LIKE '%tenant%')
		 ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	cols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tpsColumn, error) {
		var c tpsColumn
		err := r.Scan(&c.table, &c.column, &c.udt)
		return c, err
	})
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	return cols
}

// tpsChecks builds the counted checks from the schema and the extras; a
// column without a classification, a classification without a column and a
// counted column of a type without a predicate are errors.
func tpsChecks(t *testing.T, cols []tpsColumn) []tpsCheck {
	t.Helper()
	var unclassified, untyped []string
	seen := map[string]bool{}
	var checks []tpsCheck
	for _, c := range cols {
		seen[c.name()] = true
		e, ok := tpsColumns[c.name()]
		if !ok {
			unclassified = append(unclassified, fmt.Sprintf("%s (%s)", c.name(), c.udt))
			continue
		}
		if e.class == tpsNotCtxScope {
			continue
		}
		col := "t." + pgx.Identifier{c.column}.Sanitize()
		var pred string
		switch c.udt {
		case "_text", "_varchar":
			pred = col + " && p.tps_scopes"
		case "uuid":
			pred = col + " = p.tps_tenant"
		case "text", "varchar", "bpchar":
			pred = col + "::text = ANY (p.tps_scopes)"
		default:
			untyped = append(untyped, fmt.Sprintf("%s (%s)", c.name(), c.udt))
			continue
		}
		checks = append(checks, tpsCheck{name: c.name(), class: e.class, sql: tpsCountSQL(c.table, pred), why: e.why})
	}
	var stale []string
	for name := range tpsColumns {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("classify the new column(s) in tpsColumns (drained / kept with reason / notCtxScope): %s", strings.Join(unclassified, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("tpsColumns names column(s) the schema no longer has: %s", strings.Join(stale, ", "))
	}
	if len(untyped) > 0 {
		t.Errorf("no predicate for the type of the counted column(s): %s", strings.Join(untyped, ", "))
	}
	for _, x := range tpsExtras {
		checks = append(checks, tpsCheck{name: x.name, class: x.class, sql: tpsCountSQL(x.table, x.pred), ids: x.ids, why: x.why})
	}
	return checks
}

func tpsCountSQL(table, pred string) string {
	return `WITH p AS (SELECT $1::text[] AS tps_scopes, $2::uuid AS tps_tenant, $3::text[] AS tps_ids)
		SELECT count(*)::int FROM ` + pgx.Identifier{table}.Sanitize() + ` t, p WHERE ` + pred
}

func tpsCount(t *testing.T, pool *pgxpool.Pool, c tpsCheck, o *tpsOwner) int {
	t.Helper()
	ids := o.ids[c.ids]
	if ids == nil {
		ids = []string{}
	}
	return tpsQ(t, pool, c.sql, o.scopes, o.tenant, ids)
}

// tpsBlock writes one block through UpsertBlock.
func tpsBlock(t *testing.T, pool *pgxpool.Pool, scope, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "learnings", title, title+" content", nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("block %s: %v", title, err)
	}
	return b.ID
}

// tpsGraph writes, per scope, six triangles of blocks with strong dream links
// inside and a weak chain between them (the shape the overview tests use),
// stores a synthetic embedding per block and runs the overview over the
// owner's scopes: the rebuild (nodes, members, edges, topics, topic edges,
// super level, meta), the centroids, and a run-journal row.
func tpsGraph(t *testing.T, f *p547Fixture, o *tpsOwner) {
	t.Helper()
	ctx := context.Background()
	const triangles = 6
	for si, scope := range o.scopes {
		ids := make([][3]string, triangles)
		for c := range triangles {
			for i := range 3 {
				id := tpsBlock(t, f.pool, scope, fmt.Sprintf("%s graph %d %d %d", o.slug, si, c, i))
				ids[c][i] = id
				vec := make([]float32, 1024)
				vec[(si*triangles+c)%1024] = 1
				vec[1023] = float32(i+1) / 10
				if err := store.StoreEmbedding(ctx, f.pool, id, "tps-embed", vec); err != nil {
					t.Fatalf("embedding: %v", err)
				}
			}
		}
		for c := range triangles {
			links := map[string][]dream.Link{
				ids[c][0]: {{TargetID: ids[c][1], Relationship: "topical", Confidence: 0.95}, {TargetID: ids[c][2], Relationship: "topical", Confidence: 0.95}},
				ids[c][1]: {{TargetID: ids[c][2], Relationship: "topical", Confidence: 0.95}},
			}
			if c+1 < triangles {
				links[ids[c][0]] = append(links[ids[c][0]], dream.Link{TargetID: ids[c+1][0], Relationship: "topical", Confidence: 0.3})
			}
			for src, ls := range links {
				n, err := dream.WriteLinks(ctx, f.pool, f.set, src, scope, 1.0, ls)
				if err != nil || n != len(ls) {
					t.Fatalf("dream links of %s: n=%d (want %d) err=%v", src, n, len(ls), err)
				}
			}
		}
	}
	opts := overview.Options{
		Resolution: 1.0, VisibleTypes: f.set.VisibleTypes(), OverviewTypes: f.set.OverviewTypes(),
		ScopeFilter: o.scopes, SuperEnabled: true, SuperTargetRows: 3, SuperMinResolution: 0.05,
	}
	st, err := overview.Rebuild(ctx, f.pool, opts)
	if err != nil || st.Skipped {
		t.Fatalf("overview rebuild of %v: stats=%+v err=%v", o.scopes, st, err)
	}
	if _, err := overview.BuildCentroids(ctx, f.pool, o.scopes, overview.CentroidOptions{Batch: 500, VisibleTypes: f.set.VisibleTypes()}); err != nil {
		t.Fatalf("centroids: %v", err)
	}
	if _, err := overview.StartRun(ctx, f.pool, overview.RunStart{ScopeSet: o.scopes, Resolution: 1.0}); err != nil {
		t.Fatalf("run journal: %v", err)
	}
}

// tpsFill writes a row of the owner into every table, through the store's
// writers; the comments name the two places that insert by SQL and why.
func tpsFill(t *testing.T, f *p547Fixture, o *tpsOwner, defaultBlock string) {
	t.Helper()
	ctx := context.Background()
	pool := f.pool
	s0, s1 := o.scopes[0], o.scopes[1]
	o.ids = map[string][]string{}

	key, _, err := store.CreateApiKey(ctx, pool, o.slug+" key", s0, o.scopes, o.tenant)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	o.keyID = key.ID

	// Blocks, links, blobs, sources and chunks.
	b1 := tpsBlock(t, pool, s0, o.slug+" one")
	b2 := tpsBlock(t, pool, s0, o.slug+" two")
	tpsBlock(t, pool, s1, o.slug+" three")
	tpsGraph(t, f, o)
	tpsTx(t, pool, "structural link", func(tx pgx.Tx) error {
		return store.PutStructuralLink(ctx, tx, store.StructuralLink{SourceID: b1, TargetID: b2, LinkClass: "references", Origin: "manual"}, []string{s0})
	})
	if _, err := store.UpsertBlob(ctx, pool, "learnings", o.slug+" blob", "b.txt", "text/plain", s0, []byte("blob of "+o.slug), nil, nil, b1); err != nil {
		t.Fatalf("blob: %v", err)
	}
	// context_sources has no writer in the binary (no INSERT INTO
	// context_sources outside tests; InsertChunk writes the chunks of an
	// existing source), so the source row is inserted here; its chunk goes
	// through InsertChunk.
	var sourceID string
	if err := pool.QueryRow(ctx, `INSERT INTO context_sources (file_path, file_hash, scope) VALUES ($1, 'h', $2) RETURNING id::text`,
		"/tps/"+o.slug, s0).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	if _, err := store.InsertChunk(ctx, pool, sourceID, 0, "learnings", o.slug+" chunk", "chunk content", nil, nil, s0); err != nil {
		t.Fatalf("chunk: %v", err)
	}

	// Per-scope configuration.
	secret := o.slug + "-secret"
	tpsTx(t, pool, "settings and secret", func(tx pgx.Tx) error {
		if err := store.UpsertSetting(ctx, tx, "search.leave_out_never_ranked", s0, json.RawMessage(`true`), nil); err != nil {
			return err
		}
		_, err := store.PutSecret(ctx, tx, secret, s0, make([]byte, 12), []byte("sealed "+o.slug), 1, nil)
		return err
	})
	var backendID string
	tpsTx(t, pool, "backend and disable profile", func(tx pgx.Tx) error {
		var err error
		backendID, err = store.CreateBackend(ctx, tx, &backends.Backend{
			Name: o.slug + "-be", Host: "http://127.0.0.1:11434", Protocol: backends.ProtocolOllama,
			ProviderClass: backends.ProviderGeneric, Trust: backends.TrustFull, Locality: "local",
			Roles: []string{backends.RoleEmbed}, ModelMap: map[string]backends.ModelSpec{backends.RoleEmbed: {Model: "tps"}},
			Enabled: true, Scope: s0, APIKeyRef: secret,
		}, nil)
		if err != nil {
			return err
		}
		pid, err := store.CreateDisableProfile(ctx, tx, &store.DisableProfile{Scope: s0, Name: o.profile, Label: o.profile}, []string{backendID}, nil)
		o.ids["profiles"] = append(o.ids["profiles"], pid)
		return err
	})
	var aliasID string
	tpsTx(t, pool, "model alias", func(tx pgx.Tx) error {
		a, err := store.CreateModelAlias(ctx, tx, store.ModelAlias{Scope: s0, Name: o.alias, Kind: "chat", Role: "chat", Sensitivity: "credentials", Enabled: true}, nil, store.ModelAliasWrite{})
		aliasID = a.ID
		return err
	})
	consumer, _, _, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{
		TenantID: o.tenant, Label: o.slug + " consumer", TenantScope: &s0,
	}, nil, store.AuditStamp{})
	if err != nil {
		t.Fatalf("model consumer: %v", err)
	}
	o.ids["consumers"] = []string{consumer.ID}
	tpsTx(t, pool, "model grant", func(tx pgx.Tx) error {
		return store.SetConsumerGrants(ctx, tx, consumer.ID, []string{aliasID}, store.AuditStamp{})
	})
	rpm := int64(5)
	if _, err := store.UpsertGatewayLimit(ctx, pool, store.ModelAccess{TenantID: o.tenant},
		store.GatewayLimitKey{Kind: gatewaylimits.SubjectTenant, TenantID: o.tenant, Tier: gatewaylimits.TierTenant, Op: keyquota.OpChat},
		gatewaylimits.Values{RPM: &rpm}, store.AuditStamp{}); err != nil {
		t.Fatalf("gateway limit: %v", err)
	}
	tpsTx(t, pool, "hue", func(tx pgx.Tx) error {
		return store.UpsertCategoryHue(ctx, tx, s0, o.hueCategory, 120, nil)
	})
	// An excluded overlay: the registry trigger queues (name, t:<tenant>).
	if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: o.blockType, Scope: s0, DisplayName: o.blockType,
		Config: json.RawMessage(`{"retrieval":{"policy":"excluded"}}`)}, nil, "tps"); err != nil {
		t.Fatalf("block type: %v", err)
	}
	calls := 10
	if err := store.UpsertTenantQuota(ctx, pool, s0, backends.TenantQuota{DailyCalls: &calls, OnExceed: backends.QuotaExceedBlock, Enabled: true}, store.AuditStamp{}); err != nil {
		t.Fatalf("quota: %v", err)
	}

	// Project and its children; CreateProject assigns the project its own,
	// third scope of the owner.
	project, _, err := store.CreateProject(ctx, pool, store.CreateProjectParams{TenantID: o.tenant, ScopeName: o.slug + "-p", Identity: "github:tps/" + o.slug})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	o.scopes = append(o.scopes, project.Scope)
	o.ids["projects"] = []string{project.ID}
	if _, err := store.StartSyncRun(ctx, pool, project.ID); err != nil {
		t.Fatalf("sync run: %v", err)
	}
	tpsTx(t, pool, "sync map", func(tx pgx.Tx) error {
		return store.InsertSyncMap(ctx, tx, store.SyncMap{ProjectID: project.ID, EntityKind: "issue", ForgeID: 1, BlockID: b1, BaseHash: "h"})
	})
	if _, err := store.InsertWebhookEvent(ctx, pool, project.ID, "tps-"+o.slug, "issues", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("webhook event: %v", err)
	}

	// Chat, stage, recall run.
	session, err := store.CreateSession(ctx, pool, s0, o.scopes, o.keyID, "tps chat")
	if err != nil {
		t.Fatalf("chat session: %v", err)
	}
	o.session = session.ID
	o.ids["sessions"] = []string{session.ID}
	if _, _, err := store.AppendMessage(ctx, pool, session.ID, store.NewMessage{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("chat message: %v", err)
	}
	if _, err := store.StagePendingWrite(ctx, pool, o.keyID, s0, "store", "mcp", json.RawMessage(`{"op":"store"}`), "tps-own-"+o.slug, 10*time.Minute); err != nil {
		t.Fatalf("stage: %v", err)
	}
	var group string
	if err := pool.QueryRow(ctx, `SELECT uuidv7()::text`).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if err := recall.Insert(ctx, pool, recall.Run{RunGroup: group, Stratum: "small", Scope: &s0, K: 10, NQueries: 1,
		QuerySource: "log", EfSearch: 40, IterativeScan: "off", Valid: false, Meta: map[string]any{}}); err != nil {
		t.Fatalf("recall run: %v", err)
	}
	// The access-log row of a search carries the hit's scope in its metadata;
	// its only writer is QueryHandler.logAccess (handler/query.go), after a
	// search, so the row is inserted here with that statement's shape.
	if _, err := pool.Exec(ctx, `INSERT INTO context_access_log (api_key_id, block_id, action, query_text, metadata, principal_id)
		VALUES ($1::uuid, $2::uuid, 'query', 'tps', jsonb_build_object('score', 0.5, 'scope', $3::text, 'source', 'agent'),
		        (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $1::uuid))`, o.keyID, b1, s0); err != nil {
		t.Fatalf("access log: %v", err)
	}

	// Rows of the default tenant that point at the owner: a key homed in its
	// first scope, a key reading and writing its second, a chat session
	// reading its first, a stage of that key in its second, grants both ways.
	_, plainHome, err := store.CreateApiKey(ctx, pool, o.slug+" foreign home", s0, nil, "")
	if err != nil {
		t.Fatalf("foreign home key: %v", err)
	}
	o.foreignHomeKey = plainHome
	dw, plainWrite, err := store.MintKeyWithQuota(ctx, pool, o.slug+" foreign write", "private", []string{s1}, []string{s1}, store.DefaultTenantID, "member", nil, store.AuditStamp{})
	if err != nil {
		t.Fatalf("foreign write key: %v", err)
	}
	o.foreignWriteKey = plainWrite
	if _, err := store.CreateSession(ctx, pool, "private", []string{"private", s0}, dw.ID, "tps foreign chat"); err != nil {
		t.Fatalf("foreign chat session: %v", err)
	}
	if _, err := store.StagePendingWrite(ctx, pool, dw.ID, s1, "store", "mcp", json.RawMessage(`{"op":"store"}`), "tps-foreign-"+o.slug, 10*time.Minute); err != nil {
		t.Fatalf("foreign stage: %v", err)
	}
	if _, err := store.CreateTenantGrant(ctx, pool, o.tenant, "shared", ""); err != nil {
		t.Fatalf("tenant grant to the owner: %v", err)
	}
	if _, err := store.CreateTenantGrant(ctx, pool, store.DefaultTenantID, s0, ""); err != nil {
		t.Fatalf("tenant grant on the owner's scope: %v", err)
	}
	if _, err := store.CreateBlockGrant(ctx, pool, b1, store.DefaultTenantID, ""); err != nil {
		t.Fatalf("block grant on the owner's block: %v", err)
	}
	o.ids["grantedBlocks"] = []string{b1}
	if _, err := store.CreateBlockGrant(ctx, pool, defaultBlock, o.tenant, ""); err != nil {
		t.Fatalf("block grant to the owner: %v", err)
	}
}

func TestTenantPruneLeavesNoRowOfTheTenant(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	start := time.Now()

	checks := tpsChecks(t, tpsSchemaColumns(t, f.pool))
	if t.Failed() {
		t.FailNow()
	}

	owner := func(label, slug string) *tpsOwner {
		o := &tpsOwner{label: label, slug: slug, scopes: []string{slug + "-a", slug + "-b"},
			hueCategory: slug + "-cat", profile: slug + "-dp", alias: slug + "-chat", blockType: slug + "-x"}
		o.tenant = f.tenant(t, slug, o.scopes[0], o.scopes[1])
		return o
	}
	tT := owner("T", "tps-t")
	tC := owner("C", "tps-c")
	defaultBlock := tpsBlock(t, f.pool, "private", "tps default block")
	tpsFill(t, f, tT, defaultBlock)
	tpsFill(t, f, tC, defaultBlock)
	// The counters: fold the deltas into context_block_counts (the
	// scheduler's fold), then one more block per owner leaves a delta row.
	if _, err := store.FoldBlockCountDeltas(ctx, f.pool); err != nil {
		t.Fatalf("fold block counts: %v", err)
	}
	for _, o := range []*tpsOwner{tT, tC} {
		tpsBlock(t, f.pool, o.scopes[1], o.slug+" after fold")
	}
	filled := time.Since(start)

	// Before: every counted check carries a row of T and of C.
	before := map[string]int{}
	var missing []string
	for _, c := range checks {
		for _, o := range []*tpsOwner{tT, tC} {
			n := tpsCount(t, f.pool, c, o)
			if o == tC {
				before[c.name] = n
			}
			if n == 0 {
				missing = append(missing, fmt.Sprintf("%s (%s, %s)", c.name, c.class, o.label))
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the fill wrote no row of the owner into: %s", strings.Join(missing, ", "))
	}

	pruneStart := time.Now()
	if err := store.PruneTenant(ctx, f.pool, tT.tenant, store.AuditStamp{}); err != nil {
		t.Fatalf("PruneTenant: %v", err)
	}
	pruned := time.Since(pruneStart)

	// After: no drained check carries a row of T; no check lost a row of C.
	var left, lost []string
	for _, c := range checks {
		n := tpsCount(t, f.pool, c, tT)
		switch {
		case c.class == tpsDrained && n > 0:
			left = append(left, fmt.Sprintf("%s = %d", c.name, n))
		case c.class == tpsKept:
			t.Logf("kept %s: %d row(s) of the pruned tenant (%s)", c.name, n, c.why)
		}
		if nc := tpsCount(t, f.pool, c, tC); nc < before[c.name] {
			lost = append(lost, fmt.Sprintf("%s: %d -> %d", c.name, before[c.name], nc))
		}
	}

	// The next owner: a tenant with T's slug, assigned T's scope names.
	reads := tpsNextOwner(t, f, tT)
	t.Logf("timing: fill %s, prune %s, total %s", filled.Round(time.Millisecond), pruned.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	var msg []string
	if len(left) > 0 {
		msg = append(msg, fmt.Sprintf("%d drained check(s) still carry rows of the pruned tenant:\n\t%s", len(left), strings.Join(left, "\n\t")))
	}
	if len(lost) > 0 {
		msg = append(msg, fmt.Sprintf("the prune deleted rows of the control tenant:\n\t%s", strings.Join(lost, "\n\t")))
	}
	if len(reads) > 0 {
		msg = append(msg, fmt.Sprintf("the next owner of the slug and scope names reads the pruned tenant's rows:\n\t%s", strings.Join(reads, "\n\t")))
	}
	if len(msg) > 0 {
		t.Errorf("%s", strings.Join(msg, "\n"))
	}
}

// tpsNextOwner creates a tenant with the pruned tenant's slug, assigns it the
// pruned tenant's scope names and runs the readers of what the fill wrote;
// it returns what they still show of the pruned tenant.
func tpsNextOwner(t *testing.T, f *p547Fixture, o *tpsOwner) []string {
	t.Helper()
	ctx := context.Background()
	pool := f.pool
	var out []string
	if n := tpsQ(t, pool, `SELECT count(*)::int FROM context_tenants WHERE slug = $1`, o.slug); n > 0 {
		return []string{"the pruned tenant's slug is still taken"}
	}
	next, err := store.CreateTenant(ctx, pool, o.slug, o.slug)
	if err != nil {
		return []string{fmt.Sprintf("CreateTenant(%s): %v", o.slug, err)}
	}
	for _, s := range o.scopes {
		if _, err := store.AssignTenantScope(ctx, pool, next.ID, s, nil); err != nil {
			return append(out, fmt.Sprintf("AssignTenantScope(%s): %v", s, err))
		}
	}
	s0 := o.scopes[0]
	if rows, err := store.LoadSettingOverrides(ctx, pool, s0); err != nil || len(rows) > 0 {
		out = append(out, fmt.Sprintf("LoadSettingOverrides(%s): %d row(s), err=%v", s0, len(rows), err))
	}
	if refs, err := store.BackendSecretRefsInScopes(ctx, pool, o.scopes); err != nil || len(refs) > 0 {
		out = append(out, fmt.Sprintf("BackendSecretRefsInScopes: %d backend(s), err=%v", len(refs), err))
	}
	if hues, err := store.LoadCategoryHues(ctx, pool, o.scopes); err != nil {
		out = append(out, fmt.Sprintf("LoadCategoryHues: %v", err))
	} else if _, ok := hues[o.hueCategory]; ok {
		out = append(out, fmt.Sprintf("LoadCategoryHues: hue of %q", o.hueCategory))
	}
	if _, found, err := store.GetTenantQuota(ctx, pool, s0); err != nil || found {
		out = append(out, fmt.Sprintf("GetTenantQuota(%s): found=%v err=%v", s0, found, err))
	}
	if p, err := store.GetDisableProfile(ctx, pool, s0, o.profile); err != nil || p != nil {
		out = append(out, fmt.Sprintf("GetDisableProfile(%s, %s): %v err=%v", s0, o.profile, p != nil, err))
	}
	if aliases, err := store.ListModelAliases(ctx, pool, o.scopes); err != nil {
		out = append(out, fmt.Sprintf("ListModelAliases: %v", err))
	} else {
		for _, a := range aliases {
			if slices.Contains(o.scopes, a.Scope) {
				out = append(out, fmt.Sprintf("ListModelAliases: alias %q in %s", a.Name, a.Scope))
			}
		}
	}
	if types, err := store.ListBlockTypes(ctx, pool, o.scopes); err != nil {
		out = append(out, fmt.Sprintf("ListBlockTypes: %v", err))
	} else {
		for _, bt := range types {
			if slices.Contains(o.scopes, bt.Scope) {
				out = append(out, fmt.Sprintf("ListBlockTypes: type %q in %s", bt.Name, bt.Scope))
			}
		}
	}
	if sessions, err := store.ListSessions(ctx, pool, s0, 10); err != nil || len(sessions) > 0 {
		out = append(out, fmt.Sprintf("ListSessions(%s): %d session(s), err=%v", s0, len(sessions), err))
	}
	if grants, err := store.ListTenantGrants(ctx, pool, store.DefaultTenantID); err != nil {
		out = append(out, fmt.Sprintf("ListTenantGrants: %v", err))
	} else {
		for _, g := range grants {
			if slices.Contains(o.scopes, g.GrantedScope) {
				out = append(out, fmt.Sprintf("ListTenantGrants: the default tenant holds a grant on %s", g.GrantedScope))
			}
		}
	}
	for _, k := range []struct{ label, plain string }{{"foreign home key", o.foreignHomeKey}, {"foreign write key", o.foreignWriteKey}} {
		ar, err := auth.Authenticate(ctx, pool, k.plain)
		if err != nil {
			out = append(out, fmt.Sprintf("Authenticate(%s): %v", k.label, err))
			continue
		}
		if !ar.IsValid {
			continue
		}
		for _, s := range o.scopes {
			if ar.HomeScope == s || slices.Contains(ar.ReadScopes, s) || slices.Contains(ar.WriteScopes, s) {
				out = append(out, fmt.Sprintf("Authenticate(%s): the default tenant's key reaches %s (home %s, read %v, write %v)",
					k.label, s, ar.HomeScope, ar.ReadScopes, ar.WriteScopes))
			}
		}
	}
	return out
}
