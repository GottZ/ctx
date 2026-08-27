package armsweep_test

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// Gate (f) of wave M-W3d: `compare` holds FOUR dumps at once (base, cond and
// the two halves of the replicate pair). At the target scale that is 4 × 290 000
// records — design/05 §6.1 puts a full campaign at "0,5–1 GB RSS before the
// variants are folded" if the dumps are read into memory. All metrics are
// paired per query, so a full load is constructively unnecessary.
//
// The probe is two runs of the same comparison: the streaming one has to stay
// under the cap, and the full-load control has to break it. Without the second
// half the first would pass on a machine with a generous allocator and prove
// nothing.

const (
	// streamCases is the per-dump line count of the gate (design/05 §7, row
	// M-W3d: "vier 290 000-Zeilen-Dumps").
	streamCases = 290000
	// streamRSSCap is the resident-set ceiling the streaming comparison must
	// respect. Generous next to the ~10 MB of per-case accumulators it really
	// needs, and far below the four materialised dumps of the control.
	streamRSSCap = 400 << 20
)

// TestCompareStreamsFourLargeDumps is gate (f).
func TestCompareStreamsFourLargeDumps(t *testing.T) {
	if testing.Short() {
		t.Skip("gate (f) writes 4 × 290 000 records; -short skips it")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "BASE.jsonl.gz")
	cond := filepath.Join(dir, "COND.jsonl.gz")
	na := filepath.Join(dir, "NOISEA.jsonl.gz")
	nb := filepath.Join(dir, "NOISEB.jsonl.gz")

	writeBigDump(t, base, false)
	writeBigDump(t, cond, true)
	copyFile(t, base, na)
	copyFile(t, base, nb)

	in := armsweep.CompareInput{
		Base: bigRef("BASE", base), Cond: bigRef("COND", cond),
		NoisePair:   []armsweep.DumpRef{bigRef("NOISEA", na), bigRef("NOISEB", nb)},
		Seed:        20260812,
		GitRevision: "deadbeef",
		GoldStamp:   goldset.Stamp{SampleSeed: 20260812, SplitSeed: 20260825},
	}

	debug.FreeOSMemory()
	before := rssBytes(t)
	body, err := armsweep.Compare(in)
	if err != nil {
		t.Fatalf("Compare over four large dumps: %v", err)
	}
	streamed := rssBytes(t) - before
	if body.Paired != streamCases {
		t.Fatalf("paired %d cases, want %d", body.Paired, streamCases)
	}
	if streamed > streamRSSCap {
		t.Fatalf("streaming comparison used %d MiB RSS, cap is %d MiB",
			streamed>>20, streamRSSCap>>20)
	}
	t.Logf("gate (f): streaming RSS delta %d MiB over 4 × %d records (cap %d MiB)",
		streamed>>20, streamCases, streamRSSCap>>20)

	// The control: the same four dumps, materialised the way a non-streaming
	// implementation would hold them.
	debug.FreeOSMemory()
	before = rssBytes(t)
	held := loadAll(t, []string{base, cond, na, nb})
	loaded := rssBytes(t) - before
	runtime.KeepAlive(held)
	t.Logf("gate (f) control: full load of the same dumps used %d MiB RSS", loaded>>20)
	if loaded <= streamRSSCap {
		t.Fatalf("the full-load control stayed under the cap (%d MiB) — the probe does not discriminate",
			loaded>>20)
	}
}

func bigRef(runID, path string) armsweep.DumpRef {
	stamp := stampFor(runID, filepath.Base(path), nil)
	stamp.Records = streamCases
	stamp.SliceFiles = []armsweep.SliceDigest{
		{Slice: goldset.SliceKI, File: goldset.FileKI, SHA256: goldset.SHA256Hex("ki"), N: streamCases},
	}
	return armsweep.DumpRef{Role: strings.ToLower(runID), Path: path, Stamp: stamp}
}

