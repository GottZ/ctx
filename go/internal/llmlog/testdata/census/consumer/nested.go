// census-want: 2
package probe

import "github.com/GottZ/ctx/internal/llmlog"

var n = [][]llmlog.Entry{{{ConsumerID: "c"}}}

var p = map[string][]*llmlog.Entry{"a": {&llmlog.Entry{ConsumerID: "c"}}}
