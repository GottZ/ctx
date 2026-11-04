package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestDoorCodesInVocabulary (W02-05, G10; W01-6 review 2 FR2-9): every code
// the model-key door refuses with is a code of the gateway vocabulary
// (oai.Codes), and every refusal's status is the one the vocabulary binds the
// code to — the renderer looks the status up (oai.WriteCode), so a door
// decision with another status would be sent with the vocabulary's.
func TestDoorCodesInVocabulary(t *testing.T) {
	vocab := map[string]int{}
	for _, c := range oai.Codes() {
		vocab[c.Code] = c.Status
	}
	for _, code := range []string{ModelAuthCodeProxyMisconfigured, ModelAuthCodeOverloaded, ModelAuthCodeUnavailable} {
		if _, ok := vocab[code]; !ok {
			t.Errorf("door code %q is not in oai.Codes()", code)
		}
	}
	for name, rej := range map[string]ModelAuthRejection{
		"invalid": rejInvalid, "locked": rejLocked, "proxy": rejProxy,
		"overloaded": rejOverloaded, "unavailable": rejUnavailable, "internal": rejInternal,
	} {
		status, ok := vocab[rej.Code]
		switch {
		case !ok:
			t.Errorf("rejection %s: code %q is not in oai.Codes()", name, rej.Code)
		case status != rej.Status:
			t.Errorf("rejection %s: status %d, the vocabulary binds %q to %d", name, rej.Status, rej.Code, status)
		}
	}
}
