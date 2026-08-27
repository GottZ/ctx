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

**Offen:** seit Welle M-W5 ist G-GLOB der zweite ungelabelte Slice, und `prime`
poolt ihn **nicht** — `buildPool` sammelt die Arm-Köpfe ausschließlich für
G-REAL (`internal/armsweep/run.go`, `buildPool`). Ob G-GLOB denselben Pool
bekommt, ist ein offener Entscheid und keine Eigenschaft des Werkzeugs.

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

**`compare`** ist die vierte Stufe und beantwortet eine andere Frage: nicht
„welches Gewichts-Vektor ist besser", sondern „was tut diese Bedingung".
Siehe „Der Bedingungs-Vergleich" weiter unten.

## Welche Slices gefahren werden (`-slices`, Welle X-W1a)

`-slices` nimmt Registry-Namen als CSV. **Vorgabe sind alle sieben:**
`G-KI,G-Q,G-REAL,G-SESS,G-MH,G-GLOB,G-GLOB-KONSTR` — zusammen 1 000 Fälle. Die
Reihenfolge im Artefakt ist immer diese, unabhängig davon, wie die Namen
übergeben wurden.

Ein Name, den die Registry nicht kennt, **bricht den Lauf ab** und wird mit den
bekannten Namen zurückgemeldet; dasselbe gilt für eine leere Nennung und für
einen benannten Slice, dessen Datei keine Fälle enthält. Bis Welle X-W1a war das
anders: die Schleife lief über eine eigene Drei-Slice-Tabelle statt über die
übergebenen Namen, ein unbekannter Name fiel **wortlos** heraus, und ein `prime`
über alle sieben Namen zog 650 statt 1 000 Fälle bei Exit 0. Eine Messung, die
35 % ihrer Grundgesamtheit still verliert, ist schlimmer als eine, die abbricht —
jede Zahl danach bleibt plausibel.

Der Stempel trägt die Folge: `prime-<Lauf>.json` nennt unter `slices` die
tatsächlich geladenen Slices, und `gold_sha256` im Dump-Stempel deckt genau
deren Dateien ab. Ein Rausch-Paar über drei Slices und ein Bedingungs-Dump über
sieben sind damit **nicht** eine Kampagne — `compare` weist das Paar als
inkongruent ab (Exit 4), statt zwei verschiedene Grundgesamtheiten zu
vergleichen.

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

## Der Bedingungs-Vergleich (`compare`, Welle M-W3d)

```bash
./ctx-armsweep compare -dump-base dumps/B0.jsonl -dump-cond dumps/B1.jsonl \
    -noise-pair dumps/V0.jsonl,dumps/V0p.jsonl
```

