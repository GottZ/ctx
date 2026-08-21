// Code generated for Stufe-2-Welle beta15 (migration fold). DO NOT EDIT by
// hand: the values below are the identities of migration files that no longer
// exist in the tree, and nothing can re-derive them.

package migrations

import (
	"bytes"
	"fmt"
)

// BaselineVersion is the top of the fold: 113_baseline.sql carries the effect
// of every migration up to and including this version. The runner keys the
// fresh-install/upgrade decision on it — a database that already has version
// 113 skips the baseline exactly the way it skips any applied migration.
const BaselineVersion = 113

// BaselineFile is the baseline's own filename in the embedded FS.
const BaselineFile = "113_baseline.sql"

// FoldedMigration is one migration whose SQL was consolidated into the
// baseline. Filename and Checksum are the identity the file had before the
// fold — they are what an upgraded database carries in _migrations and what
// the baseline's ledger writes into a fresh one, so the schema contract can
// keep checking migration integrity at full strength for these versions.
type FoldedMigration struct {
	Version  int
	Filename string
	// Checksum is sha256(hex) of the file as it existed before the fold.
	Checksum string
	// Normalized marks the two files whose standalone BEGIN;/COMMIT; lines
	// were dropped when they moved into the baseline (the runner supplies the
	// transaction). For these, Section() returns the file minus those lines,
	// so sha256(Section(f)) is SectionChecksum, not Checksum.
	Normalized      bool
	SectionChecksum string
}

