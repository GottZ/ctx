package overview

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Gate S2-G5 (design/04 §6.7 / §3.5): ein NEUES Kind an einem ALTEN
// Elternprozess muss LAUT scheitern, nie still Erfolg melden.
//
// Der Hintergrund ist der strikte Kind→Eltern-Decoder (worker.go
// DisallowUnknownFields). S2 erweitert Stats um die Messflaeche des Journals;
// im Mischversions-Fenster liest ein alter Elternprozess ein Stats-Dokument mit
// unbekannten Feldern. Das MUSS ein Fehler sein — ein toleranter Decoder wuerde
// die neuen Felder still verwerfen und der Elternprozess schriebe ein Journal
// voller Nullen, das wie eine Messung aussieht.
//
// K12-Einordnung, damit die Probe nicht mehr behauptet als sie belegt: Eltern
// und Kind sind DASSELBE Binary, und ein Container-Deploy tauscht beide atomar.
// Das Fenster ist damit dokumentiert (BP-8), nicht akut — die Probe sichert die
// Fail-Richtung, nicht die Haeufigkeit.
func TestStatsIPC_G5_OldParentRejectsNewChild(t *testing.T) {
	// legacyStats ist die Stats-Form VOR S2, auf die Felder gekuerzt, die ein
	// alter Elternprozess kennt. Bewusst hier dupliziert statt aus der Historie
	// importiert: die Probe soll gegen eine FESTGESCHRIEBENE alte Form pruefen,
	// nicht gegen das, was zufaellig noch uebrig ist.
	type legacyStats struct {
		Skipped        bool
		SkipReason     string
		NodeCount      int
		ClusterCount   int
		EdgeRows       int
		Modularity     float64
		CandidateCount map[string]int
	}

	var buf bytes.Buffer
	if err := EncodeWorkerStats(&buf, Stats{
		NodeCount: 1192, ClusterCount: 59, Modularity: 0.8768,
		// Die S2-Felder: genau sie sind im alten Decoder unbekannt.
		LoadMs: 12, ClusterMs: 340, PersistMs: 88, LockHeldMs: 61,
		PeakRSSKb: 433_152, PartitionHash: []byte{0xde, 0xad},
	}); err != nil {
		t.Fatalf("EncodeWorkerStats: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.DisallowUnknownFields() // exakt die Einstellung, die worker.go faehrt
	var old legacyStats
	err := dec.Decode(&old)
	if err == nil {
		t.Fatal("alter Eltern-Decoder akzeptierte das neue Stats-Dokument — " +
			"ein stiller Erfolg mit verworfenen Messwerten ist genau der verbotene Ausgang")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Fehler ist nicht die Protokoll-Drift, sondern: %v", err)
	}

	// Gegenprobe: der AKTUELLE Decoder liest dasselbe Dokument verlustfrei.
	// Ohne sie belegte die Haelfte oben nur, dass irgendetwas kaputt ist.
	got, err := DecodeWorkerStats(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("aktueller Decoder scheitert am eigenen Dokument: %v", err)
	}
	if got.LockHeldMs != 61 || got.PeakRSSKb != 433_152 || !bytes.Equal(got.PartitionHash, []byte{0xde, 0xad}) {
		t.Fatalf("Roundtrip verlor S2-Felder: %+v", got)
	}
}
