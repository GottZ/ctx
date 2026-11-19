package oai

import "github.com/GottZ/ctx/internal/testalloc"

// EmbedRecordFrame is the frame of the function that books and writes the
// embeddings route's log row: the allocation pin of its text
// (TestEmbedLogRowTextIsBookedBeforeItIsWritten) sums what was allocated
// under it. Named through the method itself, so a rename breaks the build
// instead of leaving a frame no stack passes through.
var EmbedRecordFrame = testalloc.Frame((*embedCall).record)
