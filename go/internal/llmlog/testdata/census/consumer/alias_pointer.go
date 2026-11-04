// census-want: 1
package probe

import ll "github.com/GottZ/ctx/internal/llmlog"

func f() *ll.Entry { return &ll.Entry{Model: "m", ConsumerID: "c"} }
