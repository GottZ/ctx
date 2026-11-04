// census-want: 2
package probe

import "github.com/GottZ/ctx/internal/llmlog"

func f(e *llmlog.Entry, c string) {
	e.ConsumerID = c
	e.Model, e.ConsumerID = "m", c
}
