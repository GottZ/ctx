// census-want: 2
package oai

import "github.com/GottZ/ctx/internal/llmlog"

func f(e *llmlog.Entry) llmlog.Entry {
	e.APIKeyID = "k"
	return llmlog.Entry{APIKeyID: "k"}
}
