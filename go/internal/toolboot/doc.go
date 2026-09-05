// Package toolboot — the boot sequence every binary runs before it can do
// anything: read the environment, check it, open the pool. One order, five
// entry points, and the OUTPUT stays with the caller.
//
// THE ORDER is the contract (design/01-schnitt.md G14, established by wave
// T01-7): FromEnv, then Validate, then the report, then HasErrors, and only
// then store.NewPool. A tool that holds a connection to a live database
// before anyone has looked at its cross-field config is the expensive class
// of mistake at N tenants and 1M-10M blocks — and a writing tool sits at the
// end of that chain.
//
// WHY report TAKES THE WHOLE LIST AT ONCE. The five entry points print three
// different things today, and all three stay byte-identical: cmd/ctxd logs
// EVERY issue and then dies if any of them is an error; the overview worker
// prints only SeverityError, and only when it is aborting; the three ctx-*
// tools print every issue, also only when aborting. "Print only if there are
// errors at all" needs the total picture, and that exists only after the last
// issue — a callback fired per issue could not express it unless every caller
// buffered the list itself. So report is called EXACTLY ONCE, with the
// complete slice and the verdict of config.HasErrors, and the caller alone
// decides what reaches which writer.
//
// WHAT DELIBERATELY STAYS OUTSIDE. signal.NotifyContext belongs to the
// caller: cmd/ctxd takes syscall.SIGINT + syscall.SIGTERM, the tools take
// os.Interrupt + syscall.SIGTERM, and the derived context arrives here as the
// first parameter. So does the exit form — os.Exit(1) in a main, return 1 out
// of a run(...) int. So does every perimeter check a tool runs before it
// touches a database at all (llmlog.CheckExportDir in ctx-armcost and
// ctx-llmlog-export). And so does the settings overlay: callers that need the
// env issues for settings.Bootstrap keep them from the report callback, which
// is the one place they pass by.
//
// NO SWITCH FOR Validate. Whether a tool aborts on HasErrors is decision
// E01-7, answered by T01-7 with "yes, the way ctxd does". Open asks HasErrors
// because that is the answer, not because it is configurable. If the answer
// ever changes it changes ONE line in this package instead of five call sites
// under cmd/ — which is the entire reason the package exists.
package toolboot
