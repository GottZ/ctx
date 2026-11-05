//go:build integration

// W01-6 gates of the /v1 model-key door (design/01 §4.2, §7 W01-6) on a probe
// router: handler.ModelAuth over a real ctx_model_auth (migration 161), a
// test renderer in place of W02-02's writer (K-02-1), and a pgx tracer that
// counts statements per request.
//
//	go test -tags=integration -p 1 -count=1 -run 'ModelAuth|Consumer|Auth' -v ./internal/handler/
package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
)

// Gate (a) — saturation (B26): 8193 sources with CRC-valid forgeries fill
// the limiter; a valid key from a fresh source still passes. Red against the
// deny-new policy of ipLimiter.allow.
func TestModelAuthSaturation_Integration(t *testing.T) {
	e := madSetup(t)
	e.settings.auth(func(a *oai.AuthSettings) { a.ProbeRate = 1_000_000 })
	d, h, rec := e.door(t, "")
	_, _, valid := e.consumer(t, store.ModelConsumerSpec{})
	const sources = modelAuthLimiterCap + 1
	keys := forged(t, sources)
	var mu sync.Mutex
	statuses := map[int]int{}
	parallel(sources, 16, func(i int) {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&255, (i>>8)&255, i&255)
		r := madDo(h, madReq{ip: ip, bearer: keys[i].Plaintext()})
		mu.Lock()
		statuses[r.Code]++
		mu.Unlock()
	})
	t.Logf("flood: %d sources, statuses %v, limiter buckets %d", sources, statuses, d.limiter.len())
	if statuses[http.StatusUnauthorized] != sources {
		t.Fatalf("flood statuses %v, want %d × 401", statuses, sources)
	}
	if d.limiter.len() != modelAuthLimiterCap {
		t.Fatalf("limiter holds %d buckets, want the cap %d", d.limiter.len(), modelAuthLimiterCap)
	}
	got := madDo(h, madReq{ip: "172.16.200.1", bearer: valid.Plaintext()})
	t.Logf("valid key from a fresh source after the flood: %d %s", got.Code, strings.TrimSpace(got.Body.String()))
	if got.Code != http.StatusOK || rec.count() != 1 {
		t.Fatalf("valid key after saturation: %d %s reached=%d, want 200 (no 429)", got.Code, got.Body.String(), rec.count())
	}
	if st := d.Stats(); st.Locked != 0 {
		t.Fatalf("%d lock 429s during a flood of one-shot keys, want 0", st.Locked)
	}
}

// Gate (b) — key-scoped lock (B27): K1 collects the limit of failures from
// X; K2 (never seen) from X passes; K1's next attempt is a 429 without a
// database query. Red against an IP-only lock.
func TestModelAuthKeyScopedLock_Integration(t *testing.T) {
	e := madSetup(t)
	// The probe bucket stays out of this gate: every attempt below must
	// reach the database or the lock, not the bucket's 503.
	e.settings.auth(func(a *oai.AuthSettings) { a.ProbeRate = 1_000_000 })
	d, h, rec := e.door(t, "")
	limit := e.settings.get().Auth.FailureLimit
	k1 := forged(t, 1)[0]
	_, _, k2 := e.consumer(t, store.ModelConsumerSpec{})
	const x = "203.0.113.9"
	for i := 0; i < limit; i++ {
		if r := madDo(h, madReq{ip: x, bearer: k1.Plaintext()}); r.Code != http.StatusUnauthorized {
			t.Fatalf("K1 attempt %d: %d, want 401", i+1, r.Code)
		}
	}
	r := madDo(h, madReq{ip: x, bearer: k2.Plaintext(), tag: "k2"})
	t.Logf("K2 from X after %d K1 failures: %d, statements %d", limit, r.Code, len(e.tracer.of("k2")))
	if r.Code != http.StatusOK || rec.count() != 1 {
		t.Fatalf("K2 from X: %d %s, want 200", r.Code, r.Body.String())
	}
	r = madDo(h, madReq{ip: x, bearer: k1.Plaintext(), tag: "k1-31"})
	stmts := e.tracer.of("k1-31")
	t.Logf("K1 attempt %d: %d %s Retry-After=%q x-should-retry=%q statements=%d", limit+1, r.Code, strings.TrimSpace(r.Body.String()), r.Header().Get("Retry-After"), r.Header().Get("x-should-retry"), len(stmts))
	if r.Code != http.StatusTooManyRequests || madCode(r) != oai.CodeRateLimitExceeded || r.Header().Get("Retry-After") == "" || r.Header().Get("x-should-retry") != "false" {
		t.Fatalf("K1 attempt %d: %d %s, want 429 rate_limit_exceeded + Retry-After", limit+1, r.Code, r.Body.String())
	}
	if len(stmts) != 0 {
		t.Fatalf("the 429 issued %d statement(s) %v, want 0", len(stmts), stmts)
	}
	// IPv6: the bucket is the /64. Failures from two hosts of one /64 add up;
	// another /64 is a different bucket.
	k3 := forged(t, 1)[0]
	for i := 0; i < limit; i++ {
		madDo(h, madReq{ip: fmt.Sprintf("2001:db8::%x", i+1), bearer: k3.Plaintext()})
	}
	if r := madDo(h, madReq{ip: "2001:db8::ffff:1", bearer: k3.Plaintext()}); r.Code != http.StatusTooManyRequests {
		t.Errorf("K3 from the same /64: %d, want 429", r.Code)
	}
	if r := madDo(h, madReq{ip: "2001:db8:0:1::1", bearer: k3.Plaintext()}); r.Code != http.StatusUnauthorized {
		t.Errorf("K3 from another /64: %d, want 401", r.Code)
	}
	if d.Stats().Locked != 2 {
		t.Errorf("lock counter %d, want 2", d.Stats().Locked)
	}
}

