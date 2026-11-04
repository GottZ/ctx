// census-want: 2
package llmlog

type Entry struct{ ConsumerID, APIKeyID string }

// StampGateway is the one sanctioned setter.
func (e *Entry) StampGateway(consumerID string) { e.ConsumerID = consumerID }

func (e *Entry) other() { e.ConsumerID = "x" }

var z = Entry{ConsumerID: "y"}
