package llmlog

import (
	"testing"
)

// The context_llm_log row of a recording consumer (issue #571) carries no
// bodies under any gateway.body_logging value: its request and answer live in
// context_llm_recordings alone, the one place only a server operator reads.
// Every other column of the row is the row the same request of an unrecorded
// consumer gets (the golden of TestGatewayUnrecordedRowsMatchTheBase), plus
// the marker metadata.gw.recorded with its recording_id, so a reader can
// tell the row apart. The matrix is the golden's: every pipeline, policy,
// sensitivity, error and body shape.
func TestGatewayRecordedRowsCarryNoBodies(t *testing.T) {
	const recordingID = "0195a6f0-0000-7000-8000-0000000f0571"
	names, rows := unrecordedMatrix()
	withBodies := 0
	for _, n := range names {
		plain := GatewayEntry(rows[n])
		if plain.RequestUser != "" || plain.ResponseContent != "" {
			withBodies++
		}
		r := rows[n]
		r.Recorded, r.RecordingID = true, recordingID
		e := GatewayEntry(r)
		if e.RequestSystem != "" || e.RequestUser != "" || e.ResponseContent != "" {
			t.Errorf("%s: the recorded row carries bodies (system %d, user %d, response %d bytes) — want none",
				n, len(e.RequestSystem), len(e.RequestUser), len(e.ResponseContent))
			continue
		}
		gw, _ := e.Metadata["gw"].(gatewayWire)
		if !gw.Recorded || gw.RecordingID != recordingID {
			t.Errorf("%s: metadata.gw recorded %v recording_id %q — want true %q", n, gw.Recorded, gw.RecordingID, recordingID)
			continue
		}
		// Without the bodies and the marker, the row is the unrecorded one.
		plainGW, _ := plain.Metadata["gw"].(gatewayWire)
		gw.Recorded, gw.RecordingID = false, ""
		e.Metadata["gw"] = gw
		plain.RequestUser, plain.ResponseContent = "", ""
		plain.Metadata["gw"] = plainGW
		if got, want := goldenLine(n, e), goldenLine(n, plain); got != want {
			t.Errorf("the recorded row differs from the unrecorded one beyond bodies and marker:\n got  %s\n want %s", got, want)
		}
	}
	if withBodies == 0 {
		t.Fatal("no unrecorded row of the matrix carries bodies: the matrix does not reach a body policy that stores them")
	}
	t.Logf("%d rows; %d of them carry bodies unrecorded, none recorded", len(names), withBodies)
}
