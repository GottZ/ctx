package louvain

// ARI ist der Adjusted Rand Index zwischen zwei Partitionen derselben
// Knotenmenge — die Qualitäts-Kennzahl von Gate S4-G2.
//
// WARUM ARI UND NICHT NUR Q. Modularität misst, wie gut eine Partition zu
// ihrer EIGENEN Zielfunktion passt; sie sagt nichts darüber, ob sie die
// WIRKLICHE Struktur getroffen hat. Ein Kern, der systematisch zu grob
// clustert, kann ein höheres Q liefern als die Ground-Truth (die
// Auflösungsgrenze der Modularität ist genau dieser Effekt) und wäre nach einem
// reinen Q-Vergleich "besser". ARI vergleicht gegen die bekannte Wahrheit und
// fällt bei zusammengelegten oder zerrissenen Communities.
//
// "Adjusted" heisst: gegen den Erwartungswert zufälliger Übereinstimmung
// korrigiert. 1 = identisch, 0 = so gut wie Zufall, negativ = schlechter als
// Zufall.
func ARI(a, b []int32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	n := float64(len(a))

	// Kontingenztabelle, dünn besetzt: nur die tatsächlich auftretenden Paare.
	type pair struct{ x, y int32 }
	joint := make(map[pair]float64)
	rowSum := make(map[int32]float64)
	colSum := make(map[int32]float64)
	for i := range a {
		joint[pair{a[i], b[i]}]++
		rowSum[a[i]]++
		colSum[b[i]]++
	}

	choose2 := func(x float64) float64 { return x * (x - 1) / 2 }

	var sumJoint, sumRow, sumCol float64
	for _, v := range joint {
		sumJoint += choose2(v)
	}
	for _, v := range rowSum {
		sumRow += choose2(v)
	}
	for _, v := range colSum {
		sumCol += choose2(v)
	}

	total := choose2(n)
	expected := sumRow * sumCol / total
	maxIdx := (sumRow + sumCol) / 2
	if maxIdx == expected {
		// Beide Partitionen sind trivial (alles in einem Cluster oder alles
		// getrennt). Der ARI ist dann undefiniert; 1 bei Gleichheit, sonst 0 —
		// eine Konvention, die hier nur die Testausgabe betrifft.
		if sumJoint == sumRow && sumJoint == sumCol {
			return 1
		}
		return 0
	}
	return (sumJoint - expected) / (maxIdx - expected)
}
