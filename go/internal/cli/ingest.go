package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/ingest"
	"github.com/spf13/cobra"
)

// ── ingest types ─────────────────────────────────────────────────────.

// ingestChunkPayload is a single chunk sent to the server.
type ingestChunkPayload struct {
	Category   string         `json:"category"`
	Title      string         `json:"title"`
	Content    string         `json:"content"`
	Tags       []string       `json:"tags,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	SourceID   string         `json:"source_id,omitempty"`
	ChunkIndex int            `json:"chunk_index"`
}

// ingestRequestPayload is the batch request to POST /api/ingest.
type ingestRequestPayload struct {
	Source string               `json:"source"`
	Scope  string               `json:"scope,omitempty"`
	Chunks []ingestChunkPayload `json:"chunks"`
}

// ingestResultPayload is a per-chunk result from the server.
type ingestResultPayload struct {
	Index  int    `json:"index"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status"` // inserted, updated, skipped, failed
	Error  string `json:"error,omitempty"`
}

// ingestResponsePayload is the server response for a batch.
type ingestResponsePayload struct {
	Success  bool                  `json:"success"`
	Total    int                   `json:"total"`
	Inserted int                   `json:"inserted"`
	Updated  int                   `json:"updated"`
	Skipped  int                   `json:"skipped"`
	Failed   int                   `json:"failed"`
	Results  []ingestResultPayload `json:"results"`
	Error    string                `json:"error,omitempty"`
}

// ingestStats collects pipeline statistics.
type ingestStats struct {
	totalFiles  int
	templates   int
	totalChunks int
	categories  map[string]int
	largestFile string
	largestSize int64
	largestCh   int
}

// ingestError records a per-file or per-chunk error.
type ingestError struct {
	File  string `json:"file"`
	Chunk int    `json:"chunk,omitempty"`
	Error string `json:"error"`
}

// ── command ──────────────────────────────────────────────────────────.

