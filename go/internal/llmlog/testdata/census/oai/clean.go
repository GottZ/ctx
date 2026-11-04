// census-want: 0
package oai

import "github.com/GottZ/ctx/internal/llmlog"

func f(r llmlog.GatewayRow) llmlog.Entry {
	llmlog.RecordGateway(nil, r)
	return llmlog.GatewayEntry(r)
}