// folded lists every folded migration in version order.
var folded = []FoldedMigration{
	{Version: 1, Filename: "001_initial.sql", Checksum: "86fb294a06007cf6be77e0622b4149df19b4e9802e5d844977b1192c7982125c"},
	{Version: 2, Filename: "002_scale_columns.sql", Checksum: "ea4ddfa9aad2bb17bbc7ba54d9c8d37a97b06adf2fdc84fe0a2fb2c8c984facb"},
	{Version: 3, Filename: "003_pg_functions.sql", Checksum: "5174354d020a5186edee2522013cd0fc4fef73fb6a0232c1048c4511ae9cd14f"},
	{Version: 4, Filename: "004_notify_triggers.sql", Checksum: "94552f413b5dbc116d72f400d0416aa25cbc1d9371e90d485077ef842393cac7"},
	{Version: 5, Filename: "005_scope_unique.sql", Checksum: "ba5aa82feb3870b93eb014bba8d7dd8ae87d1470f1ec3cf166848bbb807f095c"},
	{Version: 6, Filename: "006_temporal_rrf.sql", Checksum: "7077e4aac9b07ec3ad63e36fa342072ccdea1e72e3e46e6c5ebcd5ad73554a7e"},
	{Version: 7, Filename: "007_temporal_gravity.sql", Checksum: "71455ad9417f574008e4dd19a644da8c2e0ad36efdf371eaa43a8d369423c2d0"},
	{Version: 8, Filename: "008_gin_range_prefilter.sql", Checksum: "9b06724d2a5f9a1a26738ee3eea0e25f6d8ee104d3ae596404472d4483ce9b1a"},
	{Version: 9, Filename: "009_temporal_dimension.sql", Checksum: "e61c7e6a8b9103d59d4ba076f02ffaafeaacd2b85419a5b55bdfb15c8d52936c"},
	{Version: 10, Filename: "010_content_dates_drop_generated.sql", Checksum: "00aa71a29d94ef4ec5ba9520a4a27d317036d07303dac53b88329251ba37c361"},
	{Version: 11, Filename: "011_guard_chunk_filter.sql", Checksum: "7102b466bf03640ae4e1b65911f2b7f56518ae0ac1a53a87bfee541591f19233"},
	{Version: 12, Filename: "012_ingestion_sources.sql", Checksum: "5061afea81713a3fc9fad73dfc1720b62d57a5d8a5e6655ee88de815059369cf"},
	{Version: 13, Filename: "013_link_dimension.sql", Checksum: "7e3665edad9af8223cb7efc7a15d167bc299d2666640644e88a32c1038df491f"},
	{Version: 14, Filename: "014_security_hardening.sql", Checksum: "ba3f06c01fc16cf343fbd7b269f945ab7449ed9e48ffa84b725453acbb5add9e"},
	{Version: 15, Filename: "015_blob_scope_unique.sql", Checksum: "cce6b75ccbf7647948d7f8791c5157763806b2a021e268712d9d1202d4e6a642"},
	{Version: 16, Filename: "016_dream.sql", Checksum: "0220bfcf644d2ea75f6304a69a67c24830ed80e653bc0fbda691401c43d03078"},
	{Version: 17, Filename: "017_dream_indexes.sql", Checksum: "291a09664ed6684f107cd271688c18c2f8ab1e61b5627c4551288e560ddde170"},
	{Version: 18, Filename: "018_fts_or_hybrid.sql", Checksum: "95833a0e5a86c87e76df95c1d6c20a40828a159215dc5c0e55fa5f06d1cca176"},
	{Version: 19, Filename: "019_monthday_seasonal.sql", Checksum: "f5b8335f940db47613530dde76ba2d0ed86c8f79515af9f6fe4821f855690083"},
	{Version: 20, Filename: "020_content_times.sql", Checksum: "1db2768ef7ed87c6ae3b0ab1d627c8e44aedf92a0844dd81ad982cfaed88c453"},
	{Version: 21, Filename: "021_created_at_anchors.sql", Checksum: "38cf11371040a28458a600b2bd64b2c38e56247d6dd649f26d803bdd2aff111c"},
	{Version: 22, Filename: "022_stats_indexes.sql", Checksum: "3ca01871ac506b081f160eaf19984330f7c7e27eceb59435328a1563e603d168"},
	{Version: 23, Filename: "023_oauth_clients.sql", Checksum: "cc476f091ebfe9ba255489deb8e0bd3645d1be26c1730d83bca669bafc28735c"},
	{Version: 24, Filename: "024_dream_links_raw_confidence.sql", Checksum: "8d9627363f754ff5423f52ad3f9c1e2ab34636f860e7a6d43eb7bb9729617a50"},
	{Version: 25, Filename: "025_llm_log.sql", Checksum: "133b6889fa46fd016b77bf4f34b3aeeaf8c7321bc17e6dc918e1e30ee2381b7e"},
	{Version: 26, Filename: "026_embed_cache.sql", Checksum: "d1844aadbbccf9ff0a806f62a7791f4ebc1f68a5224f744a00143f381e3eebe9"},
	{Version: 27, Filename: "027_block_dream_keywords.sql", Checksum: "34dd8d3183af72a7429050aa642bb44b114c58b0518c9f7be64d59a97d22d1b7"},
	{Version: 28, Filename: "028_block_temporal_validated.sql", Checksum: "fca9651715d746874bcbf88757d20f423690c29be363aacaadba70008420dae7"},
	{Version: 29, Filename: "029_block_is_meta.sql", Checksum: "fb82002fa1de5fbdab65d63062b4c8fa0f5950264c6ff42814c6f8960cc21cc6"},
	{Version: 30, Filename: "030_rrf_mass_factor.sql", Checksum: "d5efb8bd2d4b043cb0a7e1236779105ca80d69f1ccf4fe105cdab9e7d3f1e387"},
	{Version: 31, Filename: "031_dream_links_recurrent.sql", Checksum: "a1b7c9ad04ad68f2e5dce898747cd5542c3100a816c1b8c00a0dd47065c180aa"},
	{Version: 32, Filename: "032_audit_blocks_is_meta.sql", Checksum: "26abad055261609e57dd02ae96424ccf354489d26809ebf998a231e71af1f012"},
	{Version: 33, Filename: "033_rrf_is_meta_filter.sql", Checksum: "0ffa34bfc50767d8655b7d45a6b94fc51114c6b260886dc7a5507613996fcea8"},
	{Version: 34, Filename: "034_audit_blocks_unmeta.sql", Checksum: "b115f295aae277786303e6a967d0bfccf65a7abaa22a4b315ef987510f6f0738"},
	{Version: 35, Filename: "035_block_role_classification.sql", Checksum: "824f8ba54eb0c5b003e942bfbbf3c453ae5a5a45616a6671f131af4cbf6ddc78"},
	{Version: 36, Filename: "036_rrf_block_role_filter.sql", Checksum: "41dde03b28bdfc24aad5cdd6ce5a14c74597edf965332bfb9a17f1a9857ce140"},
	{Version: 37, Filename: "037_rrf_role_damping_tuning.sql", Checksum: "90d2941daa7b81878c9ecf42d842040ba88a436dbe9c698eee1057e949770e6f"},
	{Version: 38, Filename: "038_rrf_role_damping_revert.sql", Checksum: "e2f0305b20fa28e7a6f812ebb8480e820b8a9fce0ca0b1a4db393f9dee18d438"},
	{Version: 39, Filename: "039_rrf_query_aware_damping.sql", Checksum: "d79280fdd0217a46d2b31c9d01781805989fe7cef6a6cabfe7d5b0c06d925e1b"},
	{Version: 40, Filename: "040_audit_trail_classification_extension.sql", Checksum: "c36c4ebbe909e59d5258e7d9bdb1d6ccbfc7f39ba07741cb6023efcddb375209"},
	{Version: 41, Filename: "041_dream_links_cleanup_dangling.sql", Checksum: "cf792d5abe1cc44dfe8fc619851dfd986e4270d18de4b56a819285eb51500c9b"},
	{Version: 42, Filename: "042_dream_full_reset.sql", Checksum: "d8f834714ff2c69e394f4f3980d859a4d25e0417dd20f3412a498b5128d5f7be"},
	{Version: 43, Filename: "043_supersedes_direction_swap.sql", Checksum: "093866acc63f0acb198b54172a25fd6a2cad201302e6a9cfcd9fb0aba97ea8f9", Normalized: true, SectionChecksum: "c298ef0afd0ff7fc8df59e477fbcf2dfe12dc15ee1d8571982e9109aafd8ca01"},
	{Version: 44, Filename: "044_title_text.sql", Checksum: "ee1ca87bc93e274b913dcd0a40d91680a996f6ef2bc6a76aa04a633688f773ed", Normalized: true, SectionChecksum: "d2a7352159a300894259b02f730b4ac87c62f6beed636cd277cc0a352f34a981"},
	{Version: 45, Filename: "045_drop_chk_scope.sql", Checksum: "ac028610895587ce2d80a0dd85ce94d010fc640bdb5d4f7a374b343993b852c6"},
	{Version: 46, Filename: "046_write_log_block_title_text.sql", Checksum: "e72114b5c81dfb7fe96d486d7b7cb44df7bcc2dadac256863e5de7855be99cdc"},
	{Version: 47, Filename: "047_scope_varchar50.sql", Checksum: "5189fa68845db09735ece4e2ceb728f0f583ce3594be48ac2be57e791f832a3a"},
	{Version: 48, Filename: "048_rrf_exclude_params.sql", Checksum: "831a02ec38b694aa3a55c40e668b2d6613a6ad175577f8916d6fe79a834a6583"},
	{Version: 49, Filename: "049_dream_backoff.sql", Checksum: "8ace0ba802128f83bc8fbffa3a35868c6e7586d8db99ecc2ff50b51bdc2e4937"},
	{Version: 50, Filename: "050_dream_links_graph_traversal.sql", Checksum: "bba524147c9c7a0cdfe34810505ed7d746c6dd3cfcf8560ab8159c53bbff6352"},
	{Version: 51, Filename: "051_settings_store.sql", Checksum: "e86db07d25c8ca577dfb65ebfe15da7fe44743679745900041c73efff385fa00"},
	{Version: 52, Filename: "052_api_key_admin.sql", Checksum: "53ce058cdea48d8bb0ea12706c7947fb688a61136beb35a8318afb45e1d94884"},
	{Version: 53, Filename: "053_backend_pool.sql", Checksum: "58989fd2bfcae937c389d2718f0620e9998dd7b47af445d94f6f6fd2c5cda01d"},
	{Version: 54, Filename: "054_llmlog_backend_telemetry.sql", Checksum: "bf71364f6647fe590efc27087d6116196f7183dae3011dc797cfbe283d230571"},
	{Version: 55, Filename: "055_block_sensitivity.sql", Checksum: "b9269e779f05865d7dfd002cc31559a6909ad62f1355a9eb476e9e49d5f19c9e"},
	{Version: 56, Filename: "056_chat_sessions.sql", Checksum: "a9e25c949f71505f629ad0aea71674dca52c79ec9c182a9d36f832464e835f22"},
	{Version: 57, Filename: "057_graph_overview.sql", Checksum: "d6f3977bf1596a1c3e94c6672d10ab18fb7387816f47c9ce62dc53f0319806d4"},
	{Version: 58, Filename: "058_scope_generalize.sql", Checksum: "33482320b3ab4b773e3f4e2c6d843d75ea0fe60db1e8b814baed215de3c66ab7"},
	{Version: 59, Filename: "059_tenants_hybrid.sql", Checksum: "31312634e6d3e4c954b07f0d689cfc3a8833308cb3f56cc96a9a48795b055071"},
	{Version: 60, Filename: "060_ctx_auth_tenant.sql", Checksum: "10ed5219ae49b143aa43ac31eef4c26df3eaf41036fc0e5ab672306286f93436"},
	{Version: 61, Filename: "061_tenant_grants.sql", Checksum: "cab06b2e7ca18c93e2ea0c3e5b0d68d4312da07481b6261ebadf20c634a2032e"},
	{Version: 62, Filename: "062_backend_pool_tenant.sql", Checksum: "fd898b2d0eb89e52fe2344cf0568f4d9c259b8da9d9432a6b00b9a34288f2685"},
	{Version: 63, Filename: "063_tenant_quota.sql", Checksum: "d3f0fe7c57296063d8d1c75ad7ac89b2439bf6bd0f1ca80aa6fe8ca85d7a35f3"},
	{Version: 64, Filename: "064_settings_tenant_index.sql", Checksum: "5d0fd32cb14e11fb75c8e1a2a91bae81162b1d3c18fc0d239a2a8803ff87154b"},
	{Version: 65, Filename: "065_settings_notify_scope.sql", Checksum: "ccb79a5c3f5063589e454ad9578340289a03bbdad7886ad6eb542beb01b6d387"},
	{Version: 67, Filename: "067_block_grants.sql", Checksum: "0452b0ad91a35d3e63b7c28bc63ff914eab53aad14df5c0781d81f26ee157b43"},
	{Version: 68, Filename: "068_rrf_block_grants.sql", Checksum: "1a2bf30fb51dd35194fae82a5462a65b842f0822dd4c9ec02862293136e1f5a3"},
	{Version: 69, Filename: "069_tenant_limits.sql", Checksum: "2117acf9f6e13ca87d67319d8cb9e9182a6afc778a39367c7bfa964888011345"},
	{Version: 70, Filename: "070_lifecycle_state_rename.sql", Checksum: "e6f32686347c4d8c22c41b329cb55f97eca325312d60d7420ea5eaa4ed11bb7b"},
	{Version: 71, Filename: "071_type_name_rename.sql", Checksum: "2dc9704409a3901e331910bef86d7f39a08b5a033f680314cbf7597849c845b3"},
	{Version: 72, Filename: "072_block_type_registry.sql", Checksum: "9d91f65ff5cd158a634d7cf18ef72d74ca2e9d8f7d9122cfc83921c419d4cf42"},
	{Version: 73, Filename: "073_rrf_policy_params.sql", Checksum: "59296a7ef60224ea9cb5d234e5d5c7a9d1d414a13021d099001c16bbcc44be04"},
	{Version: 74, Filename: "074_guard_check_type_policy.sql", Checksum: "9a7f5f61abf87dd0a095885bba5ff11a766a0da374318fc7f373ec2c94a8e694"},
	{Version: 75, Filename: "075_drop_is_meta.sql", Checksum: "a79f78c6209df98f5366aff3f0416f1d5a0ba6f60ab627ef5678fb6446ffe509"},
	{Version: 76, Filename: "076_structural_links.sql", Checksum: "98f8ae62a5d1b96620ecc2b3bfb040bf4406e3be93fab3d6a62626270b9ceae7"},
	{Version: 77, Filename: "077_workflow_status.sql", Checksum: "57ba556f7197fa7c89a05c6ed720e6b3bd69b9db92b8b2a655bc96cfa9e472d2"},
	{Version: 78, Filename: "078_write_scopes.sql", Checksum: "9451b3c73736e7b09a5047352994ba8fb5d7a963825a95fc41a26e813f1a9ef3"},
	{Version: 79, Filename: "079_context_projects.sql", Checksum: "e0ab4742551f6627b24065f3bd45b905352c17d7304a398bbae6b416d9b64b4e"},
	{Version: 80, Filename: "080_forge_sync.sql", Checksum: "7a2bfb006445d4355702ac1bbcfc4caa9f4d38b8bd250eae4f1c786377bf3a42"},
	{Version: 81, Filename: "081_project_notify.sql", Checksum: "98502674756a6e56c7e9aac29428d2b164c32d11d55d315a088a705f4e852af4"},
	{Version: 82, Filename: "082_webhook_inbox.sql", Checksum: "0ca5706d7d0f8fddde373c91b8267843431db6f3f84eecbf88902e0d02ed7f9c"},
	{Version: 83, Filename: "083_block_type_refcount_index.sql", Checksum: "6ce88e74a4c210523c3b9121d863d2e06e3359a6e30d08ec8e8fdfc857a6b95d"},
	{Version: 84, Filename: "084_issue_comment_type_seeds.sql", Checksum: "0487849986b7cb6d3f5d7e4aedd5f3e239612377f5d28d125cd90fa36e2b2cfa"},
	{Version: 85, Filename: "085_comment_seed_flip.sql", Checksum: "7c2b768f8f7d7ed6ddd1f8c9205ac6bb01c59fc1447302eff6ea97550fae5aee"},
	{Version: 86, Filename: "086_workflow_created_index.sql", Checksum: "d7a185748824a81c7ba88e7cbdc0bfb85019aae423e15810ba7a75c4e1234147"},
	{Version: 87, Filename: "087_member_scope.sql", Checksum: "6d27892bcaca329d301d86ff4472b50497523b5070ea334264f42cd0cc7a4d76"},
	{Version: 88, Filename: "088_meta_scope_pk.sql", Checksum: "c2ec9dcad63f0c8ef43960e1babd76a7cdd7b8c3b724cbbdd97d0d55befc2701"},
	{Version: 89, Filename: "089_pending_writes.sql", Checksum: "eb11500af45ab945478dac7c21b3e6b27cff3765f5e140efd62e2f00715251b4"},
	{Version: 90, Filename: "090_confirm_writes_capability.sql", Checksum: "f394558609e32f891c7f74c7b67371ea4f1b1541fdef5b3bbd776b653ccd26c7"},
	{Version: 91, Filename: "091_dispatch_telemetry.sql", Checksum: "6a3fc1272233f2cbab5c42178ad6bf4e0f96c891e2af96eca035e690571db07a"},
	{Version: 92, Filename: "092_disable_profiles.sql", Checksum: "a507ae815d90fad139488d34458187c57d10e42eadf48704c3274e2005d68cf1"},
	{Version: 93, Filename: "093_graph_category_hues.sql", Checksum: "f91872012d4e43e3ab98d110474467b848df90915fcddbdb111d5cea0c8767f9"},
	{Version: 94, Filename: "094_principal_identity.sql", Checksum: "98ef4bedb21b6160c35e3fc5dee923c2acc7ed6d8830ea5428edb24c9f3837fb"},
	{Version: 95, Filename: "095_ctx_auth_principal.sql", Checksum: "43e71d22a2583ef0c3906f5d5891e243e089c4d0b2b24b747a52d53089a3d50e"},
	{Version: 96, Filename: "096_audit_principal_fks.sql", Checksum: "099b196e21fec7de79dccc2d3be4158c97a94d754cb0814f2498ceb90f27456d"},
	{Version: 97, Filename: "097_oauth_client_model.sql", Checksum: "ab49f18fc8c2d83be98eb369474b6bb9c0b6facbdc80ba70d1917881eeecc545"},
	{Version: 98, Filename: "098_oauth_codes.sql", Checksum: "d7bd7d37cd6653f32cd6039e6c9279f6c05816cce84f0584400e4c269a586cb1"},
	{Version: 99, Filename: "099_access_tokens.sql", Checksum: "1eee6cd35fd3736c635c63320e16e955b40cded6eba8e392d104c50a63a218f5"},
	{Version: 100, Filename: "100_oauth_providers.sql", Checksum: "0d713be28b82c27f255798263af9f94e30485f71c3bbb2a8cd0d52d9511a9f83"},
	{Version: 101, Filename: "101_sso_states.sql", Checksum: "7ff3810686f163373e82f536eaf1feedb9737dc5cdd3128499538c2e3cfb7627"},
	{Version: 102, Filename: "102_web_sessions.sql", Checksum: "b04ab99fbfddd321311c595a3f6035e9c04ffc8b5442c90eeb99ecc3f2c614de"},
	{Version: 103, Filename: "103_audit_trail_structural_references.sql", Checksum: "de69989c6e5534406499130f1be41e03097df0df56934edfb37c977582811ea7"},
	{Version: 104, Filename: "104_struct_links_graph_traversal.sql", Checksum: "f32f21b21aa8dc031bf290eb3d38cc7490687350112604e5b203a3e0090ad41a"},
	{Version: 105, Filename: "105_structural_class_collision_preflight.sql", Checksum: "e16bd5708ac3d5644937a98f0e8c35dd5376edb2d89b19efa7d8b5074b384e74"},
	{Version: 106, Filename: "106_graph_stat_indexes.sql", Checksum: "3aecea3e9002c58798c1274b70d99c00e2f87a9086b4f44b2675020cffc0135c"},
	{Version: 107, Filename: "107_checkpoint_type_seed.sql", Checksum: "2807f2e17205e6153e498034ff2f25dbb36486a77a34a5b7261ec8b00c0b3b69"},
	{Version: 108, Filename: "108_migrations_checksum.sql", Checksum: "29e04744438d55fb8415476264cc4775d3479e981fc4f1b25d64e580751867b4"},
	{Version: 109, Filename: "109_embed_provenance.sql", Checksum: "7f7b9c4cb51bc2891a65f7ad811d991f9fad0621df6b8b8e9dd7a74c7f0d1629"},
	{Version: 110, Filename: "110_recall_runs.sql", Checksum: "e581bdab3c732507029ba27cefff0ddd1e87aa2074600025c2caaadfe7388a92"},
	{Version: 111, Filename: "111_stratify_covering_index.sql", Checksum: "6656d472538dcde6f8addf8c4fa8f2bf0f92f4dfdf4f3bf2208e99cfc9b16258"},
	{Version: 112, Filename: "112_rrf_gen15_dual_arm.sql", Checksum: "9037ae30a5b66bdce33ee012189459953186179861543889bc055d0e9b76a92b"},
	{Version: 113, Filename: "113_embed_failures.sql", Checksum: "ade92846b7feb681b1ae97cca30e529ded7b52e01b4d27839c0f2631df476333"},
}

