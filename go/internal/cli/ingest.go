package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

	// wireBytes is the chunk's length in the request body (encodedLen), set
	// by filePayloads; it is not sent.
	wireBytes int
}

// ingestRequestPayload is the batch request to POST /api/ingest.
type ingestRequestPayload struct {
	Source string `json:"source"`
	// TargetScope is --scope; empty leaves the field out, and the server
	// writes to the key's home scope. The request no longer carries `scope`:
	// no server read it, and none will, because every CLI before #322 sent
	// "private" there whether the user chose it or not.
	TargetScope string               `json:"target_scope,omitempty"`
	Chunks      []ingestChunkPayload `json:"chunks"`
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
	Success bool `json:"success"`
	// Scope is the scope the server wrote the batch to; a server before #322
	// leaves it out (and writes to the key's home scope).
	Scope    string                `json:"scope,omitempty"`
	Total    int                   `json:"total"`
	Inserted int                   `json:"inserted"`
	Updated  int                   `json:"updated"`
	Skipped  int                   `json:"skipped"`
	Failed   int                   `json:"failed"`
	Results  []ingestResultPayload `json:"results"`
	Error    string                `json:"error,omitempty"`
	Code     string                `json:"code,omitempty"` // machine code of a refusal (docs/api.md)
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
reserved for derived blocks, a request over 10 MB for that chunk alone) is
held back and reported; the other files are uploaded. A batch ends at --batch
chunks or before its request would pass the server's 10 MB limit, whichever
comes first. A directory that cannot be read is reported too. The command
exits 1 when a file or directory was not ingested or a chunk was not written,
a dry run when the upload would leave one out.

A symbolic link to a folder is followed, also out of the vault (listed under
"Followed out of the vault"), and its notes keep the path through the link
(title, category, vault_path), as a link to a note does. Each folder is
ingested once: through no link if it can be, else through the fewest links,
else through the link whose path sorts first. Every other way to a folder,
and a link to the vault or a folder that holds it, is listed under "Not
followed" and does not change the exit status. A link to a note is ingested
under its own path, as before, also when the note is reachable another way.
A link that leads nowhere counts like an unreadable file, unless its name has
an extension other than .md: that is taken for a missing attachment, which
ingest would not read, and only named under "Not followed".

