# ctx-armsweep

Der Treiber des Arm-Gewichts-Sweeps (Design 04 §4.6–§4.9, Welle B-W5).

`ctx_rrf` fusioniert vier Arme — semantisch 0.45, `fts_de` 0.20, `fts_en` 0.25,
Trigramm 0.10, k = 60. Diese Zahlen sind nie gemessen worden. `ctx-armsweep`
misst sie: es nimmt die Roh-Ränge auf, die die Fusion füttern, und rechnet
offline nach, was andere Gewichte daraus gemacht hätten.

## Ablauf

```bash
cd go && go build ./cmd/ctx-armsweep

./ctx-armsweep prime                                  # Pins ziehen, Embed-Cache wärmen
./ctx-armsweep dump -pins pins-<Lauf>.jsonl           # Messlauf V0
./ctx-armsweep dump -pins pins-<Lauf>.jsonl           # Messlauf V0′ (zweiter Lauf, gleiche Pins)
./ctx-armsweep score -dump dumps/<A>.jsonl -dump-b dumps/<B>.jsonl
```

**`prime`** fährt jede Gold-Query einmal ohne Pins über die admin-gegatete
`arm_ranks`-Naht. Es sammelt Übersetzung und temporale Expansion als Pins
(`pins-<Lauf>.jsonl`), wärmt den Embed-Cache und schreibt einen Stempel
(`prime-<Lauf>.json`). Für den ungelabelten Slice G-REAL entsteht zusätzlich
`pool-<Lauf>.jsonl`: die Top-20 je Arm, Eingabe für die Relevanz-Urteile in
Welle B-W6. Nichts wird gescort.

Aus dieser Datei baut `ctx-goldset pool` die blinde Urteils-Vorlage (Vereinigung
der vier Arm-Köpfe plus fünf gleichverteilt gezogene Kontroll-Blöcke, dedupliziert,
seed-permutiert, ohne Score, Arm oder Rang), ein Mensch urteilt, und
`ctx-goldset ingest` schreibt die Labels nach `g-real.jsonl` samt
Kontroll-Trefferquote in den Stempel. Ablauf und Regeln:
[`docs/development.md`](../../../docs/development.md) — Abschnitt „Blind relevance
judgements for G-REAL". Solange G-REAL ungelabelt ist, rechnet `score` den Slice
als „unlabelled, skipped".

**`dump`** fährt dieselben Queries MIT Pins. Ein fehlender Pin ist ein Fehler,
kein stiller Rückfall auf den ungepinnten Pfad — ein teils gepinnter Lauf ist
weder das eine noch das andere, und der Unterschied wäre im Artefakt unsichtbar.
Ergebnis ist `dumps/<Lauf>.jsonl` plus `dumps/<Lauf>.stamp.json`.

**`score`** ist offline. Es fasst keinen Server an und keine Uhr außer der
Kopfzeile: zwei Läufe über denselben Dump erzeugen dieselben Bytes.

## Warum zwei Dumps

`V0` und `V0′` sind dieselbe Konfiguration auf zwei unabhängigen Läufen. Ihre
Uneinigkeit IST das Rauschmaß des Instruments (Gate **G-NOISE**): Diskordanz
Recall@5 ≤ 5 % und ein gepaartes 95-%-CI von ΔnDCG@10, das die 0 enthält. Fällt
G-NOISE durch — oder fehlt der zweite Dump —, markiert der Report sich als
**nicht interpretierbar**, und keine Zahl in der Varianten-Tabelle ist ein
Ergebnis. Ein Effekt unterhalb der Wiederhol-Streuung ist kein Effekt.

**G-WIN** wird ausschließlich auf `G-Q-HOLD` entschieden — der Hälfte des
seed-basierten 50/50-Splits, auf der nicht abgeleitet wurde. V1 ist die eine
vorab festgelegte Primärvergleichung und wird bei 95 % gelesen; die übrigen 13
laufen bei 1 − 0,05/13 und heißen, wenn sie durchkommen, „Kandidat,
unbestätigt".

## Die Damping-Kurve (Welle M-W8)

```bash
./ctx-armsweep score -dump dumps/<A>.jsonl -damping-type checkpoint
```

