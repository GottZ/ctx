package oai_test

// The re-authentication harness of the stream route (W02-08): a scriptable
// Deps.Reauth. Separate from chat_stream_harness_test.go, which compiles
// against the base too (the red runs of the relay gates).

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/GottZ/ctx/internal/auth"
)

// reauthFake is a scriptable Deps.Reauth: every call takes the next answer
// of script (the last repeats); counts calls.
type reauthFake struct {
	mu     sync.Mutex
	script []reauthAnswer
	calls  atomic.Int64
}

// reauthAnswer is one scripted answer: ok with ca nil hands the consumer
// back as it came in; ca set hands back that consumer (the door read it
// changed); not ok and no error: no longer valid.
type reauthAnswer struct {
	ok  bool
	ca  *auth.ConsumerAuth
	err error
}

func (f *reauthFake) set(script ...reauthAnswer) {
	f.mu.Lock()
	f.script = script
	f.mu.Unlock()
}

func (f *reauthFake) reauth(_ context.Context, ca *auth.ConsumerAuth) (*auth.ConsumerAuth, error) {
	n := f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 {
		return ca, nil
	}
	a := f.script[min(int(n)-1, len(f.script)-1)]
	switch {
	case a.err != nil:
		return nil, a.err
	case !a.ok:
		return nil, nil
	case a.ca != nil:
		return a.ca, nil
	}
	return ca, nil
}

func withReauth(f *reauthFake) rigOption {
	return func(r *chatRig) { r.deps.Reauth = f.reauth }
}

