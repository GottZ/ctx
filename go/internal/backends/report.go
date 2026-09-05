package backends

import "time"

// ReportFunc feeds attempt outcomes back into the pool's health state.
// ClassOK clears the failure streak; everything else earns the class
// cooldown. Wired to Pool.ReportSuccess/ReportFailure by the caller — the
// wire packages (llm chat chain, embedcache embed sequence) take it as a
// parameter and stay free of pool state. It lives here, next to the pool it
// feeds, so both wire families name the SAME func type: a reporter built for
// one is assignable to the other without a conversion that could silently
// re-order the arguments.
type ReportFunc func(backendID string, class ErrClass, retryAfter time.Duration)
