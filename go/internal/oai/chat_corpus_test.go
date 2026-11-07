package oai_test

// The corpus recorder of the FOLGE-P differential (gate 2): with
// CTX_FOLGEP_CORPUS=<file> every body a route test posts through chatRig.post
// is appended to <file> (one JSON string per line, bodies over 64 KiB left
// out) — the source of testdata/chat_parse_corpus.jsonl, which the
// differential replays against the base parse. Regenerate with
//
//	CTX_FOLGEP_CORPUS=$PWD/internal/oai/testdata/chat_parse_corpus.raw go test -short -count=1 ./internal/oai/
//
// and sort -u the file into the .jsonl.

import (
	"encoding/json"
	"os"
	"sync"
)

var corpusMu sync.Mutex

func recordCorpus(body string) {
	path := os.Getenv("CTX_FOLGEP_CORPUS")
	if path == "" || len(body) > 64<<10 {
		return
	}
	line, err := json.Marshal(body)
	if err != nil {
		return
	}
	corpusMu.Lock()
	defer corpusMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