`score` vergleicht **Gewichte** auf einem Dump. `compare` vergleicht **zwei
Bedingungen**: Dump A ohne einen Blocktyp, Dump B mit ihm. Dieselbe gepaarte
Mechanik, aber die umgekehrte Lesart — bei V0/V0′ ist eine Differenz Rauschen,
hier ist sie das Signal. Deshalb ein eigenes Unterkommando und nicht ein zweites
Flag: `-dump-b` ist im Code das **Replikat** („the noise floor, not a variant"),
und beide Rollen auf einem Flag würden dem Report seine eigene Rausch-Definition
entziehen.

**Was `compare` verweigert, und mit welchem Code:**

| Lage | Exit |
|---|---|
| `-noise-pair` fehlt oder nennt nicht genau zwei Dumps | 3 |
| G-NOISE des Paars ist rot | 3 (Report wird trotzdem geschrieben) |
| `-condition-field` nennt ein Feld, das nicht deklarierbar ist | 3 (**kein** Report — es wurde nichts gemessen) |
| Stempel nicht kongruent: `pin_run_id`/`pin_sha256`, Gold-Bytes, `migrations_max`, Post-Fusion-Stufen, `instance_kind`, `hnsw.ef_search` | 4 |
| GUC-Zustand je Fall abweichend: `hnsw.iterative_scan`, `hnsw.max_scan_tuples` | 4 |
| Auf Basis `delivered` deklariert, ein Dump trägt aber keine gelieferte Rangliste | 4 |

Die drei GUCs sind die Determinismus-Schrauben des schwersten Arms (Design 05
§4.4b, F-23). `hnsw.ef_search` kommt aus dem Dump-Stempel (`/api/status`,
`db.hnsw.ef_search_effective`); `iterative_scan` und `max_scan_tuples` setzt
`ctx_rrf_arms` je Anfrage **nur** auf dem ann-Pfad (142:216-220) — sie werden
deshalb aus dem aufgezeichneten Selector-Zustand je Fall gelesen, nicht aus einem
Konfigurations-Schnappschuss. Der `instance_kind`-Vergleich läuft auf den ROHEN
Dump-Stempeln: der Report-Env führt die Arten eines Paars seit M-W2 zu einem
String zusammen, und ein Gate auf dieser Zusammenführung sähe einen Wert, wo zwei
stehen.

### Die deklarierte Bedingung (`-condition-field`, X-W3a)

Manche Messwellen haben als **Bedingung** genau das, was die Kongruenz-Regel
verbietet: X-W2b legt `cluster.inject_max` von 3 auf 0 um, und `post_fusion_stages`
ist ein Kongruenz-Feld. Design 05 widerspricht sich an dieser Stelle selbst
(§4.3 gegen §7 X-W2b), und der Vergleich lief in Exit 4.

`-condition-field <name>` löst das **ohne** generisches Aufweichen: es erklärt
**genau ein benanntes** Kongruenz-Feld zur Bedingung dieses Vergleichs. Alles
andere bleibt hart — eine zweite Abweichung verwirft den Dump-Satz weiterhin mit
Exit 4. Die Deklaration steht als eigener Block im Report, mit den Werten von
Basis, Bedingung und Rausch-Paar. Deklarierbar ist heute genau:
`post_fusion_stages`; ein anderer Name wird abgewiesen (Exit 3), nicht ignoriert.

Zwei Konsequenzen, die zur Deklaration gehören:

- **Das Rausch-Paar darf die Bedingung nicht überspannen.** Ein Replikat ist ein
  Replikat in JEDEM Feld, auch im deklarierten — sonst misst der Rauschboden die
  Bedingung statt des Instruments. Exit 4.
- **`post_fusion_stages` schaltet die Rechenbasis auf `delivered` um.** Die
  Post-Fusion-Stufen laufen *nach* `ctx_rrf`; in den Arm-Rängen, aus denen die
  Offline-Fusion neu gerechnet wird, existieren sie nicht (X-W2b maß byte-gleiche
  Arm-Signaturen über `inject_max` 0, 3 und 20). Auf der Fusion gerechnet wäre so
  ein Vergleich eine **Tautologie**: exakt 0 mit vollem Bootstrap-CI drumherum.
  Deshalb rechnet er auf der ausgelieferten Rangliste — Kennzahlen, MDE, Rausch-
  boden und Verdrängungs-Tabelle gemeinsam, und der Report sagt es in der
  Kopfzeile, im Bedingungs-Block und im Geltungsbereich.

Die Lieferliste ist auf das Server-Limit gekappt (in der X-W2b-Kampagne 5). Der
Report nennt die längste gemessene Länge: `nDCG@10` über eine 5er-Liste ist
faktisch `nDCG@5`, und Arm- und Lieferebene sind nicht gegeneinander lesbar.
Eine gelieferte ID, die in keinem Arm stand, erscheint in der Verdrängungs-
Tabelle als `(in keinem Arm — Post-Stufe)`.

```bash
ctx-armsweep compare -condition-field post_fusion_stages \
  -dump-base xw2b-b1.jsonl -dump-cond xw2b-b0.jsonl \
  -noise-pair xw2b-v0kp.jsonl,xw2b-v0kpp.jsonl
```

**Was der Report zeigt:** ΔnDCG@10, ΔRecall@5 und ΔMRR@10 je Slice mit
gepaartem Bootstrap-CI und McNemar auf Hit@5; die **Verdrängungs-Tabelle** (wie
viele Top-5-Plätze die Schatten-Typen belegen, welche Typen Plätze verlieren, wie
viele verdrängte Blöcke gelabelt waren — ohne Labels bleibt die Spalte leer und
der Report sagt es); und den **Auflösungs-Report**: die halbe CI-Breite von
ΔnDCG@10 zwischen den beiden identischen Läufen IST die kleinste Differenz, die
eine Slice zeigen kann (MDE). Über 2 pp gilt die Slice vorab als „für Effekte in
Literatur-Größenordnung nicht auflösbar".

Ein Effekt heißt nur dann **lesbar**, wenn sein CI die 0 ausschließt, er über der
MDE seiner Slice liegt **und** seine Diskordanz die des Replikat-Paars übersteigt
(F-32). Eine Bedingung, die weniger Fälle bewegt als das Instrument von selbst,
ist kein Befund.

Dumps werden **streamend** gelesen und dürfen gzip-komprimiert sein
(`.jsonl.gz`): ein Vergleich hält vier Dumps gleichzeitig, und bei 290 000
Datensätzen je Dump ist das der Unterschied zwischen ~40 MB und über 1 GB
Arbeitsspeicher.

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

## Der G-REAL-Regime-Split (Welle X-W0b)

```bash
./ctx-armsweep score   -dump dumps/<A>.jsonl -dump-b dumps/<B>.jsonl \
    -regime-labels x-w0-labels.jsonl
./ctx-armsweep compare -dump-base <B0> -dump-cond <B1> -noise-pair <V0>,<V0'> \
    -regime-labels x-w0-labels.jsonl
```

`-regime-labels` nimmt die X-W0-Label-Datei aus dem Gold-Verzeichnis
(`query_sha256` → `local`/`global`, gemessen 131/19 auf den 150 G-REAL-Queries)
und trägt G-REAL **zusätzlich** als `G-REAL-local` und `G-REAL-global` aus:
Zensus-Zeile, Metrik-Zeile je Konfiguration, Vergleichs-/Effekt-Zeile,
Verdrängungs-Zeile und — in `compare` — eine eigene MDE-Zeile. Die
Gesamt-Zeile bleibt daneben stehen und Zahl für Zahl unverändert; ohne das Flag
ist der Report exakt der bisherige.

Der Grund steht in `design/05 §4.4b`: die Literatur misst in beiden Regimen
**gegenläufige** Sieger, also ist jede Uplift-Aussage über eine ungeteilte
G-REAL-Zeile ein Mittel über zwei Regime, die sich gegenseitig aufheben. Mit
n=19 auf der globalen Hälfte fällt deren MDE entsprechend groß aus — das ist
eine Eigenschaft der Slice und steht als Zahl im Report, nicht als Fußnote.

Die beiden Hälften sind **Melde-Zeilen, nie Gatter-Eingang** — schärfer
begründet als der Boden-Check G-GLOB-KONSTR: ein Stratum ist eine **Teilmenge**
einer Zeile, die bereits abstimmt. Ließe man beide abstimmen, zählten dieselben
Fälle zweimal — in der Auswertbarkeits-Konjunktion von G-NOISE und im
Regressions-Veto von G-WIN. Beide laufen deshalb weiter über `ReportSlices()`.

Ein G-REAL-Fall ohne Label bricht den Lauf ab (**Exit 4**) statt still eine
Rest-Hälfte zu bilden: die Hälften addierten sich sonst zu einer Zahl über eine
Menge, die niemand definiert hat, und kein Feld im Report würde das sagen. Die
Label-Datei wird nicht in `STAMP.json` registriert (sie enthält keine Fälle,
sondern partitioniert vorhandene) — ihr Name und ihr sha256 stehen im
Report-Env unter `regime_labels`.

## Betrieb

- **Off-Peak fahren, nicht parallel zu Dream.** Das ist eine Betriebsnotiz,
  keine technische Garantie — das Werkzeug erzwingt nichts. Die Messung läuft in
  einer RepeatableRead/ReadOnly-Transaktion, die einen Snapshot hält; 1 000
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

**Die Instanz-Art steht seit X-W3a in JEDEM Dump-Stempel**, nicht nur bei
`-shadow-types`: `dump` liest `server.instance_kind` vor der ersten Messanfrage
und stempelt, was die Instanz gesagt hat. Grund ist die Kampagnenregel F-32
(„alle Dumps einer Kampagne kommen von EINER Instanz") — sie bewachte vorher
gerade die Dumps nicht, aus denen eine Kampagne besteht: X-W2b maß einen
Vergleich aus zwei Kopie-Dumps und einem **Live**-Rausch-Paar, der mit Exit 0
durchlief, weil leer gleich leer vergleicht. Eine Instanz, die den Schlüssel
beantwortet, aber nichts sagt, wird als `unknown` gestempelt — nie leer. Leer
heißt jetzt genau zweierlei: Trockenlauf, oder Dump aus einem Lauf **vor**
X-W3a. Solche Alt-Dumps bleiben **lesbar**; eine Kampagne, die sie mit
gestempelten mischt, wird verworfen (Exit 4), und die Abweisung sagt, welche
Seite nie gestempelt wurde.

Das Instrument verweigert einen Schatten-Dump gegen eine Instanz, die sich nicht
als Mess-Kopie ausweist — **Exit 5**. Grund: ein Schatten-Block ist eine echte
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