// IngestCmd creates the `ctx ingest` Cobra command.
//
//nolint:cyclop // complex CLI command with many flags and validation paths
func IngestCmd(getClient func() (*Client, error)) *cobra.Command {
	var (
		dryRun   bool
		workers  int
		batch    int
		category string
		scope    string
		extract  bool
	)

	cmd := &cobra.Command{
		Use:   "ingest <vault-path>",
		Short: "Ingest Obsidian vault into context store",
		Long: `Scan an Obsidian vault directory, parse and chunk all Markdown files,
and upload them in batches to the context store.

Use --dry-run to preview what would be uploaded without sending anything.

A file with a chunk the server refuses (title over 500 bytes, category over
100 bytes, content over 50 KB, an empty title or category, a category
reserved for derived blocks) is held back and reported; the other files are
uploaded. A directory that cannot be read is reported too. The command exits 1
when a file or directory was not ingested or a chunk was not written, a dry
run when the upload would leave one out.`,
		Example: `  ctx ingest /path/to/vault/
  ctx ingest /path/to/vault/ --dry-run
  ctx ingest /path/to/vault/ --workers 4 --batch 100
  ctx ingest /path/to/vault/ --category reference --scope shared`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// No worker would upload nothing and report nothing failed.
			if workers < 1 {
				return fmt.Errorf("--workers must be at least 1, got %d", workers)
			}
			// The server refuses a request over MaxChunksPerRequest whole (413).
			if batch < 1 || batch > ingest.MaxChunksPerRequest {
				return fmt.Errorf("--batch must be between 1 and %d, the server's limit per request, got %d",
					ingest.MaxChunksPerRequest, batch)
			}

			vaultPath, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolve path: %w", err)
			}

			// Validate vault path exists and is a directory
			info, err := os.Stat(vaultPath)
			if err != nil {
				return fmt.Errorf("vault path: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("vault path is not a directory: %s", vaultPath)
			}
			// WalkDir reads the root with Lstat and does not follow a symlink,
			// so a vault path that links to a directory would be walked as one
			// entry and give no file.
			vaultPath, err = filepath.EvalSymlinks(vaultPath)
			if err != nil {
				return fmt.Errorf("vault path: %w", err)
			}

			// Phase 1: Scan + Parse + Chunk
			Errorf("ctx ingest: scanning %s", vaultPath)

			files, errors, err := collectMarkdownFiles(vaultPath)
			if err != nil {
				return fmt.Errorf("scan vault: %w", err)
			}

			if len(files) == 0 {
				Errorf("  No .md files found.")
				printFileErrors(errors)
				return ingestIncomplete(cmd, dryRun, errors, 0, nil)
			}

			stats := &ingestStats{
				categories: make(map[string]int),
			}
			var allChunks []ingestChunkPayload

			for i, filePath := range files {
				relPath, _ := filepath.Rel(vaultPath, filePath)
				relPath = filepath.ToSlash(relPath) // normalize to forward slashes

				content, err := os.ReadFile(filePath)
				if err != nil {
					errors = append(errors, ingestError{File: relPath, Error: err.Error()})
					continue
				}

				stats.totalFiles++
				// The size of what was read and is ingested; a second Stat
				// would race a file that changes or disappears meanwhile.
				fileSize := int64(len(content))

				// Track largest file
				if fileSize > stats.largestSize {
					stats.largestSize = fileSize
					stats.largestFile = relPath
				}

				// Parse
				filename := strings.TrimSuffix(filepath.Base(filePath), ".md")
				parsed := ingest.Parse(filename, content, relPath)

				if parsed.IsTemplate {
					stats.templates++
					continue
				}

				// Chunk
				// Titles carry the vault-relative path, not the basename:
				// block-mode ingest upserts by (category, title, scope), so
				// docs/README.md would overwrite README.md under a shared title.
				chunks := ingest.Chunk(strings.TrimSuffix(relPath, ".md"), parsed.Content, parsed.Tags)
				if len(chunks) == 0 {
					continue // empty or negligible content
				}

				// Track largest by chunk count
				if len(chunks) > stats.largestCh {
					stats.largestCh = len(chunks)
					stats.largestFile = relPath
				}

				// Determine category
				cat := parsed.Category
				if category != "" {
					cat = category
				}

				payloads := filePayloads(relPath, cat, parsed, chunks)
				if breach := limitBreach(payloads); breach != "" {
					errors = append(errors, ingestError{File: relPath, Error: "not uploaded: " + breach})
					continue
				}
				allChunks = append(allChunks, payloads...)
				stats.categories[cat] += len(payloads)
				stats.totalChunks += len(chunks)

				// Progress every 100 files
				if (i+1)%100 == 0 {
					Errorf("  Parsed %d/%d files, %d chunks", i+1, len(files), stats.totalChunks)
				}
			}

			Errorf("  Parsed %d/%d files, %d chunks", stats.totalFiles, len(files), stats.totalChunks)

			printFileErrors(errors)

			// Print statistics
			Errorf("")
			Errorf("  Files:      %d", stats.totalFiles)
			Errorf("  Templates:  %d (skipped)", stats.templates)
			Errorf("  Chunks:     %d", stats.totalChunks)

			// Categories sorted by count (simple insertion sort)
			type catCount struct {
				name  string
				count int
			}
			cats := make([]catCount, 0, len(stats.categories))
			for name, count := range stats.categories {
				cats = append(cats, catCount{name, count})
			}
			for i := 1; i < len(cats); i++ {
				for j := i; j > 0 && cats[j].count > cats[j-1].count; j-- {
					cats[j], cats[j-1] = cats[j-1], cats[j]
				}
			}
			var catParts []string
			for _, c := range cats {
				catParts = append(catParts, fmt.Sprintf("%s(%d)", c.name, c.count))
			}
			Errorf("  Categories: %s", strings.Join(catParts, ", "))

			if stats.largestFile != "" {
				sizeKB := float64(stats.largestSize) / 1024
				Errorf("\n  Largest file: %s (%.1f KB, %d chunks)", stats.largestFile, sizeKB, stats.largestCh)
			}
			if stats.totalFiles > 0 {
				avg := float64(stats.totalChunks) / float64(stats.totalFiles-stats.templates)
				Errorf("  Average chunks/file: %.1f", avg)
			}

			if stats.totalChunks == 0 {
				Errorf("\nNo chunks to upload.")
				return ingestIncomplete(cmd, dryRun, errors, 0, nil)
			}

			// Phase 2: Dry run or upload
			if dryRun {
				Errorf("\nDry run complete. Use 'ctx ingest %s' to upload.", args[0])
				return ingestIncomplete(cmd, dryRun, errors, 0, nil)
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			apiBase := strings.TrimSuffix(c.BaseURL, "/")

			Errorf("\nctx ingest: uploading %d chunks to %s", len(allChunks), apiBase)

			// Split into batches
			batches := splitIntoBatches(allChunks, batch)

			// Upload with worker pool
			var (
				uploaded   int64
				inserted   int64
				updated    int64
				skipped    int64
				failed     int64
				uploadErrs []ingestError
				mu         sync.Mutex
				startTime  = time.Now()
			)

			batchCh := make(chan int, len(batches))
			for i := range batches {
				batchCh <- i
			}
			close(batchCh)

			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for batchIdx := range batchCh {
						batchChunks := batches[batchIdx]
						req := ingestRequestPayload{
							Source: "obsidian",
							Scope:  scope,
							Chunks: batchChunks,
						}
						// TODO: if extract, signal server to enable LLM extraction
						// (handled via source field or separate flag in future)

						resp, err := postIngest(c, apiBase+"/api/ingest", req)
						batchSize := int64(len(batchChunks))

						if err != nil {
							atomic.AddInt64(&failed, batchSize)
							mu.Lock()
							uploadErrs = append(uploadErrs, ingestError{
								File:  fmt.Sprintf("batch-%d", batchIdx),
								Error: err.Error(),
							})
							mu.Unlock()
						} else {
							atomic.AddInt64(&inserted, int64(resp.Inserted))
							atomic.AddInt64(&updated, int64(resp.Updated))
							atomic.AddInt64(&skipped, int64(resp.Skipped))
							atomic.AddInt64(&failed, int64(resp.Failed))
							if resp.Failed > 0 {
								mu.Lock()
								uploadErrs = append(uploadErrs, chunkFailures(batchIdx, batchChunks, resp)...)
								mu.Unlock()
							}
						}

						newUploaded := atomic.AddInt64(&uploaded, batchSize)
						total := int64(len(allChunks))
						pct := float64(newUploaded) / float64(total) * 100
						elapsed := time.Since(startTime).Seconds()
						rate := float64(newUploaded) / elapsed
						remaining := float64(total-newUploaded) / rate

						bar := progressBar(int(newUploaded), int(total), 20)
						Errorf("  [%s] %d/%d (%.1f%%) | %.1f chunks/s | ETA %s",
							bar, newUploaded, total, pct, rate, formatDuration(remaining))
					}
				}()
			}
			wg.Wait()

			elapsed := time.Since(startTime)

			// Upload errors
			if len(uploadErrs) > 0 {
				Errorf("\n  Upload errors (%d):", len(uploadErrs))
				for _, e := range uploadErrs {
					Errorf("    %s: %s", e.File, e.Error)
				}
			}

			Errorf("\nDone. %d chunks processed in %s.", len(allChunks), formatDuration(elapsed.Seconds()))
			Errorf("  Inserted: %d", atomic.LoadInt64(&inserted))
			Errorf("  Updated:  %d", atomic.LoadInt64(&updated))
			Errorf("  Skipped:  %d (identical content)", atomic.LoadInt64(&skipped))
			Errorf("  Failed:   %d", atomic.LoadInt64(&failed))

			return ingestIncomplete(cmd, false, errors, atomic.LoadInt64(&failed), uploadErrs)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Parse and chunk only, don't upload")
	cmd.Flags().IntVar(&workers, "workers", 1, "Parallel upload workers")
	cmd.Flags().IntVar(&batch, "batch", 50,
		fmt.Sprintf("Chunks per batch, 1 to %d (the server's limit per request)", ingest.MaxChunksPerRequest))
	cmd.Flags().StringVar(&category, "category", "", "Override category (default: from folder path)")
	cmd.Flags().StringVar(&scope, "scope", "private", "Scope for uploaded chunks (a scope belongs to one tenant)")
	cmd.Flags().BoolVar(&extract, "extract", false, "Enable LLM extraction on server")

	return cmd
}