// Gate (c) — probe bucket (B18): a flood of 10 × auth_probe_rate unknown
// forgeries per second reaches the database at most at the bucket's rate
// (burst = rate, refill = rate/s); a known key passes meanwhile. Red
// without the bucket.
func TestModelAuthProbeBucket_Integration(t *testing.T) {
	e := madSetup(t)
	d, h, rec := e.door(t, "")
	rate := e.settings.get().Auth.ProbeRate
	_, _, known := e.consumer(t, store.ModelConsumerSpec{})
	if r := madDo(h, madReq{bearer: known.Plaintext()}); r.Code != http.StatusOK {
		t.Fatalf("known key warm-up: %d", r.Code)
	}
	const flood = 2 * time.Second
	n := int(flood.Seconds()) * 10 * rate
	keys := forged(t, n)
	interval := time.Second / time.Duration(10*rate)
	statuses := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	knownFails := 0
	stop := make(chan struct{})
	go func() { // the known key, every 50 ms during the flood
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if r := madDo(h, madReq{bearer: known.Plaintext(), tag: "known"}); r.Code != http.StatusOK {
					mu.Lock()
					knownFails++
					mu.Unlock()
				}
			}
		}
	}()
	start := time.Now()
	for i := 0; i < n; i++ {
		if wait := time.Until(start.Add(time.Duration(i) * interval)); wait > 0 {
			time.Sleep(wait)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.20.%d.%d", (i>>8)&255, i&255)
			r := madDo(h, madReq{ip: ip, bearer: keys[i].Plaintext(), tag: "flood"})
			k := fmt.Sprintf("%d %s retry-after=%s should-retry=%s", r.Code, madCode(r), r.Header().Get("Retry-After"), r.Header().Get("x-should-retry"))
			mu.Lock()
			statuses[k]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stop)
	probes := e.tracer.probesOf("flood")
	bound := float64(rate) + float64(rate)*elapsed.Seconds()
	perSecond := map[int]int{}
	for _, at := range probes {
		perSecond[int(at.Sub(start)/time.Second)]++
	}
	t.Logf("flood: %d forgeries in %v (%d/s offered), DB probes %d (bound burst+rate·t = %.1f), per second %v, statuses %v, known-key failures %d, known-key requests reached %d",
		n, elapsed.Round(time.Millisecond), 10*rate, len(probes), bound, perSecond, statuses, knownFails, rec.count())
	if float64(len(probes)) > bound {
		t.Fatalf("%d DB probes from unknown hashes in %v, above the bucket bound %.1f", len(probes), elapsed, bound)
	}
	if knownFails != 0 {
		t.Fatalf("the known key failed %d time(s) during the flood", knownFails)
	}
	if statuses["503 auth_overloaded retry-after=1 should-retry=false"] == 0 {
		t.Fatalf("no 503 auth_overloaded (Retry-After: 1, x-should-retry: false) in %v", statuses)
	}
	if st := d.Stats(); st.ProbeRejected == 0 {
		t.Fatalf("probe counter %d", st.ProbeRejected)
	}
}

