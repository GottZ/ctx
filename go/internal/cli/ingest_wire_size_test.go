package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/ingest"
)

// `ctx ingest` cut its batches by chunk count only, and the server refuses a
// request body over ingest.MaxRequestBytes whole. Every chunk of a file
// carried the file's metadata.links then, so a note with many links made a
// batch over the limit out of few chunks, and the chunks of the other files in
// it were lost with it (#323). Since #329 only the first chunk carries them;
// chunks long by their content (paragraphs near 50 KB, text that JSON escapes)
// still make such a batch. The integration test in cmd/ctxd runs the command
// against the production router and its mounted body limit.

// cappedIngest is fakeIngest behind the body limit ctxd mounts POST
// /api/ingest with: a longer request is refused whole with 413, as
// HandleIngest refuses it. It records every request it is sent.
type cappedIngest struct {
	fakeIngest
	reqMu sync.Mutex
	reqs  []cappedRequest
}

// cappedRequest is one request as cappedIngest received it.
type cappedRequest struct {
	bytes   int    // the body length
	chunks  int    // its chunks, 0 for a refused request
	first   int    // the length of its first chunk as sent, 0 for a refused request
	refused bool   // over the limit
	body    []byte // the body, for an accepted request
}

func (c *cappedIngest) serveHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ingest.MaxRequestBytes))
	if err != nil {
		c.reqMu.Lock()
		c.reqs = append(c.reqs, cappedRequest{bytes: int(r.ContentLength), refused: true})
		c.reqMu.Unlock()
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "request body exceeds 10 MB cap"})
		return
	}
	var elems struct {
		Chunks []json.RawMessage `json:"chunks"`
	}
	_ = json.Unmarshal(raw, &elems)
	req := cappedRequest{bytes: len(raw), chunks: len(elems.Chunks), body: raw}
	if len(elems.Chunks) > 0 {
		req.first = len(elems.Chunks[0])
	}
	c.reqMu.Lock()
	c.reqs = append(c.reqs, req)
	c.reqMu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	c.fakeIngest.serveHTTP(w, r)
}

func (c *cappedIngest) requests() []cappedRequest {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	return append([]cappedRequest(nil), c.reqs...)
}

// hubNote is a note with n distinct wikilinks, 150 to a "## Part" section, so
// that it is cut into n/150 chunks, the first of which carries all n link
// targets in metadata.links (every one did before #329).
func hubNote(n int) string {
	var b strings.Builder
	b.WriteString("# Hub\n")
	for i := 0; i < n; i++ {
		if i%150 == 0 {
			fmt.Fprintf(&b, "\n## Part %d\n\n", i/150)
		}
		fmt.Fprintf(&b, "[[Linked note number %05d]] ", i)
	}
	b.WriteString("\n")
	return b.String()
}

// bigLinks is a paragraph of n wikilinks with targets of targetBytes bytes and
// a one-letter display text: in the chunk's content each is "x ", in
// metadata.links it is the whole target.
func bigLinks(n, targetBytes int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "[[%05d-%s|x]] ", i, strings.Repeat("t", targetBytes-6))
	}
	return b.String()
}

// heavyNote is a note of n "## Part" sections of 40 000 bytes of "<" each:
// n chunks of about 240 KB each on the wire, where JSON writes "<" as six
// bytes.
func heavyNote(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "## Part %d\n\n%s\n\n", i, strings.Repeat("<", 40_000))
	}
	return b.String()
}