// writeBigDump streams one dump to disk in case-key order — the fixture itself
// must not need the memory the gate is about. The keys ascend because the
// digest is the zero-padded case number and the case index is constant, and
// armsweep.Record.Key() is slice/index/digest.
func writeBigDump(t *testing.T, path string, swapped bool) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	w, err := armsweep.NewRecordWriter(f, path)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	for i := 0; i < streamCases; i++ {
		if err := w.Write(bigRecord(i, swapped)); err != nil {
			t.Fatalf("write record %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func bigRecord(i int, swapped bool) armsweep.Record {
	idA := "a" + strconv.Itoa(i)
	idB := "b" + strconv.Itoa(i)
	rankA, rankB := 1, 2
	if swapped {
		rankA, rankB = 2, 1
	}
	return armsweep.Record{
		Slice: goldset.SliceKI, Index: 0,
		QuerySHA256: fmt.Sprintf("%064x", i),
		GoldIDs:     []string{idA},
		Rows: []rrf.ArmRow{
			{ID: idA, RankSemantic: &rankA, MassFactor: 1, TypeFactor: 1, TypeName: plainType},
			{ID: idB, RankSemantic: &rankB, MassFactor: 1, TypeFactor: 1, TypeName: plainType},
		},
		Selector: armsweep.Selector{Mode: "ann", Reason: "grey", ScanTuples: intp(60000)},
		Attempts: 1,
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}

// loadAll is the negative control: every record of every dump, held at once.
func loadAll(t *testing.T, paths []string) [][]armsweep.Record {
	t.Helper()
	out := make([][]armsweep.Record, 0, len(paths))
	for _, p := range paths {
		s, err := armsweep.OpenRecordStream(p)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		var recs []armsweep.Record
		for {
			rec, ok, err := s.Next()
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			if !ok {
				break
			}
			recs = append(recs, rec)
		}
		_ = s.Close()
		out = append(out, recs)
	}
	return out
}

// rssBytes reads the resident set size off /proc/self/statm (Linux, the only
// platform this instrument runs on). runtime.MemStats would report the Go heap,
// which is exactly the number a full load could hide behind a released span.
func rssBytes(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		t.Skipf("no /proc/self/statm: %v", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		t.Fatalf("unexpected /proc/self/statm: %q", string(b))
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("parse rss: %v", err)
	}
	return pages * int64(os.Getpagesize())
}

// TestRecordStreamRoundTripsGzip pins the artefact half of the gate: a dump
// written with the .gz suffix is compressed, and the stream reads it back
// record for record.
func TestRecordStreamRoundTripsGzip(t *testing.T) {
	dir := t.TempDir()
	recs := records(dumpOpts{})
	plain := filepath.Join(dir, "plain.jsonl")
	packed := filepath.Join(dir, "packed.jsonl.gz")
	if err := armsweep.WriteRecords(plain, recs); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	if err := armsweep.WriteRecords(packed, recs); err != nil {
		t.Fatalf("write gzip: %v", err)
	}
	pi, err := os.Stat(plain)
	if err != nil {
		t.Fatalf("stat plain: %v", err)
	}
	gi, err := os.Stat(packed)
	if err != nil {
		t.Fatalf("stat gzip: %v", err)
	}
	if gi.Size() >= pi.Size() {
		t.Errorf("the .gz artefact is %d bytes against %d uncompressed — it was not compressed", gi.Size(), pi.Size())
	}
	if gi.Mode().Perm() != 0o600 {
		t.Errorf("dump mode = %v, want 0600", gi.Mode().Perm())
	}
	for _, p := range []string{plain, packed} {
		s, err := armsweep.OpenRecordStream(p)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		var got []armsweep.Record
		for {
			rec, ok, err := s.Next()
			if err != nil {
				t.Fatalf("next %s: %v", p, err)
			}
			if !ok {
				break
			}
			got = append(got, rec)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %s: %v", p, err)
		}
		want, err := json.Marshal(sortedByKey(recs))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		have, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(want) != string(have) {
			t.Errorf("%s: round trip lost records (%d read, %d written)", p, len(got), len(recs))
		}
	}
}

// sortedByKey mirrors the order WriteRecords persists in.
func sortedByKey(recs []armsweep.Record) []armsweep.Record {
	out := append([]armsweep.Record(nil), recs...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Key() < out[j-1].Key(); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