// Gate (e) — mutual exclusion (B6): Auth and ModelAuth stacked, in both
// orders, never reach the handler — the ModelAuth side answers 503 (K5),
// the Auth side 500 in the house format. The request carries both
// credentials (knowledge key in X-Context-Key, model key as Bearer), so
// without the guards both doors pass. Red without the guards.
func TestModelAuthMutualExclusion_Integration(t *testing.T) {
	e := madSetup(t)
	ctx := context.Background()
	_, knowledge, err := store.CreateApiKey(ctx, e.seed, "mad-knowledge", "private", nil, "")
	if err != nil {
		t.Fatalf("knowledge key: %v", err)
	}
	_, _, model := e.consumer(t, store.ModelConsumerSpec{})
	both := madReq{bearer: model.Plaintext(), hdr: map[string]string{"X-Context-Key": knowledge}}

	d, _, _ := e.door(t, "")
	orders := []struct {
		name   string
		build  func(next http.Handler) http.Handler
		status int
		body   string
	}{
		{"Auth→ModelAuth", func(next http.Handler) http.Handler { return Auth(e.pool)(d.Handler(next)) }, http.StatusServiceUnavailable, `"code":"internal_error"`},
		{"ModelAuth→Auth", func(next http.Handler) http.Handler { return d.Handler(Auth(e.pool)(next)) }, http.StatusInternalServerError, `{"error":"authentication error"}`},
	}
	for _, o := range orders {
		probe := &madRecorder{}
		r := madDo(o.build(probe), both)
		t.Logf("%s: %d %s x-should-retry=%q reached=%d", o.name, r.Code, strings.TrimSpace(r.Body.String()), r.Header().Get("x-should-retry"), probe.count())
		if probe.count() != 0 {
			t.Errorf("%s: the handler was reached with consumer=%v knowledge=%v", o.name, probe.ca != nil, probe.ar != nil)
			continue
		}
		if r.Code != o.status || !strings.Contains(r.Body.String(), o.body) {
			t.Errorf("%s: %d %s, want %d %s", o.name, r.Code, r.Body.String(), o.status, o.body)
		}
	}
	if r := madDo(Auth(e.pool)(d.Handler(&madRecorder{})), both); r.Header().Get("x-should-retry") != "false" {
		t.Errorf("ModelAuth-side refusal: x-should-retry %q, want false", r.Header().Get("x-should-retry"))
	}
	if st := d.Stats(); st.WiringConflicts == 0 {
		t.Errorf("wiring-conflict counter %d", st.WiringConflicts)
	}
}

