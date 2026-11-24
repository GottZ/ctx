package handler

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// BenchmarkManageNULField — the review of #530 (note 6): with a NUL present,
// a 1 MB data object of many keys took 57 ms and 233,000 allocations to name
// the field. The dispatcher now finds it in one pass over the text.
func BenchmarkManageNULField(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; sb.Len() < 1<<20; i++ {
		fmt.Fprintf(&sb, `"key%07d":"value %d",`, i, i)
	}
	sb.WriteString(`"last":"x` + "\\" + `u0000"}`)
	req := manageRequest{Action: "get", Data: json.RawMessage(sb.String())}
	b.ReportAllocs()
	b.SetBytes(int64(len(req.Data)))
	for b.Loop() {
		if manageNULField(req) != "data.last" {
			b.Fatal("not found")
		}
	}
}
