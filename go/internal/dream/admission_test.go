package dream

import (
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
)

// testDispatcher is ONE shared pass-through dispatcher for the dream test
// binary (MW3: Router.Admit is mandatory — a router without an admitter
// walks no chain). Empty policy = Durchreiche, so no test behavior changes;
// the reaper goroutine lives for the process, which is fine in a test binary.
var testDispatcher = dispatch.New(nil, dispatch.DefaultSettings())

// testAdmit is the background admission every dream test router binds —
// mirroring the production scheduler binding (design/01 §4.6 N1).
func testAdmit() llm.Admission {
	return llm.Admission{Admitter: testDispatcher, Class: dispatch.ClassBackground}
}
