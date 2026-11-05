// census-want: 2
package probe

import "github.com/GottZ/ctx/internal/llmlog"

var es = []llmlog.Entry{{ConsumerID: "c"}, {Model: "m"}}

var ps = map[string]*llmlog.Entry{"a": {ConsumerID: "c"}}