// The five normal notes sort before the heavy note, so a batch of 50 by count
// carries them and 45 of its chunks of about 240 KB each, about 11 MB. (The
// test used the #323 hub note with 9 000 links until #329 took the links off
// all but its first chunk.) Every request now stays within the limit, every
// chunk is written, the run exits 0, and a batch is cut only where the next
// chunk would pass the limit or --batch.
func TestIngestCutsBatchesByWireSize(t *testing.T) {
	for _, workers := range []string{"1", "4"} {
		t.Run("workers_"+workers, func(t *testing.T) {
			vault := t.TempDir()
			normals := []string{"a-note-1.md", "a-note-2.md", "a-note-3.md", "a-note-4.md", "a-note-5.md"}
			for _, rel := range normals {
				writeIngestFixture(t, vault, rel, ingestNote("MARKER-"+rel))
			}
			writeIngestFixture(t, vault, "heavy.md", heavyNote(60))
			c := &cappedIngest{}
			srv := httptest.NewServer(http.HandlerFunc(c.serveHTTP))
			defer srv.Close()

			stderr, err := runIngestCmd(t, srv.URL, vault, "--category", "wiresize", "--workers", workers)

			for _, rel := range normals {
				if n := c.acceptedFrom(rel); n != 1 {
					t.Errorf("%s: %d chunks written, want 1 — the note with long chunks took its batch down", rel, n)
				}
			}
			if n := c.acceptedFrom("heavy.md"); n != 60 {
				t.Errorf("heavy.md: %d chunks written, want 60", n)
			}
			if err != nil {
				t.Errorf("run returned %v, want nil: every chunk was written", err)
			}
			wantInReport(t, stderr, "Failed:   0")
			reqs := c.requests()
			for i, r := range reqs {
				if r.refused || r.bytes > ingest.MaxRequestBytes {
					t.Errorf("request %d: %d bytes, refused=%v; want every request within %d", i, r.bytes, r.refused, ingest.MaxRequestBytes)
				}
			}
			if workers != "1" {
				return
			}
			// One worker sends the batches in order: each one ends where the
			// next chunk, with its comma, would take it past the limit, or at
			// --batch (50).
			for i := 0; i+1 < len(reqs); i++ {
				if reqs[i].chunks != 50 && reqs[i].bytes+1+reqs[i+1].first <= ingest.MaxRequestBytes {
					t.Errorf("request %d: %d chunks, %d bytes, cut although the next chunk (%d bytes) fits",
						i, reqs[i].chunks, reqs[i].bytes, reqs[i+1].first)
				}
			}
			t.Logf("%d requests: %+v", len(reqs), sizes(reqs))
		})
	}
}

func sizes(reqs []cappedRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, fmt.Sprintf("%d chunks/%d B", r.chunks, r.bytes))
	}
	return out
}

// A chunk whose request alone is over the limit cannot be sent in any batch.
// Its file is held back before the upload, whole, like a file with a chunk
// over a per-chunk limit, and reported with the size and the limit; the other
// files are written and the run exits 1. first_of_two: the first chunk of the
// file does not fit a request (it carries the file's links), the second does
// (40 000 bytes of "<", six bytes each on the wire); the second is not sent
// either. Until #329 the case was second_of_two, links of 10.3 MB that the
// second chunk repeated: TestIngestUploadsNoteWhoseListsFitTheFirstChunk.
func TestIngestHoldsBackChunkOverRequestLimit(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		chunk string // the chunk the report names
	}{
		{"single_chunk", "# Giant\n\n" + bigLinks(1100, 10_000) + "\n", "chunk 1 of 1"},
		{"first_of_two", "# Giant\n\n" + bigLinks(1100, 10_000) + "\n\n## Escaped\n\n" + strings.Repeat("<", 40_000) + "\n", "chunk 1 of 2"},
	}
	for _, tc := range cases {
		for _, dry := range []bool{false, true} {
			name := tc.name
			if dry {
				name += "_dry_run"
			}
			t.Run(name, func(t *testing.T) {
				vault := t.TempDir()
				normals := []string{"a-note.md", "z-note.md"}
				for _, rel := range normals {
					writeIngestFixture(t, vault, rel, ingestNote("MARKER-"+rel))
				}
				writeIngestFixture(t, vault, "giant.md", tc.body)
				c := &cappedIngest{}
				srv := httptest.NewServer(http.HandlerFunc(c.serveHTTP))
				defer srv.Close()

				args := []string{"--category", "wiresize"}
				if dry {
					args = append(args, "--dry-run")
				}
				stderr, err := runIngestCmd(t, srv.URL, vault, args...)

				if err == nil {
					t.Errorf("run returned nil, want an error: giant.md was not ingested")
				}
				wantInReport(t, stderr, "giant.md: not uploaded: "+tc.chunk+": the request for this chunk alone is ",
					"bytes, the server accepts at most "+strconv.Itoa(ingest.MaxRequestBytes))
				if n := c.acceptedFrom("giant.md"); n != 0 {
					t.Errorf("giant.md: %d chunks written, want 0 — the file is held back whole", n)
				}
				for _, r := range c.requests() {
					if r.refused {
						t.Errorf("a request of %d bytes was sent over the limit", r.bytes)
					}
				}
				if dry {
					if n := len(c.requests()); n != 0 {
						t.Errorf("dry run sent %d requests", n)
					}
					return
				}
				for _, rel := range normals {
					if n := c.acceptedFrom(rel); n != 1 {
						t.Errorf("%s: %d chunks written, want 1", rel, n)
					}
				}
			})
		}
	}
}