// Folded returns the folded migration chain in version order.
func Folded() []FoldedMigration {
	out := make([]FoldedMigration, len(folded))
	copy(out, folded)
	return out
}

// IsFolded reports whether a version's SQL lives inside the baseline.
func IsFolded(version int) bool {
	for _, f := range folded {
		if f.Version == version {
			return true
		}
	}
	return false
}

const (
	markerBegin = "-- @@ ctx-fold begin "
	markerEnd   = "-- @@ ctx-fold end "
)

// Section returns the SQL text of one folded migration, byte for byte as the
// baseline carries it. Callers that used to read a now-folded file out of FS
// read it through here instead; for anything still shipping as its own file
// (114 and up) Section falls through to FS so a caller does not have to know
// which side of the fold line its migration sits on.
func Section(filename string) ([]byte, error) {
	body, err := FS.ReadFile(BaselineFile)
	if err != nil {
		return nil, fmt.Errorf("reading baseline: %w", err)
	}
	start := []byte(markerBegin + filename + "\n")
	end := []byte(markerEnd + filename + "\n")
	i := bytes.Index(body, start)
	if i < 0 {
		// Not folded — an ordinary embedded migration.
		b, err := FS.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("%s is neither a baseline section nor an embedded file: %w", filename, err)
		}
		return b, nil
	}
	i += len(start)
	j := bytes.Index(body[i:], end)
	if j < 0 {
		return nil, fmt.Errorf("baseline section %s has no end marker", filename)
	}
	out := make([]byte, j)
	copy(out, body[i:i+j])
	return out, nil
}
