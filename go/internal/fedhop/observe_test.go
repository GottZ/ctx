package fedhop

// Gates of F-W3 on the Observe scaffold (design/04 §4.4): it reads the
// response headers on every tier — None included — and calls the Observer of
// the context (else the default): Ctx-Instance ⇒ NotePeerInstance, the own
// instance id or a 508 echoing an own Ctx-Loop-Token ⇒ MarkSelfLoop. It only
// reads and only excludes; the learned state is F-W11's. A pinned embed space
// (Target.EmbedSpace) that the 2xx answer does not echo ⇒ ErrEmbedSpaceMismatch.
//
//	go test -count=1 -run 'TestObserve' ./internal/fedhop/

import (
	"errors"
	"net/http"
	"slices"
	"testing"
)

type recObserver struct{ calls []string }

func (r *recObserver) NotePeerInstance(backendID, instance string) {
	r.calls = append(r.calls, "note:"+backendID+":"+instance)
}

func (r *recObserver) MarkSelfLoop(backendID string) {
	r.calls = append(r.calls, "self:"+backendID)
}

func resetDefaultObserver(t *testing.T) {
	t.Helper()
	defaultObserver.Store(nil)
	t.Cleanup(func() { defaultObserver.Store(nil) })
}

func TestObserve(t *testing.T) {
	resetDefaultObserver(t)
	resetDefault(t)
	self := NewIdentity(testID, testSecret)
	own := self.MintToken()
	foreign := NewIdentity(uid(2), []byte("another instance key, 32 bytes!!")).MintToken()
	for _, c := range []struct {
		name   string
		tier   Tier
		status int
		resp   http.Header
		want   []string
	}{
		{"peer instance on tier F", TierFederation, 200, hdr(HeaderInstance, uid(2)), []string{"note:b:" + uid(2)}},
		{"peer instance on tier L", TierLoop, 200, hdr(HeaderInstance, uid(2)), []string{"note:b:" + uid(2)}},
		{"peer instance on tier None", TierNone, 500, hdr(HeaderInstance, uid(2)), []string{"note:b:" + uid(2)}},
		{"own instance id", TierLoop, 200, hdr(HeaderInstance, testID), []string{"note:b:" + testID, "self:b"}},
		{"instance upper case", TierLoop, 200, hdr(HeaderInstance, "6F1C2C9E-4D0B-4A52-9A43-0C3F5E1D2B7A"), nil},
		{"instance garbage", TierLoop, 200, hdr(HeaderInstance, "x"), nil},
		{"instance two lines", TierLoop, 200, hdr(HeaderInstance, uid(2), HeaderInstance, uid(3)), nil},
		{"508 own loop token", TierLoop, 508, hdr(HeaderLoopToken, own), []string{"self:b"}},
		{"508 foreign loop token", TierLoop, 508, hdr(HeaderLoopToken, foreign), nil},
		{"508 without token", TierLoop, 508, hdr(HeaderReason, "loop"), nil},
		{"own loop token without 508", TierLoop, 200, hdr(HeaderLoopToken, own), nil},
		{"508 own token and own id", TierLoop, 508, hdr(HeaderLoopToken, own, HeaderInstance, testID), []string{"note:b:" + testID, "self:b"}},
		{"nothing", TierFederation, 200, http.Header{}, nil},
		{"nil header", TierFederation, 200, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			obs := &recObserver{}
			ctx := WithObserver(WithIdentity(t.Context(), self), obs)
			if err := Observe(ctx, Target{BackendID: "b", Tier: c.tier}, c.status, c.resp); err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if !slices.Equal(obs.calls, c.want) {
				t.Errorf("observer calls %v, want %v", obs.calls, c.want)
			}
		})
	}
	// The default observer serves contexts without one; none at all is no panic.
	if err := Observe(WithIdentity(t.Context(), self), Target{BackendID: "b"}, 200, hdr(HeaderInstance, uid(2))); err != nil {
		t.Fatal(err)
	}
	def := &recObserver{}
	SetDefaultObserver(def)
	_ = Observe(WithIdentity(t.Context(), self), Target{BackendID: "d"}, 200, hdr(HeaderInstance, uid(4)))
	if !slices.Equal(def.calls, []string{"note:d:" + uid(4)}) {
		t.Errorf("default observer calls %v", def.calls)
	}
	ctxObs := &recObserver{}
	_ = Observe(WithObserver(WithIdentity(t.Context(), self), ctxObs), Target{BackendID: "c"}, 200, hdr(HeaderInstance, uid(4)))
	if len(def.calls) != 1 || len(ctxObs.calls) != 1 {
		t.Errorf("context observer did not take precedence: default %v, context %v", def.calls, ctxObs.calls)
	}
	// The zero Identity never takes a foreign answer for a self-loop.
	zero := &recObserver{}
	_ = Observe(WithObserver(t.Context(), zero), Target{BackendID: "z"}, 508, hdr(HeaderLoopToken, own, HeaderInstance, uid(2)))
	if !slices.Equal(zero.calls, []string{"note:z:" + uid(2)}) {
		t.Errorf("zero identity: %v", zero.calls)
	}
}

func TestObserveEmbedSpace(t *testing.T) {
	resetDefaultObserver(t)
	ctx := selfCtx()
	const space = "0123456789abcdef"
	pinned := Target{BackendID: "e", Tier: TierFederation, EmbedSpace: space}
	for _, c := range []struct {
		name   string
		t      Target
		status int
		resp   http.Header
		want   error
	}{
		{"echoed", pinned, 200, hdr(HeaderEmbedSpace, space), nil},
		{"missing", pinned, 200, http.Header{}, ErrEmbedSpaceMismatch},
		{"other space", pinned, 200, hdr(HeaderEmbedSpace, "fedcba9876543210"), ErrEmbedSpaceMismatch},
		{"upper case", pinned, 200, hdr(HeaderEmbedSpace, "0123456789ABCDEF"), ErrEmbedSpaceMismatch},
		{"two lines", pinned, 200, hdr(HeaderEmbedSpace, space, HeaderEmbedSpace, space), ErrEmbedSpaceMismatch},
		{"error status is classified by status", pinned, 409, http.Header{}, nil},
		{"204 echoed", pinned, 204, hdr(HeaderEmbedSpace, space), nil},
		{"204 missing", pinned, 204, http.Header{}, ErrEmbedSpaceMismatch},
		{"299 missing", pinned, 299, http.Header{}, ErrEmbedSpaceMismatch},
		{"300 missing", pinned, 300, http.Header{}, nil},
		{"not pinned", Target{BackendID: "e", Tier: TierFederation}, 200, hdr(HeaderEmbedSpace, "fedcba9876543210"), nil},
		{"unreadable pin echoed", Target{BackendID: "e", Tier: TierFederation, EmbedSpace: "0123456789ABCDEF"}, 200, hdr(HeaderEmbedSpace, "0123456789ABCDEF"), ErrEmbedSpaceMismatch},
	} {
		if err := Observe(ctx, c.t, c.status, c.resp); !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: Observe = %v, want %v", c.name, err, c.want)
		}
	}
}
