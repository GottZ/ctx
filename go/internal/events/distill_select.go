// distill_select.go — the selection stage of the distiller arm (design/02
// §4.2.5, §4.2.4 and §4.5.2, wave A02-6). It is the middle A02-5 left open:
// what a batch of foreign chunks has to survive before anything is spent on it,
// and what the run journal records about that.
//
// FOUR FILTERS, IN THIS ORDER, and the order is the design's:
//
//  1. Boilerplate. It is already gone when the arm sees an item — the reader
//     cuts at "## Direct transcript" (ctxcheckpoint/parse.go:80) and drops a
//     part without the marker entirely. Nothing is left to do here, and the
//     duplicate strip is deliberately NOT built: it would be a second place
//     that has to agree with the producing plugin. The gate suite asserts the
//     property on the selected chunks instead of re-implementing it.
//  2. Credentials, DROP and never mask — ctx has no redactor at all
//     (sensitivity.Scan answers Match{Kind,Reason}, §5 BA6). Two stages, see
//     distillSelect.
//  3. Minimum substance, distill.min_row_runes.
//  4. Cross-run dedup over distill_seen, keyed per CHUNK.
//
// THE DEDUP KEY IS THE CHUNK, NOT THE PART (§4.2.3, NA-10). The predecessor
// design hashed a head-capped part, which covered 11 % of a typical 36 000
// character body and marked the other 89 % as seen for good. A part whose first
// chunk was distilled before therefore still delivers its remaining chunks, and
// the gate probes exactly that.
//
// THE DUMP IS AN EGRESS CHANNEL (§5 BA13). While there is no LLM call, the
// selected chunks are written to distill.dryrun_dir so the wave is measurable
// at all — and that dump carries the material the detector did not catch. It
// therefore never lands in a git working copy, its directory is 0700, its file
// name is a run id and never foreign text, and an empty key turns it off.
//
// Source: https://github.com/GottZ/ctx
package events

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/sensitivity"
)

// THE SIZING KEYS ARE NOT CLAMPED HERE, and that is a decision rather than an
// omission (review #4). An earlier version of this file clamped a non-positive
// distill.max_row_runes to 4000, in the shape distillInterval uses for the
// cadence. The two are not the same case: config.validateDistillCounters
// (validate.go:409-429) refuses every non-positive sizing key with
// SeverityError and writes the policy out — "the sizing keys have NO safe zero
// … a silent second off-switch that the settings surface renders as a
// configured size". A clamp next to it is a second authority for one question,
// and the absorbing one carried the opposite policy.
//
// So the validated value stands as it is. If it ever arrives non-positive
// anyway — a hand-built Config in a test, a future writer that bypasses
// Validate — the reader answers an empty, INCOMPLETE batch to a non-positive
// cap (ctxcheckpoint.go:332-336), distillBatches closes the run as `partial`
// and the ledger shows rows_seen 0: visible, not absorbed. Both halves are
// pinned by tests (TestDistillSizingIsTheValidatorsAuthority and
// TestDistillSelection/ANonPositiveRuneCapProcessesNothing).

// The dump's file modes. MkdirAll and OpenFile both take the process umask off
// these, so the directory mode is re-applied with Chmod (a 022 umask would
// otherwise leave 0755 on a directory of raw session prose).
const (
	distillDumpDirMode  os.FileMode = 0o700
	distillDumpFileMode os.FileMode = 0o600
)

// errDistillDump marks every failure of the dry-run sink — a refused target, a
// directory that cannot be created or sealed, a write that did not land. The
// arm needs the class rather than the text: distill_run.error is a fixed
// vocabulary, and the dump is this wave's durable write, so the whole family
// maps onto block_write_failed.
var errDistillDump = errors.New("dry-run dump")