// A vault in which no batch comes near the limit is sent as before: every
// request but the last carries exactly --batch chunks, in order.
func TestIngestBatchesBelowLimitByCount(t *testing.T) {
	vault := t.TempDir()
	for i := 0; i < 120; i++ {
		body := ingestNote(fmt.Sprintf("MARKER-%03d", i))
		if i%10 == 0 {
			body = repeatedSections(fmt.Sprintf("Part %d [[Some link %d]]", i, i), 3)
		}
		writeIngestFixture(t, vault, fmt.Sprintf("n-%03d.md", i), body)
	}
	for _, batch := range []int{50, 7} {
		t.Run(strconv.Itoa(batch), func(t *testing.T) {
			c := &cappedIngest{}
			srv := httptest.NewServer(http.HandlerFunc(c.serveHTTP))
			defer srv.Close()

			if _, err := runIngestCmd(t, srv.URL, vault, "--category", "count", "--batch", strconv.Itoa(batch)); err != nil {
				t.Fatalf("run: %v", err)
			}
			reqs := c.requests()
			total := 0
			for i, r := range reqs {
				total += r.chunks
				if i < len(reqs)-1 && r.chunks != batch {
					t.Errorf("request %d: %d chunks, want %d", i, r.chunks, batch)
				}
			}
			if last := reqs[len(reqs)-1].chunks; last < 1 || last > batch {
				t.Errorf("last request: %d chunks, want 1 to %d", last, batch)
			}
			if n := len(c.accepted); n != total {
				t.Errorf("%d chunks accepted, %d sent", n, total)
			}
		})
	}
}

// wireFixture is the payloads of a file whose text needs every kind of escape
// json.Marshal writes: `<`, `>` and `&` (six bytes each), quotes, backslashes,
// control characters, invalid UTF-8 (`\ufffd`), U+2028, and multi-byte runes,
// with links, dates and tags in its metadata.
func wireFixture(t *testing.T) []ingestChunkPayload {
	t.Helper()
	para := "a <b> & \"c\" \\ d\te\x01 \xff\xfe é 漢 \u2028 [[Link <1> & \"x\"]] [[Ref\\2]] 2026-11-17 #tag/x "
	var b strings.Builder
	b.WriteString("---\ntags: [alpha, beta]\ndate: 2026-01-02\n---\n# Escapes\n\n")
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "## Part %d <&>\n\n%s\n\n", i, strings.Repeat(para, 30))
	}
	parsed := ingest.Parse("esc", []byte(b.String()), "dir/esc <&>.md")
	chunks := ingest.Chunk("dir/esc <&>", parsed.Content, parsed.Tags)
	payloads := filePayloads("dir/esc <&>.md", "wire", parsed, chunks)
	if len(payloads) < 4 || len(parsed.Links) == 0 || len(parsed.Dates) == 0 || len(parsed.Tags) == 0 {
		t.Fatalf("fixture: %d chunks, %d links, %d dates, %d tags; want several of each",
			len(payloads), len(parsed.Links), len(parsed.Dates), len(parsed.Tags))
	}
	c := payloads[0].Content
	if utf8.ValidString(c) || !strings.Contains(c, "\u2028") || !strings.Contains(c, "<b>") || !strings.Contains(c, "\x01") {
		t.Fatalf("fixture: the chunk content lost an escape case: %.120q", c)
	}
	return payloads
}