`-damping-type` hängt eine **zweite Familie** an den Report: die Damping-Kurve
eines Blocktyps über zehn Stützstellen — 0,05 · 0,10 · 0,15 · 0,20 · 0,30 · 0,35
· 0,50 · 0,70 · 0,85 · 1,00. Das Gitter enthält jeden Faktor, den die Registry
heute vergibt (`toolEvidenceDamping` 0,15, `auditTrailDamping` 0,3,
`toolOverviewDamping` 0,35, ungedämpft 1,0), damit der Ist-Zustand immer eine
der Stützstellen ist und das Optimum daneben steht statt daran vorbei.

Warum das offline geht: seit Migration 142 trägt jede Dump-Zeile ihren
`type_name`, und `type_factor` wirkt multiplikativ **nach** der Arm-Zugehörigkeit
— die Arm-CTEs sehen ihn nicht. Die ganze Kurve wird also aus einem einzigen
Dump neu fusioniert, ohne noch einmal zu messen.

Die Kurve steht in einer **eigenen** Report-Sektion und bekommt bewusst **kein**
G-WIN-Urteil: das Optimum ist ein Befund für den, der den Registry-Wert setzt,
und zehn zusätzliche Zeilen in der Varianten-Tabelle würden deren fest
verdrahtetes Bonferroni-Niveau über 13 Vergleiche stillschweigend aufweichen.

Ein Dump, der **vor** Migration 142 gemessen wurde, wird bei angeforderter Kurve
hart abgewiesen (Exit 4) — nicht auf einen Vorgabewert zurückgesetzt. Solche
Zeilen tragen keinen Typnamen; die Kurve käme flach heraus, und zwar per
Konstruktion statt per Messung. Läufe **ohne** `-damping-type` sind von alledem
unberührt und über Alt-Dumps unverändert lauffähig.

## Betrieb

- **Off-Peak fahren, nicht parallel zu Dream.** Das ist eine Betriebsnotiz,
  keine technische Garantie — das Werkzeug erzwingt nichts. Die Messung läuft in
  einer RepeatableRead/ReadOnly-Transaktion, die einen Snapshot hält; 650
  Queries davon neben einem Dream-Lauf belasten dieselbe GPU und dieselbe
  Datenbank, die gerade gemessen werden.
- **`-concurrency` bleibt bei 1.** Höhere Werte stapeln Snapshots auf einer
  Produktivinstanz in genau dem Fenster, das der Lauf nicht stören soll.
- **Admin-Key nötig.** Die `arm_ranks`-Naht antwortet Nicht-Admins mit 403, und
  der Treiber bricht darauf sofort ab statt sein Retry-Budget zu verbrennen.
- **Der Server muss die B-W2-Naht tragen.** v5.5.0 tut das nicht; gegen eine
  solche Instanz meldet der Treiber „response carries no arm_ranks block".

## Drift-Protokoll

Jeder Dump ist links und rechts von einer Korpus-Zählung eingeklammert (pro Typ:
`count`, `max(created_at)`, `max(updated_at)`, NULL-Embeddings; dazu die
Lebenszyklus-Stempel der gelabelten Blöcke). Drei Regeln verwerfen den Lauf:

1. Ein Gold-Block wurde während des Laufs geändert oder ist verschwunden.
2. Ein **retrievbarer** Typ ist von 0 auf >0 NULL-Embeddings gesprungen.
   Ausgeschlossene Typen zählen nicht mit — der Live-Korpus hält dort tausende
   NULL-Embeddings als stehende Politik.
3. Die Zahl retrievbarer Blöcke hat sich um mehr als ±0,5 % bewegt.

Dazu die Kontaminations-Probe: ein gelabelter Block, der nach
`corpus_max_created_at` aus `STAMP.json` entstanden ist, kann zum Ziehungs-
zeitpunkt kein Label gewesen sein.

Ein verworfener Dump wird zu `<Lauf>.jsonl.aborted` umbenannt, nicht gelöscht.
Die Bytes sind der einzige Beleg für die Drift, die den Abbruch ausgelöst hat.

## Ablage

