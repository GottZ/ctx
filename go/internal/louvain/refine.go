package louvain

// Die Refinement-Phase (Achse 04 / Welle S5, design/04 §4.3).
//
// ══ DAS PROBLEM, das sie löst ══
//
// Louvain kann UNZUSAMMENHÄNGENDE Communities liefern — auch ein korrekt
// implementierter. Der Mechanismus ist bekannt (Traag et al. 2019, §4.1b): ein
// Knoten, der zwei Teile seiner Community als einziger verbindet, kann selbst
// weiterziehen; die beiden Teile bleiben dann in derselben Community zurück,
// ohne dass zwischen ihnen noch eine Kante läuft. Die Modularität merkt davon
// nichts — sie summiert innere Kantengewichte, sie prüft keinen Zusammenhang.
//
// Für diese Codebasis ist das kein ästhetisches Problem, sondern ein
// inhaltliches: eine unzusammenhängende Community ist ein schlechtes Topic und
// ein schlechter core_blocks-Kern (design/01 §4.4). Die Qualität der Achsen 01
// und 03 hängt daran, nicht nur das Aussehen der Karte.
//
// ══ WAS DIESE PHASE TUT ══
//
// Nach der Konvergenz einer Ebene wird jede Community in ihre
// ZUSAMMENHANGSKOMPONENTEN zerlegt (bezüglich des von ihr induzierten
// Teilgraphen), und die Reduktion arbeitet auf diesen Teilmengen weiter.
//
// Das ist die zusammenhangs-garantierende Hälfte von Leidens Phase 2. Die
// andere Hälfte (randomisiertes Aufspalten gut verbundener Teilmengen zur
// Verbesserung der Zielfunktion) ist hier ABSICHTLICH NICHT gebaut: sie bringt
// Qualität, aber sie bringt auch einen PRNG zurück in einen Kern, dessen
// Determinismus-Anker gerade darauf beruht, keinen zu haben (§4.6, Achse 1
// "entfällt"). Was hier steht, ist deterministisch und kostet O(m).
//
// ══ WARUM Q DABEI NICHT FALLEN KANN ══
//
// Zwischen zwei Komponenten derselben Community läuft per Definition keine
// Kante, der A-Term ist also 0; der Strafterm −γ·k_i·k_j/2m ist strikt negativ.
// Das Trennen entfernt damit ausschliesslich negative Beiträge — dasselbe
// Argument, mit dem §4.4 den Komponenten-Vorpass als exakt beweist. S5-G2
// prüft es empirisch statt es zu glauben.

// splitDisconnected zerlegt jede Community in ihre Zusammenhangskomponenten.
//
// Die neuen Community-Indizes werden anschliessend kanonisiert, damit die
// Nummerierung wie überall im Paket an der Knotenreihenfolge hängt und nicht an
// der Besuchsreihenfolge der Suche.
func splitDisconnected(g *Graph, memb []int32) ([]int32, int) {
	n := g.N()
	out := make([]int32, n)
	for i := range out {
		out[i] = -1
	}
	// Iterative Tiefensuche mit explizitem Stapel: eine rekursive Variante
	// liefe bei 9,8M Knoten in einer Riesenkomponente (live 93,7 %) in den
	// Stapelüberlauf.
	stack := make([]int32, 0, 64)
	next := int32(0)
	for s := 0; s < n; s++ {
		if out[s] >= 0 {
			continue
		}
		home := memb[s]
		out[s] = next
		stack = append(stack[:0], int32(s)) //nolint:gosec // n < 2^31
		for len(stack) > 0 {
			v := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for k := g.Off[v]; k < g.Off[v+1]; k++ {
				u := int32(g.Adj[k]) //nolint:gosec // Index < 2^31
				// Nur INNERHALB derselben Community laufen — die Suche
				// beschreibt den von der Community induzierten Teilgraphen.
				if memb[u] != home || out[u] >= 0 {
					continue
				}
				out[u] = next
				stack = append(stack, u)
			}
		}
		next++
	}
	return canonicalize(out)
}

// DisconnectedCommunities zählt, wie viele Communities einer Partition in mehr
// als eine Zusammenhangskomponente zerfallen.
//
// Exportiert, weil es die Messgrösse des Gates S5-G1 IST — und weil ein
// Zusammenhangs-Versprechen, das nur intern geprüft wird, keines ist. Der
// Rebuild kann die Zahl später ins Lauf-Journal schreiben.
func DisconnectedCommunities(g *Graph, memb []int32) int {
	split, splitCount := splitDisconnected(g, memb)
	_ = split
	orig := 0
	seen := make(map[int32]struct{}, len(memb)/4+1)
	for _, c := range memb {
		if _, ok := seen[c]; !ok {
			seen[c] = struct{}{}
			orig++
		}
	}
	// Jede Community, die zerfällt, erzeugt mindestens eine zusätzliche
	// Komponente. Die Differenz ist damit die Zahl der ÜBERZÄHLIGEN Teile,
	// nicht die der betroffenen Communities — für ein Gate "ist irgendetwas
	// unzusammenhängend?" ist beides gleichwertig, und die Differenz ist ohne
	// zweite Passe zu haben.
	return splitCount - orig
}