// distillLedger is the per-batch half of migration 135's counter block
// (135:115-120). The columns count CHUNKS, not parts: the chunk is the prompt
// unit, so it is the unit a cost estimate can be built on (§4.5.2).
type distillLedger struct {
	seen        int   // rows_seen — every chunk the reader handed out
	selected    int   // rows_selected — what survived all four filters
	droppedCred int   // rows_dropped_cred — §4.2.5 rule 2
	droppedDup  int   // rows_dropped_dup — §4.2.4, within the batch and across runs
	chars       int64 // chars_selected — runes of the selected chunks

	// The extraction's three columns (135:121-124, wave A02-8). insights_kept
	// counts what survived G1-G7; insights_rejected is the W18 gate — everything
	// the model offered and the gate refused, the schema's own drops included.
	calls            int
	insightsKept     int
	insightsRejected int

	// rejects is insightsRejected DECOMPOSED by gate — the eight buckets of
	// distillRejectKeys, written to the histogram columns of 149 (wave C4-1,
	// finding N-6). A nil map is legal and reads as all-zero: distillSelect
	// builds a ledger long before an extraction has happened, and a Go map read
	// on nil yields the zero value, so the write path needs no second branch.
	rejects map[string]int

	// g3 is that histogram's g3 bucket DECOMPOSED once more — the four columns
	// of 150 (wave C5-A, entscheid C5-3): a quote the gate could not find in the
	// chunk the model named either stands in another chunk of the same part
	// ("chunk"), across a chunk boundary of that part ("span"), in a different
	// part of the same call ("part"), or nowhere in what the call showed
	// ("none"). Nil reads as all-zero for the same reason rejects does.
	g3 map[string]int

	// groupsShrunk is distill_run.call_groups_shrunk — how often the call
	// planner sized a group down to the block's remaining room.
	groupsShrunk int

	// blocksWritten is the block write's own column (135:125, wave A02-9). It is
	// 0 or 1 per batch and NOT a count of insights: the run writes ONE block and
	// re-writes it per batch, so the column answers "how many durable writes did
	// this run make", which is the number the write order is checkable against.
	blocksWritten int
}

// distillParts groups a batch's items into the parts they were chunked from.
//
// The reader restarts ChunkIndex at 1 for every part and delivers a part's
// chunks consecutively (ctxcheckpoint.go:562-586), so a maximal run of
// consecutive indexes under one block id is exactly one part. The rule is
// written over the INDEX and not over the id alone because of a live case: one
// part is listed in two manifests (019f5b5f-e51c-7a94-a374-91c104491dd2) and a
// manifest may list a part twice, which arrives as two runs of the same id —
// concatenating them would hand the detector a doubled body.
func distillParts(items []distillsource.Item) [][]distillsource.Item {
	var out [][]distillsource.Item
	for i, it := range items {
		if i > 0 {
			prev := items[i-1]
			same := it.Origin.BlockID == prev.Origin.BlockID &&
				it.Origin.ChunkIndex == prev.Origin.ChunkIndex+1
			if same {
				out[len(out)-1] = append(out[len(out)-1], it)
				continue
			}
		}
		out = append(out, []distillsource.Item{it})
	}
	return out
}

