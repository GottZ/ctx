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

	"github.com/GottZ/ctx/internal/ingest"
	"github.com/spf13/cobra"
)

// ── ingest types ─────────────────────────────────────────────────────

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

// ── command ──────────────────────────────────────────────────────────

// IngestCmd creates the `ctx ingest` Cobra command.
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

Use --dry-run to preview what would be uploaded without sending anything.`,
		Example: `  ctx ingest /path/to/vault/
  ctx ingest /path/to/vault/ --dry-run
  ctx ingest /path/to/vault/ --workers 4 --batch 100
  ctx ingest /path/to/vault/ --category reference --scope shared`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			// Phase 1: Scan + Parse + Chunk
			Errorf("ctx ingest: scanning %s", vaultPath)

			files, err := collectMarkdownFiles(vaultPath)
			if err != nil {
				return fmt.Errorf("scan vault: %w", err)
			}

			if len(files) == 0 {
				Errorf("  No .md files found.")
				return nil
			}

			stats := &ingestStats{
				categories: make(map[string]int),
			}
			var allChunks []ingestChunkPayload
			var errors []ingestError

			for i, filePath := range files {
				relPath, _ := filepath.Rel(vaultPath, filePath)
				relPath = filepath.ToSlash(relPath) // normalize to forward slashes

				content, err := os.ReadFile(filePath)
				if err != nil {
					errors = append(errors, ingestError{File: relPath, Error: err.Error()})
					continue
				}

				stats.totalFiles++
				fileInfo, _ := os.Stat(filePath)
				fileSize := fileInfo.Size()

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
				chunks := ingest.Chunk(filename, parsed.Content, parsed.Tags)
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

				// Build payloads
				for _, ch := range chunks {
					tags := parsed.Tags
					if tags == nil {
						tags = []string{}
					}

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

					allChunks = append(allChunks, ingestChunkPayload{
						Category:   cat,
						Title:      ch.Title,
						Content:    ch.Header + "\n\n" + ch.Content,
						Tags:       tags,
						Metadata:   meta,
						ChunkIndex: ch.Index,
					})

					stats.categories[cat]++
				}

				stats.totalChunks += len(chunks)

				// Progress every 100 files
				if (i+1)%100 == 0 {
					Errorf("  Parsed %d/%d files, %d chunks", i+1, len(files), stats.totalChunks)
				}
			}

			Errorf("  Parsed %d/%d files, %d chunks", stats.totalFiles, len(files), stats.totalChunks)

			// Print errors if any
			if len(errors) > 0 {
				Errorf("\n  Errors (%d):", len(errors))
				for _, e := range errors {
					Errorf("    %s: %s", e.File, e.Error)
				}
			}

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
				return nil
			}

			// Phase 2: Dry run or upload
			if dryRun {
				Errorf("\nDry run complete. Use 'ctx ingest %s' to upload.", args[0])
				return nil
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			// Build API base URL (strip /webhook suffix)
			apiBase := c.BaseURL
			apiBase = strings.TrimSuffix(apiBase, "/webhook")
			apiBase = strings.TrimSuffix(apiBase, "/")

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
						if extract {
							// Signal server to enable LLM extraction
							// (handled via source field or separate flag in future)
						}

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

			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Parse and chunk only, don't upload")
	cmd.Flags().IntVar(&workers, "workers", 1, "Parallel upload workers")
	cmd.Flags().IntVar(&batch, "batch", 50, "Chunks per batch")
	cmd.Flags().StringVar(&category, "category", "", "Override category (default: from folder path)")
	cmd.Flags().StringVar(&scope, "scope", "private", "Scope for uploaded chunks")
	cmd.Flags().BoolVar(&extract, "extract", false, "Enable LLM extraction on server")

	return cmd
}

// ── helpers ──────────────────────────────────────────────────────────

// collectMarkdownFiles walks a directory and returns all .md files,
// skipping .obsidian/, dot-directories, and node_modules.
func collectMarkdownFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			name := d.Name()
			// Skip hidden directories and known noise
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".md") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
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

// postIngest sends a batch to POST /api/ingest and parses the response.
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
	defer resp.Body.Close()

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

	if !result.Success {
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
