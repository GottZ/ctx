package keyquota

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func sortedReasons() []string {
	out := make([]string, 0, len(reasons))
	for r := range reasons {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// TestLedgerReasonsMatchTheCheck pins the reason vocabulary against the CHECK
// of migration 167: a reason the ledger counts but the table refuses would
// fail the flush statement with every other count in it.
func TestLedgerReasonsMatchTheCheck(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/167_llm_rejections.sql")
	if err != nil {
		t.Fatalf("read migration 167: %v", err)
	}
	m := regexp.MustCompile(`CHECK \(reason IN \(([^)]*)\)\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("migration 167 has no CHECK (reason IN (...))")
	}
	var check []string
	for _, v := range strings.Split(string(m[1]), ",") {
		check = append(check, strings.Trim(strings.TrimSpace(v), "'"))
	}
	slices.Sort(check)
	if got := sortedReasons(); !slices.Equal(got, check) {
		t.Fatalf("ledger reasons %v, CHECK of 167 %v", got, check)
	}
}