// distillSelect applies the credential drop and the substance floor to one
// batch and returns the survivors plus the counters. Dedup is the caller's next
// step — it needs the journal.
//
// STAGE (a), THE PART SCAN, IS WHY THE PARTS ARE REASSEMBLED HERE. sensitivity
// .Scan is length- and adjacency-gated in four places (a 64+ hex run,
// sensitivity.go:69; a 32+ base64 run, :68; key AND value inside one match,
// :60; a hash label within 32 bytes of a hex run, :108). A non-overlapping
// split breaks exactly those gates: a 64-hex secret across a chunk boundary
// becomes 30 + 34 characters and Scan answers false on BOTH halves. The chunks
// of one part concatenate byte-identically to the stripped part body
// (ctxcheckpoint/parse.go:121-125), so reassembling them here restores the
// text the detector needs without asking the reader for a second shape.
//
// A hit drops the WHOLE part, rows_dropped_cred += len(chunks) (§4.2.5 rule 2a).
//
// THE SEAM WINDOW OF STAGE (b) IS DELIBERATELY NOT BUILT, and this is the one
// place this wave reads the design against its letter. §4.2.5 prescribes a
// 256+256 byte window per chunk boundary because it assumes the arm chunks the
// material itself and therefore never holds the whole body. Here the body IS
// held, and every window is a contiguous substring of it: a Scan hit inside a
// window is a hit in the body, so stage (a) already catches every true positive
// the window could. What the window would add is the FALSE-positive direction
// the design names itself — a "SHA-256:" label cut off at the seam makes an
// integrity hash look unlabelled. Building it would drop material for a
// property of the cut, not of the text.
//
// Stage (c), the per-chunk scan, stays: it costs one pass over the same bytes
// and it is the layer that still holds if the reassembly is ever wrong.
//
// THE OMISSION RESTS ON A CONTRACT, AND THE CONTRACT IS ASSERTED HERE (review
// #6). Stage (a) only replaces stage (b) while every chunk of a part reaches
// this function IN ONE BATCH, consecutively, so that distillParts rebuilds the
// WHOLE body. That is true because a manifest is the reader's atom and
// readManifest chunks each listed part in one go — a property of a package this
// wave must not touch. A reader change that split a part across batches, or
// reordered items, would silently degrade stage (a) to stage (c) and bring the
// seam case back with no test in this package failing. So the gate suite binds
// it: TestDistillSelection/ThePartArrivesWholeSoTheSeamScanIsCovered reads the
// production reader, asserts ONE part group whose concatenation is byte-equal
// to the stripped body, and then drives a 64-hex secret across the real 4000-
// rune boundary through the arm. Whoever changes the batching sees that probe
// go red before the protection is gone.
func distillSelect(items []distillsource.Item, minRunes int) ([]distillsource.Item, distillLedger) {
	l := distillLedger{seen: len(items)}
	kept := make([]distillsource.Item, 0, len(items))
	for _, part := range distillParts(items) {
		if m, hit := sensitivity.Scan(distillPartBody(part)); hit {
			// Kind and Reason are the detector's own labels and carry no
			// matched text (sensitivity.go:8-14) — that promise is why they may
			// be logged at all.
			slog.Warn("scheduler: distiller dropped a part on a credential signal",
				"block", part[0].Origin.BlockID, "chunks", len(part),
				"kind", m.Kind, "reason", m.Reason)
			l.droppedCred += len(part)
			continue
		}
		for _, it := range part {
			if m, hit := sensitivity.Scan(it.Text); hit {
				slog.Warn("scheduler: distiller dropped a chunk on a credential signal",
					"block", it.Origin.BlockID, "chunk", it.Origin.ChunkIndex,
					"kind", m.Kind, "reason", m.Reason)
				l.droppedCred++
				continue
			}
			// The substance floor has no counter of its own in migration 135;
			// it is the remainder of seen - selected - cred - dup, and the wave
			// report says so rather than inventing a column.
			if utf8.RuneCountInString(it.Text) < minRunes {
				continue
			}
			kept = append(kept, it)
		}
	}
	return kept, l
}

// distillPartBody reassembles a part from its chunks. The concatenation is
// byte-identical to the body the reader stripped — chunks never overlap and
// nothing is trimmed at a cut.
func distillPartBody(part []distillsource.Item) string {
	if len(part) == 1 {
		return part[0].Text
	}
	var b strings.Builder
	for _, it := range part {
		b.WriteString(it.Text)
	}
	return b.String()
}

