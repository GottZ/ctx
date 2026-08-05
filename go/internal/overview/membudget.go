package overview

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
)

// Das Kind-Speicherbudget (Achse 04 / S7b, design/04 §7 S7b + SP-8).
//
// ══ WOGEGEN ══
//
// S1 hat gemessen: der Ist-Rechenpfad belegt am heutigen 200k-Cap 423 MB
// Spitze, gegen ein Container-Limit von 512 MiB — und die Elternlast
// (graphcache-CSR über den ganzen Korpus, ungedeckelt, UD-09-04) ist darin
// NICHT enthalten. Das Limit gilt der cgroup, nicht dem Prozess. Reisst sie,
// entscheidet der OOM-Killer, welcher der beiden Prozesse stirbt — und der
// Elternprozess ist der Daemon.
//
// Zwei Massnahmen, die zusammengehören:
//
//	(a) Das Kind bekommt ein EIGENES Go-Speicherlimit und einen Vorab-Check.
//	    Reisst der Vorab-Check, endet der Lauf als sauberer Skip mit
//	    Journal-Zeile — statt als OOM ohne jede Spur.
//	(b) Das Kind macht sich zum bevorzugten OOM-Opfer (oom_score_adj), damit
//	    ein trotzdem eintretender cgroup-Kill NICHT den Daemon trifft.
//
// ══ WARUM DER DEFAULT AUS IST — und das ist eine Abweichung vom Entwurf ══
//
// design/04 §3.4 nennt als Default 160 MiB. Dieser Wert stammt aus der Zeit
// VOR der S1-Messung. Bei 423 MB Ist-Spitze am heutigen Cap würde ein
// 160-MiB-Limit JEDEN Lauf am aktuellen Korpus-Cap abbrechen — aus einem
// Speicher-SCHUTZ würde ein Speicher-VERBOT, und die Karte fröre dauerhaft
// ein. Deshalb: Default 0 = aus, verhaltensneutral. Der Betreiber wählt den
// Wert gegen peak_rss_kb aus dem Lauf-Journal, das genau dafür da ist.
//
// ══ WARUM ÜBER ENV UND NICHT ÜBER DIE OPTIONS-IPC ══
//
// Das Kind erbt die Umgebung des Daemons (events/overview_worker.go setzt
// cmd.Env bewusst nicht — dort steht auch der CONTEXT_DB_*-DSN). Der Wert ist
// damit ohne eine DRITTE Protokolländerung erreichbar, und er wird an genau
// der Stelle gebraucht, an der es noch keine Options gibt: VOR dem Decode,
// beim Prozess-Eintritt. Ein Options-Feld wäre ein weiteres strikt-dekodiertes
// Neustartfenster für einen reinen Ops-Knopf.

// WorkerMemLimitEnv ist die Umgebungsvariable, aus der sich das Kind selbst
// deckelt. Gleichnamig zum Config-Key, damit Compose-Deklaration und
// Laufzeitverhalten nicht auseinanderlaufen können.
const WorkerMemLimitEnv = "CTX_GRAPH_OVERVIEW_WORKER_MEM_LIMIT"

// WorkerMemLimitBytes liest das Kind-Budget aus der Umgebung. 0 = aus.
//
// Ein unlesbarer Wert ist bewusst KEIN Fehler, sondern "aus" plus eine
// Warnung des Aufrufers: ein vertippter Ops-Knopf darf den Rebuild nicht
// verhindern — er darf ihn nur nicht schützen.
func WorkerMemLimitBytes() (int64, error) {
	raw := os.Getenv(WorkerMemLimitEnv)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("overview: %s=%q is not a byte count — the child stays unlimited", WorkerMemLimitEnv, raw)
	}
	return v, nil
}

// ApplyWorkerMemLimit setzt das Go-Speicherlimit des Kindes.
//
// debug.SetMemoryLimit ist ein WEICHES Limit: der Garbage Collector läuft
// darüber häufiger, statt zu töten. Genau das ist gewollt — es verwandelt eine
// Speicherwand in Rechenzeit, und die Rechenzeit hat mit dem Zeitbudget aus
// S6+S7 bereits einen eigenen Guard. Ein hartes Limit gäbe es in Go nicht,
// ohne den Prozess selbst zu beenden, und das täte der OOM-Killer besser.
func ApplyWorkerMemLimit(limit int64) {
	if limit <= 0 {
		return
	}
	debug.SetMemoryLimit(limit)
}

// estimateBytesPerNode / estimateBytesPerPair sind die Konstanten der
// Vorab-Abschätzung, KALIBRIERT gegen die S3-G3-Messung und nicht geschätzt:
//
//	CSR-Substrat @400k Knoten / 899k Paare = 62 MB Gesamt-VmHWM
//	CSR-Substrat @200k Knoten / 450k Paare = 39 MB
//	marginal: 200k Knoten + 449k Paare ⇒ 23 MB
//
// Daraus ~40 B je Knoten (UUID-String-Header + Scope + Off) und ~24 B je Paar
// (2 × (Adj 4 B + W 8 B)). Die Konstanten sind bewusst grosszügig: eine
// Abschätzung, die zu NIEDRIG liegt, lässt den Lauf ins OOM laufen — genau in
// den Fall, den sie verhindern soll.
const (
	estimateBytesPerNode = 48
	estimateBytesPerPair = 32
	// estimateOverheadBytes deckt Go-Runtime, pgx-Puffer und den
	// gonum-Graphen, den der gonum-Pfad zusätzlich baut.
	estimateOverheadBytes = 32 << 20
)

// EstimateRebuildBytes schätzt den Spitzenbedarf des Rechenpfads.
//
// Sie läuft VOR dem CSR-Build (SP-8): ein Check NACH dem Laden käme zu spät,
// weil das Laden selbst der teure Teil ist. Die Knotenzahl steht nach
// loadNodes fest, die Kantenzahl ist eine Schätzung aus der Dichte — deshalb
// nimmt der Aufrufer die Kandidatenzahl und die zuletzt gemessene Dichte.
func EstimateRebuildBytes(nodes int, pairs int) int64 {
	return int64(nodes)*estimateBytesPerNode + int64(pairs)*estimateBytesPerPair + estimateOverheadBytes
}