// The length the CLI counts for a request is the length of the body
// postIngest marshals, byte for byte, with and without --scope.
func TestIngestRequestLengthIsTheSentBody(t *testing.T) {
	payloads := wireFixture(t)
	for _, scope := range []string{"", "team <a>&b"} {
		for n := 1; n <= len(payloads); n++ {
			batch := payloads[:n]
			want, err := json.Marshal(ingestRequest(scope, batch))
			if err != nil {
				t.Fatal(err)
			}
			got := requestOverhead(scope) + n - 1
			for _, p := range batch {
				got += p.wireBytes
			}
			if got != len(want) {
				t.Errorf("scope %q, %d chunks: counted %d bytes, the body is %d", scope, n, got, len(want))
			}
		}
	}
}

// splitIntoBatches against a small limit: every batch's body is within it, a
// batch of exactly the limit stays one batch, one byte less cuts it, and the
// chunks keep their order.
func TestSplitIntoBatchesByBytes(t *testing.T) {
	payloads := wireFixture(t)
	const scope = "team"
	overhead := requestOverhead(scope)
	bodyLen := func(batch []ingestChunkPayload) int {
		b, err := json.Marshal(ingestRequest(scope, batch))
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	exact := bodyLen(payloads[:3])

	cases := []struct {
		name     string
		maxBytes int
		first    int // chunks in the first batch
	}{
		{"exactly_three", exact, 3},
		{"one_byte_less", exact - 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batches := splitIntoBatches(payloads, 50, overhead, tc.maxBytes)
			if got := len(batches[0]); got != tc.first {
				t.Errorf("first batch: %d chunks, want %d", got, tc.first)
			}
			var order []int
			for i, b := range batches {
				if l := bodyLen(b); l > tc.maxBytes {
					t.Errorf("batch %d: %d bytes, over %d", i, l, tc.maxBytes)
				}
				if i+1 < len(batches) {
					if l := bodyLen(append(append([]ingestChunkPayload(nil), b...), batches[i+1][0])); l <= tc.maxBytes {
						t.Errorf("batch %d cut although the next chunk fits (%d bytes)", i, l)
					}
				}
				for _, p := range b {
					order = append(order, p.ChunkIndex)
				}
			}
			for i, idx := range order {
				if idx != payloads[i].ChunkIndex || len(order) != len(payloads) {
					t.Fatalf("chunk order %v, want the input order", order)
				}
			}
		})
	}
	// The count still caps a batch whose bytes would allow more.
	if got := len(splitIntoBatches(payloads, 2, overhead, 1<<30)[0]); got != 2 {
		t.Errorf("batch size 2: first batch has %d chunks", got)
	}
}

// paddedChunk is a one-chunk file whose request, with overhead bytes of
// envelope, is exactly total bytes.
func paddedChunk(t *testing.T, overhead, total int) []ingestChunkPayload {
	t.Helper()
	p := ingestChunkPayload{Category: "c", Title: "t", Content: "x", Metadata: map[string]any{"pad": ""}}
	p.Metadata = map[string]any{"pad": strings.Repeat("a", total-overhead-encodedLen(p))}
	p.wireBytes = encodedLen(p)
	if overhead+p.wireBytes != total {
		t.Fatalf("fixture: request of %d bytes, want %d", overhead+p.wireBytes, total)
	}
	return []ingestChunkPayload{p}
}