// distillNormalize is the dedup comparison form: NFC, then whitespace collapse
// (§4.2.4). It is NOT derived.Normalize, and the difference is one step: that
// form case-folds, because it answers "is this quote contained in that source".
// This one answers "is this the same material", and folding case there would
// merge two chunks that differ only in a rename — a silent drop of material,
// which is the direction a dedup key must not err in.
func distillNormalize(s string) string {
	s = norm.NFC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// distillRowHash is distill_seen.row_hash: SHA-256 over the normalized chunk
// text (135:210).
func distillRowHash(text string) []byte {
	sum := sha256.Sum256([]byte(distillNormalize(text)))
	return sum[:]
}

// distillDedup drops the chunks this source has already distilled, within the
// batch and across runs, and returns the survivors with their hashes.
//
// The ledger is keyed (source_key, row_hash) (135:208-213) and source_key
// carries the scope (§4.5.1), so a hash seen in one scope never silences a
// chunk in another.
//
// IT ALSO WRITES, and that is wave C5-B rather than a side effect that crept
// in: a ledger hit IS the sighting last_seen is named after, and this is the
// only place in the arm where one is observed. distillMarkSeen cannot do it —
// it books the prefix that reached a CALL (§4.5.4), and a repeat never gets
// that far because the loop below drops it first. See distillTouchSeen.
func (s *Scheduler) distillDedup(ctx context.Context, key string, items []distillsource.Item) ([]distillsource.Item, [][]byte, int, error) {
	if len(items) == 0 {
		return nil, nil, 0, nil
	}
	hashes := make([][]byte, len(items))
	probe := make([][]byte, 0, len(items))
	batch := make(map[string]bool, len(items))
	for i, it := range items {
		hashes[i] = distillRowHash(it.Text)
		if k := string(hashes[i]); !batch[k] {
			batch[k] = true
			probe = append(probe, hashes[i])
		}
	}

	rows, err := s.pool.Query(ctx, `
		SELECT row_hash FROM distill_seen
		 WHERE source_key = $1 AND row_hash = ANY($2::bytea[])`, key, probe)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("distill: reading dedup ledger for %q: %w", key, err)
	}
	defer rows.Close()
	seen := make(map[string]bool, len(probe))
	// The ledger hits, kept as bytes for the touch below. Only these: the
	// in-batch repeats marked further down have no row yet, and an UPDATE
	// against them would be a statement about nothing.
	met := make([][]byte, 0, len(probe))
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, nil, 0, fmt.Errorf("distill: scanning dedup ledger: %w", err)
		}
		seen[string(h)] = true
		met = append(met, bytes.Clone(h))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("distill: reading dedup ledger for %q: %w", key, err)
	}
	rows.Close()
	s.distillTouchSeen(ctx, key, met)

	keptItems := make([]distillsource.Item, 0, len(items))
	keptHashes := make([][]byte, 0, len(items))
	dropped := 0
	for i, it := range items {
		k := string(hashes[i])
		if seen[k] {
			dropped++
			continue
		}
		// A repeat INSIDE the batch counts as a duplicate too (§4.3 R4 asks for
		// both directions): the ledger row does not exist yet, so only this set
		// can see it.
		seen[k] = true
		keptItems = append(keptItems, it)
		keptHashes = append(keptHashes, hashes[i])
	}
	return keptItems, keptHashes, dropped, nil
}

// distillTouchSeen slides last_seen forward for the hashes this batch met
// again — the producing half of the horizon distill.seen_retention_days
// deletes on (wave C5-B).
//
// IT IS NOT COSMETIC BOOKKEEPING. The design's horizon is "a hash is useful for
// as long as the same output keeps coming back" (135:49-50), and the ON CONFLICT
// DO UPDATE in distillMarkSeen was written to carry it. It cannot: a returning
// chunk is dropped by distillDedup BEFORE anything reaches distillMarkSeen, so
// the conflict branch never fires on a repeat and last_seen stood at INSERT
// time for the life of the row. Measured before this line existed: a chunk
// re-listed by a later manifest fell as a duplicate while its ledger row stayed
// 400 days old (wave report, red probe 2) — which under a consumer on that
// column is the cyclic test run "payable again every 30 days" that the comment
// promised to prevent, and at 1M+ blocks the arm's dominant cost item
// (135:222-227).
//
// The existing gate could not see it: it asserts last_seen > now() - 1 minute
// with the INSERTING run seconds earlier in the same test
// (distill_select_integration_test.go:342-355), which is true either way.
//
// Failure is logged and dropped, deliberately. This is housekeeping on the read
// path of a batch that is about to be selected, dumped and extracted; a ledger
// that could not be touched costs at worst one re-purchase after the horizon,
// while failing the batch would cost the whole tick.
func (s *Scheduler) distillTouchSeen(ctx context.Context, key string, hashes [][]byte) {
	if len(hashes) == 0 {
		return
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE distill_seen SET last_seen = now()
		 WHERE source_key = $1 AND row_hash = ANY($2::bytea[])`, key, hashes); err != nil {
		slog.Warn("scheduler: distiller dedup ledger touch failed",
			"source_key", key, "hashes", len(hashes), "error", err)
	}
}

// distillMarkSeen records the hashes of a durable batch.
//
// It INSERTS the sightings; distillTouchSeen slides them. The ON CONFLICT
// branch here stays for the writer it actually has — two batches of one run
// that carry the same hash, and the crash window between a mark and the next
// read — and is no longer the file's answer to the sliding window (§4.2.4).
func (s *Scheduler) distillMarkSeen(ctx context.Context, key string, hashes [][]byte) error {
	if len(hashes) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO distill_seen (source_key, row_hash)
		SELECT $1, h FROM unnest($2::bytea[]) AS h
		ON CONFLICT (source_key, row_hash) DO UPDATE SET last_seen = now()`, key, hashes)
	if err != nil {
		return fmt.Errorf("distill: writing dedup ledger for %q: %w", key, err)
	}
	return nil
}

