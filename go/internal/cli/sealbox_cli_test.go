package cli

import (
	"strings"
	"testing"
)

// SECURITY PROPERTY (F2-W8): a secret value passed as a positional argument
// is already leaked (world-readable /proc/<pid>/cmdline, shell history) —
// set/rotate must refuse it and show the stdin pipe instead of accepting.
func TestSecretsArgvValueRejected(t *testing.T) {
	err := rejectArgvValue("prov-main")
	if err == nil {
		t.Fatal("argv value must be rejected")
	}
	for _, want := range []string{"stdin", "echo", "| ctx secrets set prov-main"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection lacks %q: %v", want, err)
		}
	}
}

// The command wiring enforces the same property end-to-end: two positional
// args on set/rotate run into rejectArgvValue before any client call.
func TestSecretsSetTwoArgsRejected(t *testing.T) {
	getClient := func() (*Client, error) {
		t.Fatal("client must never be built for an argv value")
		return nil, nil
	}
	cmd := secretsCmd(getClient)
	for _, sub := range []string{"set", "rotate"} {
		cmd.SetArgs([]string{sub, "prov-main", "sk-leaked-value"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "stdin") {
			t.Errorf("%s with argv value: err = %v, want stdin rejection", sub, err)
		}
	}
}