The chunks go to the key's home scope, or to the scope --scope names. The key
must be allowed to write that scope (a scope belongs to one tenant), or the
server refuses the batch and the run sends no further one. The summary names
the scope the server wrote to. A server that does not name it ignores --scope
and writes to the key's home scope: unless --scope names that scope, the run
stops after the first batch and exits 1.`,
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

			scan, err := collectMarkdownFiles(vaultPath)
			if err != nil {
				return fmt.Errorf("scan vault: %w", err)
			}
			files, errors := scan.files, scan.unreadable

			if len(files) == 0 {
				Errorf("  No .md files found.")
				scan.printLinks()
				printFileErrors(errors)
				return ingestIncomplete(cmd, dryRun, errors, 0, nil)
			}

			stats := &ingestStats{
				categories: make(map[string]int),
			}
			var allChunks []ingestChunkPayload
			overhead := requestOverhead(scope)

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
				if breach := limitBreach(payloads, overhead); breach != "" {
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

			scan.printLinks()
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
				Errorf("  Scope:      %s", dryRunScope(scope))
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
			batches := splitIntoBatches(allChunks, batch, overhead, ingest.MaxRequestBytes)

			// Upload with worker pool
			var (
				uploaded   int64
				inserted   int64
				updated    int64
				skipped    int64
				failed     int64
				notSent    int64
				stopped    atomic.Bool // set by the first answer that refused or did not confirm --scope
				uploadErrs []ingestError
				written    = writtenScope{requested: scope, home: sync.OnceValue(func() string { return keyHomeScope(c) })}
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
						batchSize := int64(len(batchChunks))
						// Once an answer refused --scope or wrote elsewhere,
						// every further batch would do the same.
						if stopped.Load() {
							atomic.AddInt64(&notSent, batchSize)
							continue
						}
						req := ingestRequest(scope, batchChunks)
						// TODO: if extract, signal server to enable LLM extraction
						// (handled via source field or separate flag in future)

						resp, err := postIngest(c, apiBase+"/api/ingest", req)

						if err != nil {
							atomic.AddInt64(&failed, batchSize)
							if scopeDenied(err) {
								stopped.Store(true)
							}
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
							mu.Lock()
							if !written.add(resp.Scope) {
								stopped.Store(true)
							}
							if resp.Failed > 0 {
								uploadErrs = append(uploadErrs, chunkFailures(batchIdx, batchChunks, resp)...)
							}
							mu.Unlock()
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
			if n := atomic.LoadInt64(&notSent); n > 0 {
				Errorf("  Not sent: %d (the run stopped: the server refused --scope or did not confirm it)", n)
			}
			if line := written.line(); line != "" {
				Errorf("  Scope:    %s", line)
			}

			if err := written.unconfirmed(); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return ingestIncomplete(cmd, false, errors, atomic.LoadInt64(&failed)+atomic.LoadInt64(&notSent), uploadErrs)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Parse and chunk only, don't upload")
	cmd.Flags().IntVar(&workers, "workers", 1, "Parallel upload workers")
	cmd.Flags().IntVar(&batch, "batch", 50,
		fmt.Sprintf("Chunks per batch, 1 to %d (the server's limit per request); a batch ends earlier where its request would pass %d MB",
			ingest.MaxChunksPerRequest, ingest.MaxRequestBytes>>20))
	cmd.Flags().StringVar(&category, "category", "", "Override category (default: from folder path)")
	cmd.Flags().StringVar(&scope, "scope", "",
		"Scope to write the chunks to (default: the key's home scope); the key must be allowed to write it, a scope belongs to one tenant")
	cmd.Flags().BoolVar(&extract, "extract", false, "Enable LLM extraction on server")

	return cmd
}

// ── helpers ──────────────────────────────────────────────────────────.

// collectMarkdownFiles walks the vault at root, a resolved path, and returns
// the scan: the .md files found, the paths it could not read or follow, the
// links it followed out of the vault and the ways to a folder or note it did
// not take (markdownWalk). A file is named by its path through the vault:
// under a linked folder, the path through the link, which the caller reads and
// derives the vault path, title and category from.
//
// A symbolic link to a folder is followed, also out of the vault. Every real
// folder is walked once, so a cycle ends and two links to one folder do not
// ingest it twice: the vault is walked first without following a link, then
// the links found there, sorted by their path in the vault, then the links
// found behind those, and so on. A folder reached without a link thus wins
// over every link to it, a way through fewer links over one through more, and
// among equal ones the link whose path sorts first (byte order), whatever
// order the file system lists a folder in.
func collectMarkdownFiles(root string) (*markdownWalk, error) {
	w := &markdownWalk{root: root}
	w.see(root, root)
	if err := w.walk(root, root); err != nil {
		return nil, err
	}
	for len(w.links) > 0 {
		round := w.links
		w.links = nil
		slices.SortFunc(round, func(a, b folderLink) int { return strings.Compare(a.shown, b.shown) })
		for _, l := range round {
			if err := w.follow(l); err != nil {
				return nil, err
			}
		}
	}
	w.noteDeadAttachments()
	return w, nil
}

// markdownWalk is the state of one vault scan.
type markdownWalk struct {
	root        string            // the vault, resolved
	base        string            // the folder being walked, resolved
	shown       string            // base's path through the vault (base itself for the vault)
	files       []string          // the .md files found, as paths through the vault
	unreadable  []ingestError     // the paths the walk could not read or follow
	followedOut []ingestError     // the links followed to a folder out of the vault
	notFollowed []ingestError     // the ways not taken to a folder or note found otherwise
	seen        map[string]string // every folder walked, resolved, to its path through the vault
	links       []folderLink      // the links to a folder found and not yet followed
	deadAttach  []string          // dead links named with an extension other than .md
}

// folderLink is a link to a folder: its path through the vault and the folder
// it resolves to.
type folderLink struct{ shown, target string }

// walk walks the resolved folder base, which appears in the vault as shown.
func (w *markdownWalk) walk(base, shown string) error {
	w.base, w.shown = base, shown
	return filepath.WalkDir(base, w.visit) //nolint:gosec // G703: the vault the user named and the folders its links lead to are what ingest reads
}

// visit is the fs.WalkDirFunc of markdownWalk.walk. A directory whose name
// starts with "." (.obsidian, .git) and node_modules are skipped on purpose,
// on the first call, before WalkDir reads them, so they never report an
// error; the folder walked is walked whatever it is called (~/.notes). A
// directory WalkDir could not read, wholly or in part, comes back in a second
// call with the error: files under it are missing from the run, so it is
// recorded like an unreadable file and the scan goes on with the entries
// WalkDir did read. Only a symbolic link costs a system call more (link); a
// directory costs a map entry.
func (w *markdownWalk) visit(path string, d fs.DirEntry, err error) error {
	shown := w.shownPath(path)
	if err != nil {
		w.unreadable = append(w.unreadable, ingestError{
			File:  w.rel(shown),
			Error: "could not be read, files under it may be missing: " + err.Error(),
		})
		return nil //nolint:nilerr // recorded above; the scan goes on with what WalkDir did read
	}
	switch {
	case d.IsDir():
		if path == w.base {
			return nil
		}
		if skippedFolder(d.Name()) {
			return filepath.SkipDir
		}
		// Under a link: a real folder walked before, through another way.
		if prev, ok := w.seen[path]; ok {
			w.skip(shown, path, prev)
			return filepath.SkipDir
		}
		w.see(path, shown)
	case d.Type()&fs.ModeSymlink != 0:
		w.link(path, shown, d.Name())
	case strings.HasSuffix(d.Name(), ".md"):
		w.files = append(w.files, shown)
	}
	return nil
}

// link sorts a symbolic link at path, named name, by what it leads to. A link
// to a folder is followed later (collectMarkdownFiles) unless its name is one
// a folder is skipped by; a link named .md to anything else is a note, read
// through the link and named by the link's path as before, also when the note
// is reachable another way (each way is its own block). A link that leads
// nowhere, or that cannot be resolved, is listed as a note when it is named
// .md (reading it reports why); otherwise it is recorded as unreadable when
// it may have led to a folder of notes, that is when its name has no
// extension. A dead link named with another extension (a missing image or
// PDF) does not count: ingest would not have read what it led to. It is still
// named in one summary line (noteDeadAttachments), as a dead link to a folder
// called like v2.0 is one.
func (w *markdownWalk) link(path, shown, name string) {
	info, err := os.Stat(path)
	if err == nil && !info.IsDir() {
		if strings.HasSuffix(name, ".md") {
			w.files = append(w.files, shown)
		}
		return
	}
	var target string
	if err == nil {
		if skippedFolder(name) {
			return
		}
		if target, err = filepath.EvalSymlinks(path); err == nil {
			w.links = append(w.links, folderLink{shown: shown, target: target})
			return
		}
	}
	switch {
	case strings.HasSuffix(name, ".md"):
		w.files = append(w.files, shown)
	case skippedFolder(name):
	case filepath.Ext(name) == "":
		w.unreadable = append(w.unreadable, ingestError{
			File:  w.rel(shown),
			Error: "could not be followed, files under it may be missing: " + err.Error(),
		})
	default:
		w.deadAttach = append(w.deadAttach, w.rel(shown))
	}
}

// follow walks the folder a link leads to, unless it is the vault or a folder
// that holds the vault (a way back, which would walk the vault again and what
// lies around it) or a folder walked before.
func (w *markdownWalk) follow(l folderLink) error {
	if within(w.root, l.target) {
		w.notFollowed = append(w.notFollowed, ingestError{
			File:  w.rel(l.shown) + " -> " + l.target,
			Error: "the vault or a folder that holds it",
		})
		return nil
	}
	if prev, ok := w.seen[l.target]; ok {
		w.skip(l.shown, l.target, prev)
		return nil
	}
	w.see(l.target, l.shown)
	if !within(l.target, w.root) {
		w.followedOut = append(w.followedOut, ingestError{File: w.rel(l.shown) + " -> " + l.target})
	}
	return w.walk(l.target, l.shown)
}

// noteDeadAttachments names the dead links taken for missing attachments in
// one line of the ways not taken: they do not count, and a vault with many of
// them would otherwise fill the summary on every run.
func (w *markdownWalk) noteDeadAttachments() {
	switch n := len(w.deadAttach); n {
	case 0:
	case 1:
		w.notFollowed = append(w.notFollowed, ingestError{File: w.deadAttach[0],
			Error: "leads nowhere, taken for a missing attachment (named with an extension other than .md)"})
	default:
		w.notFollowed = append(w.notFollowed, ingestError{File: fmt.Sprintf("%s and %d more", w.deadAttach[0], n-1),
			Error: "lead nowhere, taken for missing attachments (named with an extension other than .md)"})
	}
}

// printLinks prints the links followed out of the vault and the ways not
// taken; neither changes the exit status.
func (w *markdownWalk) printLinks() {
	printPathList("Followed out of the vault", w.followedOut)
	printPathList("Not followed", w.notFollowed)
}

// see records the resolved folder dir as walked, through the vault path shown.
func (w *markdownWalk) see(dir, shown string) {
	if w.seen == nil {
		w.seen = make(map[string]string)
	}
	w.seen[dir] = shown
}

// skip records the way shown to target, which was found through prev before.
func (w *markdownWalk) skip(shown, target, prev string) {
	w.notFollowed = append(w.notFollowed, ingestError{
		File:  w.rel(shown) + " -> " + target,
		Error: "already ingested through " + w.rel(prev),
	})
}

// shownPath is the path through the vault of path in the folder being walked.
func (w *markdownWalk) shownPath(path string) string {
	if w.base == w.shown {
		return path
	}
	return filepath.Join(w.shown, strings.TrimPrefix(path, w.base))
}

// rel is the vault-relative form of a path through the vault, with forward
// slashes, as the run reports it.
func (w *markdownWalk) rel(shown string) string {
	rel, err := filepath.Rel(w.root, shown)
	if err != nil {
		rel = shown
	}
	return filepath.ToSlash(rel)
}

// skippedFolder reports whether a folder of this name is skipped on purpose:
// a hidden one (.obsidian, .git) or node_modules.
func skippedFolder(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules"
}

// within reports whether path is dir or lies under it; both are resolved.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// printFileErrors lists the paths a run did not ingest: unreadable files and
// directories, links that cannot be followed, and files held back over a
// server limit.
func printFileErrors(errs []ingestError) {
	printPathList("Errors", errs)
}

// printPathList prints a summary section of paths and what became of them.
func printPathList(heading string, list []ingestError) {
	if len(list) == 0 {
		return
	}
	Errorf("\n  %s (%d):", heading, len(list))
	for _, e := range list {
		if e.Error == "" {
			Errorf("    %s", e.File)
			continue
		}
		Errorf("    %s: %s", e.File, e.Error)
	}
}

// filePayloads builds the upload payloads of one file's chunks. The file's
// links and dates go into the metadata of its first chunk only (noteList).
func filePayloads(relPath, cat string, parsed *ingest.ParseResult, chunks []ingest.ChunkResult) []ingestChunkPayload {
	tags := parsed.Tags
	if tags == nil {
		tags = []string{}
	}
	payloads := make([]ingestChunkPayload, 0, len(chunks))
	for i, ch := range chunks {
		meta := map[string]any{
			"source":     "obsidian",
			"vault_path": relPath,
		}
		noteList(meta, "links", parsed.Links, i == 0)
		noteList(meta, "dates", parsed.Dates, i == 0)
		p := ingestChunkPayload{
			Category:   cat,
			Title:      ch.Title,
			Content:    ch.Header + "\n\n" + ch.Content,
			Tags:       tags,
			Metadata:   meta,
			ChunkIndex: ch.Index,
		}
		p.wireBytes = encodedLen(p)
		payloads = append(payloads, p)
	}
	return payloads
}

// noteList puts a list that belongs to the whole note (its links, its dates)
// into the metadata of one of its chunks under key: the list on the note's
// first chunk, JSON null on every other chunk, nothing on any chunk when the
// note has no such list (#329). Before, every chunk carried the list, and a
// note with thousands of links repeated them with every chunk, in the request
// and in every stored block. The other chunks name the key with null, not
// leave it out, because the server merges the metadata of a chunk whose
// content is unchanged into the stored block (store.UpsertBlock): a missing
// key would keep the list such a block stored before, also on a chunk that
// has stopped being the first. A reader finds the note's lists on the chunk of
// its vault_path whose key holds an array (docs/cli.md).
func noteList(meta map[string]any, key string, list []string, first bool) {
	switch {
	case len(list) == 0:
	case first:
		meta[key] = list
	default:
		meta[key] = nil
	}
}

// ingestRequest is the body of POST /api/ingest for one batch.
func ingestRequest(scope string, chunks []ingestChunkPayload) ingestRequestPayload {
	return ingestRequestPayload{Source: "obsidian", TargetScope: scope, Chunks: chunks}
}

// requestOverhead is the length of the request body without a chunk
// (`{"source":"obsidian","chunks":[]}`, with target_scope when scope is set).
// json.Marshal writes a slice element as it writes the value alone, so a
// request of chunks c1…cn is requestOverhead + encodedLen(c1) + … +
// encodedLen(cn) + n-1 commas, byte for byte the body postIngest sends.
func requestOverhead(scope string) int {
	b, _ := json.Marshal(ingestRequest(scope, []ingestChunkPayload{})) // two strings and an empty slice cannot fail
	return len(b)
}

// byteCounter is an io.Writer that keeps only the count of what it is given.
type byteCounter int

func (c *byteCounter) Write(p []byte) (int, error) {
	*c += byteCounter(len(p))
	return len(p), nil
}

// encodedLen is the length of p as json.Marshal writes it into the request
// body (requestOverhead). The encoder writes into a counter, not into a new
// buffer, so measuring a chunk allocates no copy of it; it ends its output
// with a newline, which Marshal does not write. It escapes as Marshal does:
// `<`, `>` and `&` as six bytes, an invalid UTF-8 byte as `\ufffd`. A payload
// holds strings, string slices and an int, which the encoder cannot fail on;
// were it to, it would give 0, and postIngest's Marshal would fail the batch
// as it did before batches were measured.
func encodedLen(p ingestChunkPayload) int {
	var n byteCounter
	if err := json.NewEncoder(&n).Encode(p); err != nil {
		return 0
	}
	return int(n) - 1
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
// the handler's own predicate, derived.IsReservedCategory. A chunk whose
// request alone, overhead bytes plus the chunk (requestOverhead), passes the
// server's body limit cannot be sent in any batch, and is held back too: every
// chunk carries the file's tags and the first its links and dates, so a file
// with very many links can make one chunk of short content a request of
// several MB.
func limitBreach(payloads []ingestChunkPayload, overhead int) string {
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
		case overhead+p.wireBytes > ingest.MaxRequestBytes:
			breach = fmt.Sprintf("the request for this chunk alone is %d bytes, the server accepts at most %d",
				overhead+p.wireBytes, ingest.MaxRequestBytes)
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

// dryRunNotChecked is why a dry run names the scope without vouching for it:
// it does not contact the server, and only the server knows which scopes the
// key may write (GET /api/whoami lists the home and read scopes, not that
// set).
const dryRunNotChecked = "(not checked: a dry run does not contact the server)"

// dryRunScope is the scope line of a dry run.
func dryRunScope(requested string) string {
	if requested == "" {
		return "the key's home scope " + dryRunNotChecked
	}
	return requested + " " + dryRunNotChecked
}

// writtenScope collects the scope the server's answers name: the scope it
// wrote each batch to, the key's home scope without --scope (#322). A server
// before #322 names none and writes to the key's home scope whatever --scope
// says. The summary reports these answers, never the flag.
type writtenScope struct {
	requested  string        // --scope; "" asks for the key's home scope
	home       func() string // the key's home scope, asked at most once; "" when unknown
	answers    int           // batches the server answered with counts
	unreported int           // answers that named no scope
	scopes     []string      // the scopes named, each once
}

// add records an answer and reports whether it wrote to the requested scope.
func (s *writtenScope) add(scope string) bool {
	s.answers++
	if scope == "" {
		s.unreported++
	} else if !slices.Contains(s.scopes, scope) {
		s.scopes = append(s.scopes, scope)
	}
	return s.confirms(scope)
}

// confirms reports whether an answer naming scope ("" for none) wrote to the
// requested scope. An answer without a scope comes from a server that writes
// to the key's home scope, so it confirms --scope only when --scope names
// that scope; only then is the home scope asked for.
func (s *writtenScope) confirms(scope string) bool {
	switch {
	case s.requested == "":
		return true
	case scope == "":
		return s.home() == s.requested
	default:
		return scope == s.requested
	}
}

// line is the summary's scope line, "" when no batch was answered.
func (s *writtenScope) line() string {
	switch {
	case s.answers == 0:
		return ""
	case s.unreported > 0 && s.requested != "" && s.confirms(""):
		return s.requested + " (the key's home scope; the server did not report it)"
	case s.unreported > 0:
		return "not reported by the server (a server that does not report it writes to the key's home scope and ignores --scope)"
	default:
		return strings.Join(s.scopes, ", ")
	}
}

// unconfirmed is the run's error when --scope named a scope the answers do not
// confirm; nil without --scope or without an answer (a refused batch is
// counted and reported as such).
func (s *writtenScope) unconfirmed() error {
	if s.requested == "" || s.answers == 0 {
		return nil
	}
	if s.unreported > 0 && !s.confirms("") {
		return fmt.Errorf("--scope %q was not applied: the server did not report the scope it wrote to, "+
			"so the chunks went to the key's home scope", s.requested)
	}
	for _, sc := range s.scopes {
		if sc != s.requested {
			return fmt.Errorf("--scope %q was not applied: the server wrote to %s", s.requested, strings.Join(s.scopes, ", "))
		}
	}
	return nil
}

// keyHomeScope is the key's home scope as GET /api/whoami names it, "" when
// the server does not answer. ctx ingest asks it only when --scope is set and
// a server answered a batch without naming the scope it wrote to.
func keyHomeScope(c *Client) string {
	body, status, err := c.Do(http.MethodGet, "/api/whoami", nil)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var who struct {
		Success   bool   `json:"success"`
		HomeScope string `json:"home_scope"`
	}
	if json.Unmarshal(body, &who) != nil || !who.Success {
		return ""
	}
	return who.HomeScope
}

// scopeDeniedCode is the machine code of the server's refusal of a scope the
// key may not write (classScopeDenied in internal/handler/errcode.go,
// docs/api.md). Every further batch of the run would be refused the same way.
const scopeDeniedCode = "scope_denied"

// ingestRefusal is a batch the server refused whole: its message and the
// machine code of the answer ("" when the answer carries none).
type ingestRefusal struct {
	code, msg string
}

func (r *ingestRefusal) Error() string { return "server: " + r.msg }

// scopeDenied reports whether err is the server's refusal of the scope.
func scopeDenied(err error) bool {
	var r *ingestRefusal
	return errors.As(err, &r) && r.code == scopeDeniedCode
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

// splitIntoBatches cuts chunks, in order, into the batches of the upload: a
// batch ends before the chunk that would take it past batchSize chunks or its
// request past maxBytes, the server's body limit. A request is overhead bytes
// (requestOverhead) plus its chunks' wireBytes and a comma between two, so
// the count is the body postIngest sends; a request of exactly maxBytes is
// accepted. A chunk over maxBytes on its own still gets a batch of its own;
// ctx ingest holds its file back before (limitBreach). The batches are
// sub-slices of chunks; nothing is copied.
func splitIntoBatches(chunks []ingestChunkPayload, batchSize, overhead, maxBytes int) [][]ingestChunkPayload {
	if batchSize <= 0 {
		batchSize = 50
	}
	var batches [][]ingestChunkPayload
	start, size := 0, overhead
	for i := range chunks {
		n := chunks[i].wireBytes
		if i > start {
			n++ // the comma before it
			if i-start == batchSize || size+n > maxBytes {
				batches = append(batches, chunks[start:i])
				start, size, n = i, overhead, chunks[i].wireBytes
			}
		}
		size += n
	}
	if start < len(chunks) {
		batches = append(batches, chunks[start:])
	}
	return batches
}

// postIngest sends a batch to POST /api/ingest and parses the response. A 200
// with success:false and failed > 0 is the documented partial success, not a
// refusal: it is returned, and the caller counts it chunk by chunk. A 413
// whose answer names no reason, as a proxy in front of the server sends it
// (an HTML page), is reported as a request too large, with its size.
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
	decodeErr := json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode == http.StatusRequestEntityTooLarge && (decodeErr != nil || result.Error == "") {
		return nil, &ingestRefusal{msg: fmt.Sprintf(
			"HTTP 413: the request of %d bytes is too large for the server or a proxy in front of it", len(data))}
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode response (status %d): %w", resp.StatusCode, decodeErr)
	}

	if resp.StatusCode != http.StatusOK {
		msg := result.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, &ingestRefusal{code: result.Code, msg: msg}
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