// distillDumpDir validates the configured dump target and answers the path to
// use — RESOLVED — or "" for "no plaintext dump".
//
// TWO LAYERS, ONE QUESTION EACH, and the split is what the wave's review made
// explicit. config.validateDistillDryRunDir (V31) owns the SYNTACTIC half:
// empty or absolute, refused otherwise, on every writing path — boot over
// environment and defaults (cmd/ctxd/main.go: FromEnv + Validate + exit),
// assembled config (config/build.go), settings write (config/store.go). This
// function owns the half a validator structurally cannot answer: whether the
// path lies in a git working copy is a fact about the FILE SYSTEM AT THE MOMENT
// OF USE — a symlink, a mount or a fresh `git init` changes it while the
// configured value stays the same — and it is also the only layer a hot
// settings change passes through before the next dump is written.
//
// An earlier version of this comment claimed a validator would see only the
// /api/settings path. That was wrong at the code, and the review proved it; the
// syntactic check now exists on both sides rather than on neither.
//
// SYMLINKS ARE RESOLVED BEFORE THE WALK. A lexical ancestor walk is trivially
// bypassed: a dryrun_dir that is itself a symlink into a working copy (or lies
// under one) passes every lexical check, while MkdirAll and OpenFile follow the
// link and drop raw session prose right into the repository — measured, not
// feared. EvalSymlinks resolves what exists; for a target that does not exist
// yet, the deepest EXISTING ancestor is resolved and the remainder appended, so
// a link in the middle of the path cannot hide behind a missing leaf.
//
// WHAT THIS CANNOT SEE, stated instead of implicitly promised: a bind mount of
// a working copy under a clean path is invisible to every path-based check. The
// operator-facing promise is therefore "no path that RESOLVES into a working
// copy", not "never inside a repository".
func distillDumpDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w: dryrun_dir %q is not absolute", errDistillDump, dir)
	}
	resolved := distillResolve(filepath.Clean(dir))
	if root, ok := distillGitWorkTree(resolved); ok {
		return "", fmt.Errorf("%w: dryrun_dir %q (resolved to %q) lies inside the git working copy %q",
			errDistillDump, dir, resolved, root)
	}
	return resolved, nil
}