// ── helpers ──────────────────────────────────────────────────────────.

// collectMarkdownFiles walks a directory and returns all .md files, skipping
// .obsidian/, dot-directories and node_modules, and the paths it could not
// read, relative to root (markdownWalk.visit).
func collectMarkdownFiles(root string) ([]string, []ingestError, error) {
	w := &markdownWalk{root: root}
	err := filepath.WalkDir(root, w.visit)
	return w.files, w.unreadable, err
}

// markdownWalk is the state of one vault scan.
type markdownWalk struct {
	root       string
	files      []string      // the .md files found, absolute
	unreadable []ingestError // the paths the walk could not read
}

// visit is the fs.WalkDirFunc of collectMarkdownFiles. A directory inside the
// vault whose name starts with "." (.obsidian, .git) and node_modules are
// skipped on purpose, on the first call, before WalkDir reads them, so they
// never report an error. A directory WalkDir could not read, wholly or in
// part, comes back in a second call with the error: files under it are
// missing from the run, so it is recorded like an unreadable file and the scan
// goes on with the entries WalkDir did read.
func (w *markdownWalk) visit(path string, d fs.DirEntry, err error) error {
	if err != nil {
		rel, relErr := filepath.Rel(w.root, path)
		if relErr != nil {
			rel = path
		}
		w.unreadable = append(w.unreadable, ingestError{
			File:  filepath.ToSlash(rel),
			Error: "could not be read, files under it may be missing: " + err.Error(),
		})
		return nil
	}
	if d.IsDir() {
		name := d.Name()
		// Skip hidden directories and known noise inside the vault; the vault
		// itself is walked whatever it is called (~/.notes).
		if path != w.root && (strings.HasPrefix(name, ".") || name == "node_modules") {
			return filepath.SkipDir
		}
		return nil
	}
	if strings.HasSuffix(d.Name(), ".md") {
		w.files = append(w.files, path)
	}
	return nil
}

