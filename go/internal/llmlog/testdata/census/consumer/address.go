// census-want: 1
package probe

import "github.com/GottZ/ctx/internal/llmlog"

func f(e *llmlog.Entry) *string { return &e.ConsumerID }