// A chunk whose request is exactly the limit is sent; one byte more holds its
// file back, also where the chunk without the request's envelope would fit,
// with and without --scope.
func TestLimitBreachAtRequestLimit(t *testing.T) {
	for _, scope := range []string{"", "team"} {
		overhead := requestOverhead(scope)
		if b := limitBreach(paddedChunk(t, overhead, ingest.MaxRequestBytes), overhead); b != "" {
			t.Errorf("scope %q: a request of exactly %d bytes is held back: %s", scope, ingest.MaxRequestBytes, b)
		}
		over := paddedChunk(t, overhead, ingest.MaxRequestBytes+1)
		if over[0].wireBytes > ingest.MaxRequestBytes {
			t.Fatalf("fixture: the chunk alone is over the limit")
		}
		want := fmt.Sprintf("chunk 1 of 1: the request for this chunk alone is %d bytes, the server accepts at most %d",
			ingest.MaxRequestBytes+1, ingest.MaxRequestBytes)
		if b := limitBreach(over, overhead); b != want {
			t.Errorf("scope %q: one byte over the limit gives %q, want %q", scope, b, want)
		}
	}
}

// splitIntoBatches over random chunk sizes, limits and --batch values: every
// batch is within --batch and the limit, a batch is cut only where the next
// chunk would pass one of them, every chunk is in exactly one batch, in order.
// Each trial cuts several times, so the envelope of every later batch counts.
func TestSplitIntoBatchesProperty(t *testing.T) {
	rnd := rand.New(rand.NewPCG(323, 1))
	for trial := 0; trial < 500; trial++ {
		scope := []string{"", "team", "t<&>"}[trial%3]
		overhead := requestOverhead(scope)
		chunks := make([]ingestChunkPayload, 1+rnd.IntN(60))
		largest := 0
		for i := range chunks {
			p := ingestChunkPayload{Category: "c", Title: fmt.Sprintf("t%d", i), Content: strings.Repeat("a<", rnd.IntN(200)), ChunkIndex: i}
			p.wireBytes = encodedLen(p)
			largest = max(largest, p.wireBytes)
			chunks[i] = p
		}
		maxBytes := overhead + largest + rnd.IntN(2000)
		batchSize := 1 + rnd.IntN(20)
		bodyLen := func(batch []ingestChunkPayload) int {
			b, err := json.Marshal(ingestRequest(scope, batch))
			if err != nil {
				t.Fatal(err)
			}
			return len(b)
		}
		batches := splitIntoBatches(chunks, batchSize, overhead, maxBytes)
		next := 0
		for i, b := range batches {
			if len(b) == 0 || len(b) > batchSize {
				t.Fatalf("trial %d: batch %d has %d chunks, --batch %d", trial, i, len(b), batchSize)
			}
			if l := bodyLen(b); l > maxBytes {
				t.Fatalf("trial %d: batch %d is %d bytes, over %d", trial, i, l, maxBytes)
			}
			if i+1 < len(batches) && len(b) < batchSize {
				if l := bodyLen(append(append([]ingestChunkPayload(nil), b...), batches[i+1][0])); l <= maxBytes {
					t.Fatalf("trial %d: batch %d cut although the next chunk fits (%d of %d bytes)", trial, i, l, maxBytes)
				}
			}
			for _, p := range b {
				if p.ChunkIndex != next {
					t.Fatalf("trial %d: chunk %d where %d belongs", trial, p.ChunkIndex, next)
				}
				next++
			}
		}
		if next != len(chunks) {
			t.Fatalf("trial %d: %d of %d chunks in the batches", trial, next, len(chunks))
		}
	}
}

// A proxy in front of the server refuses a long request with 413 in its own
// format. Before, the CLI reported the answer as an undecodable body
// ("decode response (status 413): invalid character '<' …"); the report now
// says the request was too large and how large it was.
func TestIngestNamesARefusalAsTooLarge(t *testing.T) {
	vault := t.TempDir()
	writeIngestFixture(t, vault, "note.md", ingestNote("proxy"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, "<html><body><h1>413 Request Entity Too Large</h1></body></html>")
	}))
	defer srv.Close()

	stderr, err := runIngestCmd(t, srv.URL, vault, "--category", "proxy")

	if err == nil {
		t.Errorf("run returned nil, want an error")
	}
	wantInReport(t, stderr, "HTTP 413", "too large", " bytes")
	if strings.Contains(stderr, "decode response") {
		t.Errorf("stderr reports the refusal as an undecodable answer:\n%s", stderr)
	}
}
