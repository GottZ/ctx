// census-want: 0
package probe

import "github.com/GottZ/ctx/internal/llmlog"

type principal struct{ ConsumerID string }

// Reading the field, copying an entry and the stamp itself are no writes;
// another struct's ConsumerID in a literal is not llmlog.Entry's.
func f(e llmlog.Entry, g llmlog.Gateway) (string, llmlog.Entry, principal) {
	c := e.ConsumerID
	cp := e
	cp.StampGateway(c, g)
	return c, cp, principal{ConsumerID: c}
}