// distillResolve answers dir with every symlink on it resolved, as far as the
// path exists. A path whose leaf is not created yet resolves its deepest
// existing ancestor and keeps the remainder — the remainder cannot be a symlink
// precisely because it does not exist.
//
// An unresolvable path is answered unchanged rather than refused: the caller's
// next step (MkdirAll) is the one that decides whether the path is usable at
// all, and the git walk over the lexical form is still strictly better than no
// walk.
func distillResolve(dir string) string {
	rest := ""
	for p := dir; ; {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return dir
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// distillGitWorkTree walks dir and its ancestors and names the first one
// carrying a .git entry.
//
// Lstat, not Stat, and no distinction between a file and a directory: a
// submodule and a linked worktree carry a .git FILE, and the .project submodule
// BA13 is written about is exactly that shape. Non-existent ancestors simply do
// not match, which is what makes the check work for a directory that has not
// been created yet.
func distillGitWorkTree(dir string) (string, bool) {
	for p := dir; ; {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return p, true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", false
		}
		p = parent
	}
}

// distillDump is the dry-run sink of one run. A nil *distillDump is the
// "dump off" state and every method on it is a no-op, so the caller has no
// branch of its own.
type distillDump struct {
	path string
	f    *os.File
}

// distillDumpRecord is one line of the dump. Everything except text is
// code-owned; text is the foreign chunk, and it is the reason the file lives
// where it lives.
type distillDumpRecord struct {
	Block   string `json:"block"`
	Chunk   int    `json:"chunk"`
	Ordinal int    `json:"ordinal,omitempty"`
	Role    string `json:"role,omitempty"`
	Hash    string `json:"hash"`
	Runes   int    `json:"runes"`
	Text    string `json:"text"`
}

// distillOpenDump creates the dump file of one run. dir == "" yields a nil
// dump.
//
// The file is named after the RUN ID and never after the source: source_key
// carries metadata->>'root_session_id' verbatim from the corpus (§4.5.2), so a
// file named after it would put writable foreign text into a path — a traversal
// surface and, at the same time, foreign content in a file name, which BA13
// refuses on its own.
func distillOpenDump(dir, runID string) (*distillDump, error) {
	if dir == "" {
		return nil, nil
	}
	if !distillHexID(runID) {
		return nil, fmt.Errorf("%w: run id %q is not a uuid — refusing to build a dump path from it", errDistillDump, runID)
	}
	if err := os.MkdirAll(dir, distillDumpDirMode); err != nil {
		return nil, fmt.Errorf("%w: creating dryrun_dir %q: %w", errDistillDump, dir, err)
	}
	// RE-CHECKED AFTER THE DIRECTORY EXISTS. distillDumpDir ran on a path whose
	// leaf may not have existed yet; now it does, so the resolution is complete
	// and a link that appeared between the two calls — or one the earlier
	// resolution could not follow — is caught before the first byte is written.
	if root, ok := distillGitWorkTree(distillResolve(dir)); ok {
		return nil, fmt.Errorf("%w: dryrun_dir %q resolves into the git working copy %q after creation",
			errDistillDump, dir, root)
	}
	// MkdirAll applies the umask, so the mode is asserted afterwards. An
	// existing directory keeps whatever it had otherwise — this dump is raw
	// session prose, and 0755 on it would be the leak the target path avoids.
	if err := os.Chmod(dir, distillDumpDirMode); err != nil {
		return nil, fmt.Errorf("%w: sealing dryrun_dir %q: %w", errDistillDump, dir, err)
	}
	path := filepath.Join(dir, runID+".ndjson")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_EXCL, distillDumpFileMode)
	if err != nil {
		return nil, fmt.Errorf("%w: opening dump %q: %w", errDistillDump, path, err)
	}
	return &distillDump{path: path, f: f}, nil
}

// distillHexID reports whether s is a uuid in text form: hex digits and dashes
// only.
func distillHexID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r == '-':
		default:
			return false
		}
	}
	return true
}

// write appends one batch and SYNCS it. The sync is the durability step of the
// dry-run's write order: the chunks are marked seen only after this returns, so
// a crash in between re-dumps them instead of losing them from both.
func (d *distillDump) write(items []distillsource.Item, hashes [][]byte) error {
	if d == nil || len(items) == 0 {
		return nil
	}
	enc := json.NewEncoder(d.f)
	for i, it := range items {
		rec := distillDumpRecord{
			Block:   it.Origin.BlockID,
			Chunk:   it.Origin.ChunkIndex,
			Ordinal: it.Origin.Ordinal,
			Role:    it.Origin.Role,
			Hash:    hex.EncodeToString(hashes[i]),
			Runes:   utf8.RuneCountInString(it.Text),
			Text:    it.Text,
		}
		if err := enc.Encode(rec); err != nil {
			return fmt.Errorf("%w: writing dump %q: %w", errDistillDump, d.path, err)
		}
	}
	if err := d.f.Sync(); err != nil {
		return fmt.Errorf("%w: syncing dump %q: %w", errDistillDump, d.path, err)
	}
	return nil
}

// close releases the dump file and removes it again when nothing was written —
// an empty file per run would be the arm's own litter.
func (d *distillDump) close() {
	if d == nil {
		return
	}
	empty := false
	if st, err := d.f.Stat(); err == nil && st.Size() == 0 {
		empty = true
	}
	if err := d.f.Close(); err != nil {
		slog.Warn("scheduler: distiller could not close its dump", "path", d.path, "error", err)
	}
	if empty {
		if err := os.Remove(d.path); err != nil {
			slog.Warn("scheduler: distiller could not remove its empty dump", "path", d.path, "error", err)
		}
	}
}