// Gate (f) — proxy consistency and transport (B22, B24, E-01-13/14).
func TestModelAuthProxyTransport_Integration(t *testing.T) {
	e := madSetup(t)
	_, _, key := e.consumer(t, store.ModelConsumerSpec{})
	_, _, cidrKey := e.consumer(t, store.ModelConsumerSpec{AllowedCIDRs: []string{"198.51.100.0/24"}})

	t.Run("no-trusted-proxy-with-xff", func(t *testing.T) {
		d, h, rec := e.door(t, "")
		r := madDo(h, madReq{bearer: key.Plaintext(), hdr: map[string]string{"X-Forwarded-For": "198.51.100.7"}, tag: "f-noproxy"})
		stmts := e.tracer.of("f-noproxy")
		t.Logf("trustedProxy=\"\" + XFF: %d %s x-should-retry=%q statements=%d", r.Code, strings.TrimSpace(r.Body.String()), r.Header().Get("x-should-retry"), len(stmts))
		if r.Code != http.StatusServiceUnavailable || madCode(r) != ModelAuthCodeProxyMisconfigured || r.Header().Get("x-should-retry") != "false" || len(stmts) != 0 || rec.count() != 0 {
			t.Fatalf("want 503 proxy_misconfigured, x-should-retry false, 0 statements")
		}
		if d.Stats().ProxyMisconfigured != 1 {
			t.Fatalf("counter %d", d.Stats().ProxyMisconfigured)
		}
	})

	const proxy = "192.0.2.10"
	viaProxy := func(xfp string) madReq {
		return madReq{ip: proxy, plain: true, bearer: key.Plaintext(),
			hdr: map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": xfp}}
	}
	t.Run("proxy-http-without-allow-plaintext", func(t *testing.T) {
		_, h, rec := e.door(t, proxy)
		r := madDo(h, viaProxy("http"))
		t.Logf("trusted proxy + XFP http: %d %s", r.Code, strings.TrimSpace(r.Body.String()))
		if r.Code != http.StatusUnauthorized || rec.count() != 0 {
			t.Fatalf("want 401")
		}
	})
	t.Run("proxy-https", func(t *testing.T) {
		_, h, rec := e.door(t, proxy)
		r := madDo(h, viaProxy("https"))
		t.Logf("trusted proxy + XFP https: %d", r.Code)
		if r.Code != http.StatusOK || rec.count() != 1 {
			t.Fatalf("want 200, got %d %s", r.Code, r.Body.String())
		}
		// The client address behind the proxy reaches gate 5.
		q := viaProxy("https")
		q.bearer = cidrKey.Plaintext()
		if r := madDo(h, q); r.Code != http.StatusOK {
			t.Fatalf("CIDR consumer via the proxy (client 198.51.100.7): %d, want 200", r.Code)
		}
		// A proxy appending its own value: the rightmost entry is the proxy's.
		q = viaProxy("https, http")
		if r := madDo(h, q); r.Code != http.StatusUnauthorized {
			t.Errorf("XFP \"https, http\" (proxy says http): %d, want 401", r.Code)
		}
		q = viaProxy("http, https")
		if r := madDo(h, q); r.Code != http.StatusOK {
			t.Errorf("XFP \"http, https\" (proxy says https): %d, want 200", r.Code)
		}
		// Several header LINES (Fremd-Review F1): the client sends its own
		// line, the proxy appends one. The proxy's line counts, not the
		// first one Header.Get would return.
		q = madReq{ip: proxy, plain: true, bearer: key.Plaintext(), lines: map[string][]string{
			"X-Forwarded-For": {"198.51.100.7"}, "X-Forwarded-Proto": {"https", "http"}}}
		if r := madDo(h, q); r.Code != http.StatusUnauthorized {
			t.Errorf("client XFP line https + proxy line http: %d, want 401", r.Code)
		}
		q = madReq{ip: proxy, bearer: cidrKey.Plaintext(), lines: map[string][]string{
			"X-Forwarded-For": {"198.51.100.7", "203.0.113.99"}}}
		r = madDo(h, q)
		t.Logf("client XFF line 198.51.100.7 + proxy line 203.0.113.99, allowlist 198.51.100.0/24: %d", r.Code)
		if r.Code != http.StatusUnauthorized {
			t.Errorf("client XFF line inside the allowlist + proxy line outside: %d, want 401", r.Code)
		}
	})
	t.Run("xfp-and-xff-from-a-non-proxy-peer", func(t *testing.T) {
		_, h, rec := e.door(t, proxy)
		q := viaProxy("https")
		q.ip = "203.0.113.5"
		r := madDo(h, q)
		t.Logf("XFP https from a non-proxy peer, no TLS: %d", r.Code)
		if r.Code != http.StatusUnauthorized || rec.count() != 0 {
			t.Fatalf("XFP from a non-proxy peer counted: %d, want 401", r.Code)
		}
		// Same peer with TLS but a spoofed XFF inside the CIDR consumer's
		// allowlist: the peer address (outside) is what gate 5 sees.
		q = madReq{ip: "203.0.113.5", bearer: cidrKey.Plaintext(), hdr: map[string]string{"X-Forwarded-For": "198.51.100.7"}}
		if r := madDo(h, q); r.Code != http.StatusUnauthorized {
			t.Fatalf("XFF from a non-proxy peer counted for the allowlist: %d, want 401", r.Code)
		}
	})
	t.Run("plaintext-consumer-on-a-private-network", func(t *testing.T) {
		_, h, _ := e.door(t, "")
		_, _, plain := e.consumer(t, store.ModelConsumerSpec{AllowedCIDRs: []string{"10.0.0.0/8"}, AllowPlaintext: true})
		if r := madDo(h, madReq{ip: "10.1.2.3", plain: true, bearer: plain.Plaintext()}); r.Code != http.StatusOK {
			t.Fatalf("allow_plaintext consumer over plaintext from 10.1.2.3: %d, want 200", r.Code)
		}
		if r := madDo(h, madReq{ip: "10.1.2.3", plain: true, bearer: key.Plaintext()}); r.Code != http.StatusUnauthorized {
			t.Fatalf("TLS-only consumer over plaintext: %d, want 401", r.Code)
		}
	})
}

// Knowledge credentials and other carriers reach no database statement at
// all (B3): a 64-hex key and a ctxt_ token as Bearer, the model key in
// X-Context-Key, a cookie or the query string.
func TestModelAuthKnowledgeCredentials_Integration(t *testing.T) {
	e := madSetup(t)
	_, knowledge, err := store.CreateApiKey(context.Background(), e.seed, "mad-knowledge", "private", nil, "")
	if err != nil {
		t.Fatalf("knowledge key: %v", err)
	}
	_, _, model := e.consumer(t, store.ModelConsumerSpec{})
	_, h, rec := e.door(t, "")
	for name, q := range map[string]madReq{
		"knowledge-bearer":   {bearer: knowledge},
		"oauth-token-bearer": {bearer: "ctxt_" + strings.Repeat("B", 43)},
		"model-key-in-x-context-key": {hdr: map[string]string{"X-Context-Key": model.Plaintext()}},
		"model-key-in-cookie":        {hdr: map[string]string{"Cookie": sessionCookieName + "=" + model.Plaintext()}},
	} {
		q.tag = name
		r := madDo(h, q)
		stmts := e.tracer.of(name)
		t.Logf("%s: %d %s statements=%d", name, r.Code, strings.TrimSpace(r.Body.String()), len(stmts))
		if r.Code != http.StatusUnauthorized || madCode(r) != oai.CodeInvalidAPIKey || len(stmts) != 0 {
			t.Errorf("%s: %d, %d statement(s) %v — want 401 and 0", name, r.Code, len(stmts), stmts)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("handler reached %d time(s)", rec.count())
	}
}

// Success: the handler sees a ConsumerAuth with every K20 field, no
// AuthResult; Touch got the key id; the ids go into the request's access
// record and the door writes no line of its own (W06-9: the group's access
// log writes the request's one line). A revoked key is a 401 on the very
// next request.
func TestModelAuthSuccessAndRevoke_Integration(t *testing.T) {
	e := madSetup(t)
	ctx := context.Background()
	scope := "work"
	exp := time.Now().Add(time.Hour)
	c, k, key, err := store.CreateConsumerWithKey(ctx, e.seed, store.ModelConsumerSpec{
		TenantID: store.DefaultTenantID, Label: "mad-full", Kind: "ctx-peer", TenantScope: &scope,
		AllModels: true, PromptSensitivity: "personal", FairWeight: 9, AdmissionLane: "urgent",
	}, &exp, e.stamp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const peer = "0190f000-0000-7000-8000-0000000beef0"
	if _, err := e.seed.Exec(ctx, `UPDATE context_model_consumers SET peer_instance_id = $2, peer_bound_at = now() WHERE id = $1`, c.ID, peer); err != nil {
		t.Fatalf("bind peer: %v", err)
	}
	d, door, rec := e.door(t, "")
	ar := &oai.AccessRecord{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		door.ServeHTTP(w, r.WithContext(oai.WithAccessRecord(r.Context(), ar)))
	})
	r := madDo(h, madReq{bearer: key.Plaintext(), tag: "ok"})
	if r.Code != http.StatusOK || rec.count() != 1 {
		t.Fatalf("valid key: %d %s", r.Code, r.Body.String())
	}
	ca := rec.ca
	t.Logf("ConsumerAuth: %+v; AuthResult nil=%v; statements %d", ca, rec.ar == nil, len(e.tracer.of("ok")))
	if rec.ar != nil || ca == nil || !ca.IsValid {
		t.Fatalf("context: ar=%v ca=%v", rec.ar, ca)
	}
	if ca.ModelKeyID != k.ID || ca.ConsumerID != c.ID || ca.TenantID != store.DefaultTenantID || ca.TenantScope != "work" ||
		ca.Kind != auth.KindCtxPeer || !ca.AllModels || ca.PromptSensitivity != "personal" || ca.FairWeight != 9 ||
		ca.KeyExpiresAt == nil || ca.AdmissionLane != "urgent" || ca.PeerInstanceID != peer {
		t.Fatalf("K20 fields incomplete: %+v", ca)
	}
	if n := len(e.tracer.of("ok")); n != 1 {
		t.Fatalf("%d statements for one auth, want 1", n)
	}
	if fmt.Sprint(e.usage.keys) != "["+k.ID+"]" {
		t.Fatalf("Touch got %v, want [%s]", e.usage.keys, k.ID)
	}
	if ar.ConsumerID != c.ID || ar.ModelKeyID != k.ID || ar.TenantID != store.DefaultTenantID || ar.Reason != "" {
		t.Fatalf("access record %+v, want consumer %s, key %s, tenant %s, no reason", ar, c.ID, k.ID, store.DefaultTenantID)
	}
	if logs := e.logs.String(); strings.Contains(logs, c.ID) || strings.Contains(logs, "accepted") {
		t.Fatalf("the door logged the accepted request itself: %s", logs)
	}
	if !d.known.contains(key.Hash()) {
		t.Fatal("the key is not known-good after a success")
	}

	if err := store.RevokeKey(ctx, e.seed, k.ID, store.ModelAccess{ServerAdmin: true}, e.stamp); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	*ar = oai.AccessRecord{}
	r = madDo(h, madReq{bearer: key.Plaintext()})
	t.Logf("next request after revoke: %d %s", r.Code, strings.TrimSpace(r.Body.String()))
	if r.Code != http.StatusUnauthorized || rec.count() != 1 {
		t.Fatalf("revoked key: %d, want 401", r.Code)
	}
	if ar.Reason != oai.CodeInvalidAPIKey || ar.ConsumerID != "" {
		t.Fatalf("access record after the refusal %+v, want reason %s and no consumer", ar, oai.CodeInvalidAPIKey)
	}
	if d.known.contains(key.Hash()) {
		t.Fatal("a key that failed its check stays exempt from the probe token")
	}
}

// ReauthConsumer (K22): same check, same semaphore, original address and
// transport. Valid ⇒ the consumer as the door reads it now (a changed
// all_models comes back changed, Fremd-Review 2 M-1 of W02-08); revoked ⇒
// nil; database error ⇒ error.
func TestReauthConsumer_Integration(t *testing.T) {
	e := madSetup(t)
	ctx := context.Background()
	_, k, key := e.consumer(t, store.ModelConsumerSpec{AllowedCIDRs: []string{"10.0.0.0/8"}})
	d, h, rec := e.door(t, "")
	if r := madDo(h, madReq{ip: "10.7.7.7", bearer: key.Plaintext()}); r.Code != http.StatusOK {
		t.Fatalf("first request: %d", r.Code)
	}
	reqCtx, ca := rec.ctx, rec.ca
	got, err := ReauthConsumer(reqCtx, ca)
	if err != nil || got == nil || got.ModelKeyID != ca.ModelKeyID || got.AllModels != ca.AllModels {
		t.Fatalf("live key: %+v err=%v, want the consumer back (the allowlist needs the original address)", got, err)
	}
	if _, err := e.seed.Exec(ctx, `UPDATE context_model_consumers SET all_models = NOT all_models WHERE id = $1::uuid`, ca.ConsumerID); err != nil {
		t.Fatalf("flip all_models: %v", err)
	}
	if got, err := ReauthConsumer(reqCtx, ca); err != nil || got == nil || got.AllModels == ca.AllModels {
		t.Fatalf("all_models flipped: %+v err=%v, want the consumer as it reads now (all_models %v)", got, err, !ca.AllModels)
	}
	if _, err := ReauthConsumer(ctx, ca); err == nil {
		t.Fatal("a context without the door: no error")
	}

	// Same semaphore: with its only slot held, the re-check waits out the
	// deadline and reports the door unavailable.
	e.settings.auth(func(a *oai.AuthSettings) { a.DBConcurrency = 1 })
	release, err := d.sem.acquire(ctx, 1)
	if err != nil {
		t.Fatalf("hold the slot: %v", err)
	}
	t0 := time.Now()
	_, err = ReauthConsumer(reqCtx, ca)
	waited := time.Since(t0)
	release()
	t.Logf("re-check with the slot held: err=%v after %v", err, waited.Round(time.Millisecond))
	if err == nil || waited < modelAuthDBDeadline/2 {
		t.Fatalf("re-check did not wait on the door's semaphore: err=%v waited=%v", err, waited)
	}

	if err := store.RevokeKey(ctx, e.seed, k.ID, store.ModelAccess{ServerAdmin: true}, e.stamp); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got, err := ReauthConsumer(reqCtx, ca); err != nil || got != nil {
		t.Fatalf("revoked key: %+v err=%v, want nil", got, err)
	}
	e.pool.Close()
	if got, err := ReauthConsumer(reqCtx, ca); err == nil || got != nil {
		t.Fatalf("closed pool: %+v err=%v, want an error", got, err)
	}
}

// Stage 7 — semaphore deadline, exhausted pool, database error: 503
// auth_unavailable, x-should-retry false, never 500.
func TestModelAuthUnavailable_Integration(t *testing.T) {
	e := madSetup(t)
	ctx := context.Background()
	_, _, key := e.consumer(t, store.ModelConsumerSpec{})
	d, h, _ := e.door(t, "")
	want := func(name string, r interface {
		Header() http.Header
	}, code int, body string) {
		t.Helper()
		t.Logf("%s: %d %s x-should-retry=%q", name, code, strings.TrimSpace(body), r.Header().Get("x-should-retry"))
		if code != http.StatusServiceUnavailable || !strings.Contains(body, ModelAuthCodeUnavailable) || r.Header().Get("x-should-retry") != "false" {
			t.Fatalf("%s: want 503 %s, x-should-retry false", name, ModelAuthCodeUnavailable)
		}
	}

	e.settings.auth(func(a *oai.AuthSettings) { a.DBConcurrency = 1 })
	release, err := d.sem.acquire(ctx, 1)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	r := madDo(h, madReq{bearer: key.Plaintext()})
	release()
	want("semaphore deadline", r, r.Code, r.Body.String())

	e.settings.auth(func(a *oai.AuthSettings) { a.DBConcurrency = 4 })
	var held []interface{ Release() }
	for i := 0; i < 20; i++ {
		c, err := e.pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("drain the pool: %v", err)
		}
		held = append(held, c)
	}
	r = madDo(h, madReq{bearer: key.Plaintext()})
	for _, c := range held {
		c.Release()
	}
	want("pool exhausted", r, r.Code, r.Body.String())

	e.pool.Close()
	r = madDo(h, madReq{bearer: key.Plaintext()})
	want("database error", r, r.Code, r.Body.String())
	if st := d.Stats(); st.SemaphoreTimeouts != 1 || st.DBErrors != 2 {
		t.Fatalf("counters: semaphore timeouts %d (want 1), db errors %d (want 2)", st.SemaphoreTimeouts, st.DBErrors)
	}
}

// Secret freedom of the logs (B15): after every outcome the door produces,
// no log line carries a key, a hint or a hash.
func TestModelAuthLogsSecretFree_Integration(t *testing.T) {
	e := madSetup(t)
	_, _, key := e.consumer(t, store.ModelConsumerSpec{})
	bad := forged(t, 1)[0]
	_, h, _ := e.door(t, "")
	ctxWithAR := context.WithValue(context.Background(), authResultKey, &auth.AuthResult{IsValid: true})
	madDo(h, madReq{bearer: key.Plaintext()})
	madDo(h, madReq{bearer: bad.Plaintext()})
	madDo(h, madReq{bearer: key.Plaintext(), hdr: map[string]string{"X-Forwarded-For": "1.2.3.4"}})
	r := httptestRequestWithContext(ctxWithAR, key.Plaintext())
	h.ServeHTTP(newDiscard(), r)
	e.pool.Close()
	madDo(h, madReq{bearer: key.Plaintext()})
	logs := e.logs.String()
	t.Logf("log lines:\n%s", logs)
	for _, k := range []interface {
		Plaintext() string
		Hash() string
		Hint() string
	}{key, bad} {
		hp := k.Hash()[:16]
		for _, needle := range []string{k.Plaintext(), k.Hash(), k.Hint(), hp, "ctxm_"} {
			if strings.Contains(logs, needle) {
				t.Fatalf("log carries %q", needle)
			}
		}
	}
}
