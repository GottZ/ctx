// census-want: 2
package probe

import "github.com/GottZ/ctx/internal/llmlog"

type row = llmlog.Entry

type own llmlog.Entry

var a = row{ConsumerID: "c"}

var b = []own{{ConsumerID: "c"}}
