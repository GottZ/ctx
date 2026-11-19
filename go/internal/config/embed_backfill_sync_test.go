package config

import (
	"strings"
	"testing"
)

// TestEmbedBackfillSyncDisabledKey pins the registry entry of issue #221's
// off switch: default false (the step runs, today's behavior, also for a raw
// &Config{}), hot, settings-only, and the one tenant-overridable key of the
// group, so a tenant can switch its own queries' step off alone.
func TestEmbedBackfillSyncDisabledKey(t *testing.T) {
	const key = "embed_backfill.sync_disabled"
	k, ok := KeyByName(key)
	if !ok {
		t.Fatalf("%s is not a registry key", key)
	}
	if k.Type != "bool" || k.Default != false || k.Mutability != "hot" || k.Tenancy != "tenant-overridable" || k.EnvVar != "" {
		t.Errorf("%s: type %q default %v mut %q tenancy %q env %q; want bool, false, hot, tenant-overridable, no env",
			key, k.Type, k.Default, k.Mutability, k.Tenancy, k.EnvVar)
	}
	if Defaults().EmbedBackfill.SyncDisabled {
		t.Error("the default switches the query path's backfill step off")
	}
	// The two keys of the step that stay global-only (their 0 is unbounded).
	for _, other := range []string{"embed_backfill.sync_cap", "embed_backfill.sync_budget"} {
		if o, _ := KeyByName(other); o.Tenancy != "global-only" {
			t.Errorf("%s tenancy %q, want global-only", other, o.Tenancy)
		}
	}
}

// TestValidateEmbedBackfillSyncV61 is V61: the step off is refused only
// where neither the background arm nor Dream embeds pending blocks.
func TestValidateEmbedBackfillSyncV61(t *testing.T) {
	const key = "embed_backfill.sync_disabled"
	for _, tc := range []struct {
		name string
		vals map[string]string
		errs int
	}{
		{"off_with_the_arm", map[string]string{key: "true"}, 0},
		{"off_with_dream", map[string]string{key: "true", "embed_backfill.interval": "0", "dream.enabled": "true"}, 0},
		{"off_with_no_other_embedder", map[string]string{key: "true", "embed_backfill.interval": "0"}, 1},
		{"on_with_no_other_embedder", map[string]string{key: "false", "embed_backfill.interval": "0"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, issues := cfgFrom(t, tc.vals)
			issues = append(issues, Validate(c)...)
			var errs []Issue
			for _, is := range issues {
				if is.Severity == SeverityError {
					errs = append(errs, is)
				}
			}
			if len(errs) != tc.errs {
				t.Fatalf("errors %+v, want %d", errs, tc.errs)
			}
			if tc.errs == 1 && (errs[0].Field != key || !strings.Contains(errs[0].Msg, "only one")) {
				t.Errorf("error %+v, want one on %s naming the query path as the only embedder", errs[0], key)
			}
		})
	}
}
