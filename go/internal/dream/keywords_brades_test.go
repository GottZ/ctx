package dream

import "testing"

// TestExtractKeywords_BradesContent reproduces the prod 2026-08-23 case:
// dense Windows-path content where the LLM degenerates. The deterministic
// fallback must still find >= MinKeywords meaningful terms. The fixture is
// synthetic but keeps the failure shape of the original block — backslash
// paths, drive letters, key=value pairs, UNC shares — so tokenize's
// path/symbol splitting is exercised without shipping private block content.
func TestExtractKeywords_BradesContent(t *testing.T) {
	content := `Migration: robocopy E:\brades-export\daten\2026 -> \\BRADES-SERVER\archiv$\jahr; ` +
		`Task-XML C:\safe\tasks\brades-sync.xml, Log unter E:\logs\sync_2026-08.log; ` +
		`rclone.conf [brades] type=sftp host=10.13.37.40 user=svc_brades; ` +
		`postgres: psql -U brades -d brades_stats -c "SELECT * FROM rclone_transfer_stats"`
	kws := ExtractKeywords("Migration BRADES-SERVER: Stand + Prozess", content, MaxKeywords)
	t.Logf("fallback keywords (%d): %v", len(kws), kws)
	if len(kws) < MinKeywords {
		t.Fatalf("ExtractKeywords = %d keywords, want >= %d", len(kws), MinKeywords)
	}
}