// printFileErrors lists the paths a run did not ingest: unreadable files and
// directories, and files held back over a server limit.
func printFileErrors(errs []ingestError) {
	if len(errs) == 0 {
		return
	}
	Errorf("\n  Errors (%d):", len(errs))
	for _, e := range errs {
		Errorf("    %s: %s", e.File, e.Error)
	}
}

// filePayloads builds the upload payloads of one file's chunks.
func filePayloads(relPath, cat string, parsed *ingest.ParseResult, chunks []ingest.ChunkResult) []ingestChunkPayload {
	tags := parsed.Tags
	if tags == nil {
		tags = []string{}
	}
	payloads := make([]ingestChunkPayload, 0, len(chunks))
	for _, ch := range chunks {
		meta := map[string]any{
			"source":     "obsidian",
			"vault_path": relPath,
		}
		if len(parsed.Links) > 0 {
			meta["links"] = parsed.Links
		}
		if len(parsed.Dates) > 0 {
			meta["dates"] = parsed.Dates
		}
		payloads = append(payloads, ingestChunkPayload{
			Category:   cat,
			Title:      ch.Title,
			Content:    ch.Header + "\n\n" + ch.Content,
			Tags:       tags,
			Metadata:   meta,
			ChunkIndex: ch.Index,
		})
	}
	return payloads
}

// limitBreach names the first per-chunk limit of POST /api/ingest that one of
// a file's payloads breaks, "" when the server takes them all. The server
// refuses a whole request over one such chunk (validateIngestRequest in
// internal/handler/ingest.go), the chunks of every other file in it too, so
// the CLI holds that file back and reports it instead of sending it. The whole
// file stays out, not only the chunk: a document missing one section is
// harder to notice in retrieval than a document that is missing and reported.
// An empty content is not checked because it cannot occur: every payload
// carries the chunk's context header. Sizes are counted as the server decodes
// them (wireLen). A category the derived layer reserves is refused whole by
// the handler's claim gate (403) and is held back the same way; the check is
// the handler's own predicate, derived.IsReservedCategory.
func limitBreach(payloads []ingestChunkPayload) string {
	for _, p := range payloads {
		cat, title, content := wireLen(p.Category), wireLen(p.Title), wireLen(p.Content)
		var breach string
		switch {
		case strings.TrimSpace(p.Category) == "":
			breach = "the category is empty"
		case cat > ingest.MaxCategoryBytes:
			breach = fmt.Sprintf("the category is %d bytes, the server accepts at most %d", cat, ingest.MaxCategoryBytes)
		case derived.IsReservedCategory(p.Category):
			breach = fmt.Sprintf("the category %q is reserved for derived blocks, the server refuses it", p.Category)
		case strings.TrimSpace(p.Title) == "":
			breach = "the title is empty"
		case title > ingest.MaxTitleBytes:
			breach = fmt.Sprintf("the title is %d bytes, the server accepts at most %d", title, ingest.MaxTitleBytes)
		case content > ingest.MaxContentBytes:
			breach = fmt.Sprintf("the content is %d bytes, the server accepts at most %d", content, ingest.MaxContentBytes)
		default:
			continue
		}
		return fmt.Sprintf("chunk %d of %d: %s", p.ChunkIndex+1, len(payloads), breach)
	}
	return ""
}

