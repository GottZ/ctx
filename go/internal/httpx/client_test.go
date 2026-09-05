package httpx

import (
	"net/http"
	"testing"
	"time"
)

// TestPooledClientValues pins the transport values PooledClient inherited from
// the three former copies (embed, llm, rerank). They are a behaviour contract,
// not a preference: changing them changes connection reuse against the
// inference backends.
func TestPooledClientValues(t *testing.T) {
	c := PooledClient()

	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (every call site sets its own ctx deadline)", c.Timeout)
	}

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
	}
	if tr.MaxIdleConns != 20 {
		t.Errorf("MaxIdleConns = %d, want 20", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 10 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 10", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", tr.IdleConnTimeout)
	}
}

// TestPooledClientIsConstructor pins the constructor semantics: two calls yield
// two distinct clients with two distinct transports, so the callers keep
// separate idle pools — byte-equivalent to the three package-level vars that
// existed before. A singleton would silently merge the pools.
func TestPooledClientIsConstructor(t *testing.T) {
	a, b := PooledClient(), PooledClient()

	if a == b {
		t.Fatal("two calls returned the same *http.Client; PooledClient must not be a singleton")
	}
	if a.Transport == b.Transport {
		t.Error("two calls share one *http.Transport; the idle pools would be merged")
	}
}
