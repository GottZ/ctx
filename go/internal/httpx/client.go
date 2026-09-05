package httpx

import (
	"net/http"
	"time"
)

// PooledClient builds the keep-alive pool the three internal backend paths
// (chat, embed, rerank) use for their wire calls. It carries NO client-side
// timeout on purpose: every call site sets its own ctx deadline (llm's
// ChatTimeout family, rerank.Timeout, embed's caller budget), and a second
// deadline on the client would silently cap the shorter one. MaxIdleConns 20 /
// MaxIdleConnsPerHost 10 / IdleConnTimeout 90s are the values all three copies
// carried verbatim before they were merged here.
//
// This is a CONSTRUCTOR, not a singleton: each caller keeps its own pool, so
// idle connections stay separated per backend family exactly as before. One
// shared instance would pool connections across chat/embed/rerank against the
// same hosts — a change in connection behaviour, not a de-duplication. If that
// is ever wanted, it can be introduced here, in one place.
func PooledClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}