Jeder Schreibvorgang ist eingegrenzt: Dumps nach `dumps/` unterhalb des
Gold-Verzeichnisses, Reports nach `reports/`. Ein Pfad, der aus der Wurzel
herausführt — auch über einen Symlink —, wird abgewiesen. Einziger Override ist
`-allow-outside-goldset`, und er steht als `allow_outside_goldset: true` im
Report.

## Schatten-Dumps (`-shadow-types`, M-W2)

`dump` kann Typen benennen, die im Ergebnis unsichtbar sind (`retrieval.policy =
excluded` **plus** `retrieval.shadow_measurable = true`): sie werden für
`ctx_rrf` und `ctx_rrf_arms` dieser Anfrage sichtbar geschaltet und für nichts
sonst. So misst ein Cond-Dump einen Korpus, der noch nicht live sein darf.

Das Instrument verweigert einen solchen Dump gegen eine Instanz, die sich nicht
als Mess-Kopie ausweist (`server.instance_kind = measure-copy`, gelesen von der
gemessenen Instanz) — **Exit 5**. Grund: ein Schatten-Block ist eine echte
Zeile mit Embedding und tsvector, und weder der HNSW- noch die beiden
GIN-Indexe sind partiell; er kostet also auf **jeder Produktionsanfrage**
Scan-Budget. Deshalb gehört der Schatten-Korpus in eine wiederhergestellte
Kopie. Override `-allow-live-instance`; Instanz-Art, Schatten-Typen und
Override stehen im Stempel und im Report.

`prime` weist `-shadow-types` ab: Pins gelten für beide Dumps eines Paares.

Gold-Daten liegen im privaten `.project`-Submodule (root-only, 0600, nicht im
Repository). Dumps und Pins tragen die effektiven Query-Texte und liegen
deshalb im selben Verzeichnis unter denselben Rechten. **Reports tun das nicht:**
ein Fall wird dort als `Slice + Index + sha256-Präfix` zitiert, nie als Text.

## Was das Instrument NICHT misst

Alles nach `ctx_rrf`. Gravity, Cluster-Injektion, Graph-Expansion, der
Aggregat-Fold und die Rerank-Stufe laufen auf dem Live-Pfad. Sie werden
**notiert** — die ausgelieferte Reihenfolge steht im Dump, der Zustand der
Stufen (`cluster.enabled`, `cluster.inject_max`, `graph.enabled`,
`rerank.enabled`) im Env-Stempel — aber keine davon wird nachgerechnet. Ein
Gewicht, das hier gewinnt, gewinnt an der Fusionsstufe; ob es die Post-Stufen
übersteht, ist eine andere Frage und braucht ein anderes Instrument.

Einschränkung dazu: die Kennzeichnung ausgelieferter Treffer heißt im Dump
`via_post_stage` und ist **abgeleitet**. `rrf.SearchResult` führt `ViaGraph`
und `ViaCluster` (`internal/rrf/search.go:48,64`), aber weder `llm.Source` noch
`sourceResponse` reicht das Feld über die API weiter. Der Ersatz ist exakt für
das, wofür er hier gebraucht wird — eine ausgelieferte ID, die nicht unter den
Arm-Kandidaten steht, kann nicht aus der Fusion stammen —, unterscheidet aber
Graph- nicht von Cluster-Injektion.

## Konfigurationen

| Name | Was |
|---|---|
| V0 | Live-Fusion 0.45/0.20/0.25/0.10, k = 60 |
| V0′ | dieselbe Konfiguration auf dem zweiten Dump — das Rauschmaß |
| S1–S4 | je ein Arm allein |
| V1 | FTS-Arme zu einem verschmolzen, Rang = min(de, en); 0.45/0.45/0.10 |
| V2–V4 | verschobene Gewichte (semantik-lastig, mittig, Trigramm aus) |
| V5 | `mass·type` vor der Summe statt in jedem Term |
| V6a/b | w ∝ nDCG_solo^β, β ∈ {1, 2}, abgeleitet nur auf `G-Q-DERIV` |
| V7a–c | k ∈ {10, 30, 120} |

Metriken je Slice, nie gepoolt: nDCG@10, Recall@5, MRR@10. G-KI ist strukturell
trigrammfreundlich; ein Mittel über die Slices würde eine Zahl zwischen
Instrumenten verschieben, die sich keines teilen.
