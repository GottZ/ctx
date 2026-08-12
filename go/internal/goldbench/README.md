# ctx-goldbench — Benchmark-Harness für die ctx-LLM-Pipelines

Der Harness „mockt ctx": Er spielt die **echten ctx-Prompts** (System-Prompts,
Prompt-Builder, promptguard-Wiring) gegen ein beliebiges OpenAI-kompatibles
Modell ab, parst die Antworten mit den **ctx-treuen Parsern** und scored gegen
Gold-Daten. Zweck: Modelle auf ihre Eignung für die einzelnen ctx-LLM-Rollen
prüfen, bevor sie eine Rolle im Backend-Pool bekommen.

Die Prompts und Parser kommen nicht als Kopie, sondern über schmale
Export-Shims (`bench_exports.go` in `internal/dream`, `internal/llm`,
`internal/rrf`, `internal/topiclabel`) direkt aus den Produktions-Paketen —
ändert sich ein Prompt in ctx, misst der Bench automatisch den neuen Stand.

## Nutzung

```bash
# Voller Lauf gegen ein Modell
go run ./cmd/ctx-goldbench \
  -endpoint https://api.example.com -model my-model \
  -axes all -concurrency 8 -out report.json -md report.md

# Schneller Teil-Lauf: 25 Fälle pro Achse, zwei Achsen
go run ./cmd/ctx-goldbench -endpoint http://127.0.0.1:8080 -model qwen3.5:9b \
  -axes rerank,synthesis -n 25

# Datenvalidierungs-Smoke ohne HTTP
go run ./cmd/ctx-goldbench -dry-run -axes all
```

| Flag | Default | Bedeutung |
|---|---|---|
| `-data` | `bench/goldbench/data` (Repo-Root) | Verzeichnis der Gold-Dateien (`<achse>.jsonl`) |
| `-endpoint` | — | OpenAI-kompatible Basis-URL oder volle `/v1/chat/completions`-URL |
| `-model` | — | Modellname im Request-Body |
| `-api-key` | env `GOLDBENCH_API_KEY` | Bearer-Token (optional) |
| `-axes` | `all` | CSV-Achsenliste oder `all` |
| `-n` | `0` | Limit pro Achse (0 = alle; Sampling deterministisch per Seed) |
| `-concurrency` | `4` | parallele LLM-Calls |
| `-out` | `goldbench-report.json` | JSON-Report |
| `-md` | — | Markdown-Report (optional) |
| `-dry-run` | `false` | kein HTTP; lädt + validiert alle Fälle, baut alle Prompts, Report mit parse_rate 0 |
| `-seed` | `20260812` | Case-Sampling + OpenAI-`seed`-Feld |
| `-timeout` | `120` | HTTP-Timeout pro Call (Sekunden) |
| `-verbose` | `false` | per-Case-Ergebnisse in den JSON-Report |

## Achsen

| Achse | Gemockte Pipeline | Primär-Metrik |
|---|---|---|
| `temporal-block` | dream-temporal (`internal/dream/validate_temporal.go`) | Set-F1 der Datumswerte (micro) |
| `temporal-query` | query-temporal (`internal/llm/temporal.go`) | F1 über (date,dir[,end])-Tupel; Leermengen-Korrektheit zählt als Treffer |
| `keywords` | dream-keywords (`internal/dream/keywords.go`) | Recall der Gold-Keywords (Substring beidseitig) |
| `tagging` | **prospective pipeline** (kein ctx-Pendant) | wie keywords, gegen `gold.tags` |
| `title` | **prospective pipeline** (kein ctx-Pendant) | Token-F1, Constraint ≤120 Zeichen |
| `links` | dream-eval (`internal/dream/evaluate.go` + `parse.go`) | 1.0 exakt (target,type) / 0.5 target / 0; + Typ-Konfusionsmatrix |
| `recurrence` | dream-recurrence (`internal/dream/recurrence.go`) | 3-Klassen-Accuracy; + FP-Rate auf none |
| `sensitivity` | sensitivity-audit (`internal/llm/classify.go`, 2 Calls/Fall) | Accuracy beider Fragen; kritisch: FN-Rate auf Positiven |
| `cluster-label` | topiclabel (`internal/topiclabel/`) | Constraint-Pass (parseLabel); informativ Token-F1 |
| `rerank` | query-rerank-judge (`internal/rrf/rerank.go`) | nDCG@15 (binäre Relevanz); + Score-Separation |
| `synthesis` | query-synthesize v5.2 (`internal/llm/synthesize.go`, `BuildPrompt`) | Contract-Pass (Refusal- bzw. Zitat-Vertrag) |
| `translate` | query-translate (`internal/llm/translate.go`) | Pass-Rate (validateTranslation + Pflicht-Tokens + umlautfrei + Längen-Ratio) |

