// Package goldbench ist der Benchmark-Harness für die ctx-LLM-Pipelines.
//
// Der Harness „mockt ctx": Er spielt die echten ctx-System-Prompts und
// Prompt-Builder (über die bench_exports.go-Shims der jeweiligen Pakete)
// gegen ein beliebiges OpenAI-kompatibles Modell ab, parst die Antworten mit
// den ctx-treuen Parsern und scored gegen Gold-Daten
// (ctx-bench-Repo data/*.jsonl, 12 Achsen). Zweck: Modelle auf ihre Eignung
// für die einzelnen ctx-LLM-Rollen prüfen.
//
// Mock-Treue: Jede Abweichung vom Original ist am Ort der Abweichung als
// Kommentar mit file:line-Quelle dokumentiert (Suchanker: "ABWEICHUNG").
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package goldbench

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
)

// Axes ist die kanonische Achsen-Liste — ein Eintrag pro Gold-Datei.
var Axes = []string{
	"cluster-label",
	"keywords",
	"links",
	"recurrence",
	"rerank",
	"sensitivity",
	"synthesis",
	"tagging",
	"temporal-block",
	"temporal-query",
	"title",
	"translate",
}

// Case ist ein einzelner Gold-Fall (Zeile einer *.jsonl-Datei). Input und
// Gold bleiben roh — die Achsen-Runner dekodieren ihr eigenes Schema.
type Case struct {
	ID           string          `json:"id"`
	Axis         string          `json:"axis"`
	Input        json.RawMessage `json:"input"`
	Gold         json.RawMessage `json:"gold"`
	LabelQuality string          `json:"label_quality"`
	Source       string          `json:"source"`
	License      string          `json:"license"`
}

// Config steuert einen Benchmark-Lauf. Die Felder spiegeln die CLI-Flags von
// cmd/ctx-goldbench.
type Config struct {
	DataDir     string   // Verzeichnis der *.jsonl-Gold-Dateien
	Endpoint    string   // OpenAI-kompatible Basis-URL oder volle /chat/completions-URL
	Model       string   // Modellname für den Request-Body
	APIKey      string   // optionaler Bearer-Token
	Axes        []string // zu fahrende Achsen (Teilmenge von Axes)
	N           int      // Limit pro Achse, 0 = alle Fälle
	Concurrency int      // parallele LLM-Calls
	DryRun      bool     // kein HTTP; Scorer sehen leere Outputs
	Seed        int64    // deterministisches Case-Sampling + Request-Seed
	TimeoutSec  int      // HTTP-Timeout pro Call in Sekunden
	Verbose     bool     // per_case-Ergebnisse in den Report aufnehmen
	GitRev      string   // Env-Stamp: git-Revision des Baus
	ServerNote    string // freier Provenienz-Text (Server-Flags/Build) für den Env-Stamp
	// MaxTokensMult skaliert das per-Achse-max_tokens-Budget (Default 1 =
	// pipeline-treu). >1 ist eine DOKUMENTIERTE Abweichung für Reasoning-
	// Modelle, deren Denk-Tokens das Antwort-Budget verbrauchen — Scores
	// sind nur zwischen Läufen mit gleichem Multiplikator vergleichbar.
	MaxTokensMult float64
}

// LoadCases lädt die Gold-Fälle einer Achse aus <dir>/<axis>.jsonl und
// validiert das axis-Feld jeder Zeile.
func LoadCases(dir, axis string) ([]*Case, error) {
	path := filepath.Join(dir, axis+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("goldbench: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var cases []*Case
	sc := bufio.NewScanner(f)
	// Einzelne Fälle (links, rerank) überschreiten das 64-KiB-Default-Token-Limit.
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var c Case
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("goldbench: %s:%d: %w", path, line, err)
		}
		if c.Axis != axis {
			return nil, fmt.Errorf("goldbench: %s:%d: axis %q erwartet, %q gefunden", path, line, axis, c.Axis)
		}
		if c.ID == "" || len(c.Input) == 0 || len(c.Gold) == 0 {
			return nil, fmt.Errorf("goldbench: %s:%d: unvollständiger Fall (id/input/gold)", path, line)
		}
		cases = append(cases, &c)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("goldbench: scan %s: %w", path, err)
	}
	return cases, nil
}

// SampleCases begrenzt eine Fall-Liste deterministisch auf n Fälle: Shuffle
// mit festem Seed, dann die ersten n, danach wieder nach ID sortiert. So ist
// die Stichprobe bei gleichem Seed über Läufe und Maschinen hinweg stabil,
// ohne einen Kopf-Bias der Datei zu erben.
func SampleCases(cases []*Case, n int, seed int64) []*Case {
	if n <= 0 || n >= len(cases) {
		return cases
	}
	out := make([]*Case, len(cases))
	copy(out, cases)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministisches Sampling, keine Kryptographie
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	out = out[:n]
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DatasetHash bildet sha256 über die Inhalte aller angeforderten Achsen-
// Dateien in sortierter Dateinamen-Reihenfolge — der Env-Stamp des Reports.
func DatasetHash(dir string, axes []string) (string, error) {
	names := make([]string, len(axes))
	copy(names, axes)
	sort.Strings(names)
	h := sha256.New()
	for _, a := range names {
		b, err := os.ReadFile(filepath.Join(dir, a+".jsonl"))
		if err != nil {
			return "", fmt.Errorf("goldbench: dataset hash: %w", err)
		}
		_, _ = fmt.Fprintf(h, "%s\n", a)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
