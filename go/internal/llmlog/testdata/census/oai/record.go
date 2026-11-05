// census-want: 2
package oai

import (
	"github.com/GottZ/ctx/internal/llmlog"
)

func f() {
	llmlog.Record(nil, llmlog.Entry{})
	rec := llmlog.Record
	_ = rec
}
