package recall

import "testing"

// TestValidateMeta pins the allowlist as the fail-closed leak guard: only
// the 12 canonical keys pass, and only scalar values under the length cap.
// This is the unit-level half of the guard — the integration test
// (persist_integration_test.go) additionally proves a rejected key never
// reaches the DB.
func TestValidateMeta(t *testing.T) {
	t.Run("allowed_key_passes", func(t *testing.T) {
		if err := validateMeta(map[string]any{
			"pgvector_version": "0.8.0",
			"epsilon":          0.01,
			"budget_exhausted": false,
		}); err != nil {
			t.Errorf("allowed keys should pass validation, got: %v", err)
		}
	})

	t.Run("empty_meta_passes", func(t *testing.T) {
		if err := validateMeta(map[string]any{}); err != nil {
			t.Errorf("empty meta should pass validation, got: %v", err)
		}
		if err := validateMeta(nil); err != nil {
			t.Errorf("nil meta should pass validation, got: %v", err)
		}
	})

	t.Run("unknown_key_rejected", func(t *testing.T) {
		err := validateMeta(map[string]any{"sample_texts": "some query text"})
		if err == nil {
			t.Fatal("sample_texts is not in the allowlist — expected an error")
		}
	})

	t.Run("block_ids_key_rejected", func(t *testing.T) {
		err := validateMeta(map[string]any{"block_ids": []string{"019f-abc"}})
		if err == nil {
			t.Fatal("block_ids is not in the allowlist — expected an error")
		}
	})

	t.Run("array_value_rejected", func(t *testing.T) {
		err := validateMeta(map[string]any{"strata_bounds": []int{100, 10000}})
		if err == nil {
			t.Fatal("array values are rejected even under an allowed key — expected an error")
		}
	})

	t.Run("map_value_rejected", func(t *testing.T) {
		err := validateMeta(map[string]any{"invalid_reason": map[string]any{"nested": true}})
		if err == nil {
			t.Fatal("map/container values are rejected — expected an error")
		}
	})

	t.Run("nil_value_rejected", func(t *testing.T) {
		err := validateMeta(map[string]any{"embed_model": nil})
		if err == nil {
			t.Fatal("nil values are rejected (not a scalar) — expected an error")
		}
	})

	t.Run("oversized_string_rejected", func(t *testing.T) {
		long := make([]byte, maxMetaStringLen+1)
		for i := range long {
			long[i] = 'x'
		}
		err := validateMeta(map[string]any{"invalid_reason": string(long)})
		if err == nil {
			t.Fatalf("string longer than %d chars should be rejected", maxMetaStringLen)
		}
	})

	t.Run("string_at_cap_passes", func(t *testing.T) {
		exact := make([]byte, maxMetaStringLen)
		for i := range exact {
			exact[i] = 'x'
		}
		if err := validateMeta(map[string]any{"invalid_reason": string(exact)}); err != nil {
			t.Errorf("string at exactly %d chars should pass, got: %v", maxMetaStringLen, err)
		}
	})
}