Composite = ungewichtetes Mittel der Primär-Scores, zusätzlich getrennt für
gold- und silver-Achsen (eine Achse gilt als silver bei >50 % silver-Cases).
Der Env-Stamp trägt Modell, Endpoint (ohne Key), sha256 über die sortierten
Datei-Inhalte, git-Revision, Timestamp, Seed und n je Achse.

## Mock-Treue

Der Harness spielt Prompts byte-treu ab, wo immer möglich. Jede Abweichung ist
zusätzlich am Ort der Abweichung als Code-Kommentar dokumentiert
(Suchanker: `ABWEICHUNG`). Vollständige Liste:

1. **Wire-Format OpenAI statt Ollama-Chain** (`client.go`): `num_predict` →
   `max_tokens`. Nicht portabel und daher weggelassen: `top_k 20`
   (dream-eval/recurrence, `internal/dream/evaluate.go:66-73`),
   `repeat_penalty 1.1` (synthesis, `internal/llm/client.go:369-379`) und
   `num_ctx` (Chain-Merge aus der Backend-Row). `Format:"json"` des
   classify-Calls (`internal/llm/classify.go:181`) wird als
   `response_format {"type":"json_object"}` übertragen. Zusätzlich sendet der
   Harness das OpenAI-`seed`-Feld — ctx sendet keinen Seed.
2. **Kein Router, keine Chain, kein Sensitivity-Gate, kein llmlog**: Der Bench
   misst das Modell, nicht die ctx-Infrastruktur. Admission, Backend-Failover
   und Telemetrie sind bewusst außerhalb des Mocks; Transport-Fehler werden
   nicht retried (der Fall scored 0 und wird als `transport_errors` gezählt).
3. **keywords ohne Retry-Schleife**: `GenerateKeywords` retried bis zu 3× bei
   Parse-Fehlern/zu wenigen Keywords (`internal/dream/keywords.go:84-139`);
   der Bench wertet den ersten Wurf — Retries würden Parse-Schwächen des
   Modells maskieren. Der Parser selbst (inkl. Regex-Fallback) ist das Original.
4. **recurrence `title_sim`**: kommt in ctx aus PG (`similarity(b.title, $2)`,
   `internal/dream/recurrence.go:144`); der Harness rechnet den Wert
   strukturgleich als pg_trgm-Reimplementation in Go nach
   (`score.go` `trgmSimilarity`). Der Wert ist reines Prompt-Metadatum
   (Header-Zeile, `recurrence.go:244`). Sampling wie dream-eval
   (der Dream-Loop reicht `DreamOptions()` an beide Pipelines).
5. **links: leerer Output = Parse-Fehler**: `parseLinks("")` ist im Original
   ein valider Leer-Verdict (`internal/dream/parse.go:44`); im Bench bedeutet
   ein leerer String „kein Response" (Dry-Run/Transport) und zählt nicht als
   geparst.
6. **synthesis ohne Budget-Fit**: `BuildPrompt` (inkl. Nonce-Rule und
   Quellen-Rendering) ist das Original; der H12-Budget-Pass
   (`fitSourcesToBudget`) und die Auto-Window-Constraints entfallen — der
   Prompt geht ungekürzt an das Modell, `temporalDates` ist nil (die
   Gold-Cases tragen keinen temporalen Kontext).
7. **cluster-label ohne Output-Screen**: gemessen wird der
   `parseLabel`-Contract (`internal/topiclabel/guard.go:54`); der
   Echo-/Scan-Screen (`screenLabel`) braucht die sensitiven Kern-Titel eines
   Live-Clusters und ist nicht Teil des Falls.
8. **tagging/title sind prospektive Pipelinen** („prospective pipeline"):
   ctx hat heute keine LLM-Tagging-/Titel-Pipeline. Die System-Prompts sind
   Neubauten im ctx-Stil (JSON-Contract, Injection-Klausel); der
   tagging-User-Prompt nutzt den originalen dream-keywords-Builder, der
   title-User-Prompt spiegelt `<block>`-Wrap + guardText/guardLine
   (`internal/dream/promptguard_wire.go:21,35`) strukturgleich nach.
9. **Nonces bleiben zufällig**: promptguard-Nonces werden wie in Produktion
   pro Prompt-Bau frisch erzeugt — Prompts mit Nonce sind über Läufe hinweg
   nicht byte-identisch, exakt wie im Live-System.

## Verifikation

```bash
cd go
GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
  go test ./internal/goldbench/ -count=1
go run ./cmd/ctx-goldbench -dry-run -axes all   # 1127 Fälle, parse_rate 0
```

Die Unit-Tests decken alle Scorer mit festen Fixtures ab; der
End-to-End-Test fährt den Runner mit `n=2` pro Achse gegen einen
httptest-Fake-Server, der pro Achse eine kanonisch gültige Antwort liefert
(echte Gold-Daten, echte Prompt-Builder, echte Parser).