// wireLen is the length in bytes of s as the server decodes it from the
// request: json.Marshal sends every byte that is not valid UTF-8 as U+FFFD,
// three bytes, so a Latin-1 file name, heading or paragraph is longer on the
// server than on disk. Valid UTF-8, the common case, costs one scan.
func wireLen(s string) int {
	if utf8.ValidString(s) {
		return len(s)
	}
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			n += utf8.RuneLen(utf8.RuneError)
		} else {
			n += size
		}
		i += size
	}
	return n
}

// chunkFailures names the chunks a partly successful batch did not write. The
// server answers such a batch with 200, success:false, failed > 0 and a failed
// result per unwritten chunk next to the written ones (docs/api.md, POST
// /api/ingest); each becomes an error naming the chunk's file. A body without
// such results still gets one error for the batch.
func chunkFailures(batchIdx int, chunks []ingestChunkPayload, resp *ingestResponsePayload) []ingestError {
	var out []ingestError
	for _, r := range resp.Results {
		if r.Status != "failed" || r.Index < 0 || r.Index >= len(chunks) {
			continue
		}
		ch := chunks[r.Index]
		file, _ := ch.Metadata["vault_path"].(string)
		out = append(out, ingestError{File: file, Error: fmt.Sprintf("chunk %d: %s", ch.ChunkIndex+1, r.Error)})
	}
	if len(out) == 0 {
		out = append(out, ingestError{
			File:  fmt.Sprintf("batch-%d", batchIdx),
			Error: fmt.Sprintf("server: %d chunk(s) failed", resp.Failed),
		})
	}
	return out
}

// ingestIncomplete is the command's result once the summary is on stderr: nil
// when every chunk was written, otherwise an error, which cmd/ctx/main.go turns
// into exit status 1 like the failure of any other command (docs/cli.md, exit
// codes). It counts the paths the run did not ingest (an unreadable file or
// directory, a file held back over a server limit) and the chunks the upload
// did not write, and names the first reason; a dry run names the paths the
// upload would not ingest.
func ingestIncomplete(cmd *cobra.Command, dryRun bool, fileErrs []ingestError, failedChunks int64, uploadErrs []ingestError) error {
	if len(fileErrs) == 0 && failedChunks == 0 {
		return nil
	}
	// The command was called right: cobra's usage text after the summary would
	// say otherwise and push the summary up and out of view.
	cmd.SilenceUsage = true
	reason := "see the errors above"
	switch {
	case len(fileErrs) > 0:
		reason = fileErrs[0].File + ": " + fileErrs[0].Error
	case len(uploadErrs) > 0:
		reason = uploadErrs[0].File + ": " + uploadErrs[0].Error
	}
	if dryRun {
		return fmt.Errorf("dry run: %d path(s) would not be ingested; first: %s", len(fileErrs), reason)
	}
	return fmt.Errorf("ingest incomplete: %d path(s) not ingested, %d chunk(s) not written; first: %s",
		len(fileErrs), failedChunks, reason)
}

// splitIntoBatches divides chunks into batches of the given size.
func splitIntoBatches(chunks []ingestChunkPayload, batchSize int) [][]ingestChunkPayload {
	if batchSize <= 0 {
		batchSize = 50
	}
	var batches [][]ingestChunkPayload
	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batches = append(batches, chunks[i:end])
	}
	return batches
}

// postIngest sends a batch to POST /api/ingest and parses the response. A 200
// with success:false and failed > 0 is the documented partial success, not a
// refusal: it is returned, and the caller counts it chunk by chunk.
func postIngest(c *Client, url string, req ingestRequestPayload) (*ingestResponsePayload, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Context-Key", c.Key)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result ingestResponsePayload
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := result.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("server: %s", msg)
	}

	if !result.Success && result.Failed == 0 {
		msg := result.Error
		if msg == "" {
			msg = "unknown error"
		}
		return nil, fmt.Errorf("server: %s", msg)
	}

	return &result, nil
}

// progressBar generates an ASCII progress bar.
func progressBar(current, total, width int) string {
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	filled := current * width / total
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// formatDuration formats seconds into a human-readable string.
func formatDuration(seconds float64) string {
	if seconds < 0 || seconds != seconds { // NaN check
		return "?"
	}
	if seconds < 60 {
		return fmt.Sprintf("%.0fs", seconds)
	}
	m := int(seconds) / 60
	s := int(seconds) % 60
	if m < 60 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh%02dm", h, m)
}
