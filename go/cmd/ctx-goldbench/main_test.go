package main

import "testing"

// TestResolveAPIKey pinnt die Präzedenz, die der Harness bisher ungetestet
// inline in run() trug: das Flag schlägt die Umgebung, ohne beides bleibt der
// Key leer. Ein Wechsel der Reihenfolge würde einen Lauf still gegen den
// falschen Endpoint-Key fahren.
func TestResolveAPIKey(t *testing.T) {
	tests := []struct {
		name string
		flag string
		env  string
		want string
	}{
		{"nur_flag", "aus-dem-flag", "", "aus-dem-flag"},
		{"nur_env", "", "aus-der-umgebung", "aus-der-umgebung"},
		{"beides_flag_gewinnt", "aus-dem-flag", "aus-der-umgebung", "aus-dem-flag"},
		{"keins", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOLDBENCH_API_KEY", tt.env)
			if got := resolveAPIKey(tt.flag); got != tt.want {
				t.Errorf("resolveAPIKey(%q) mit GOLDBENCH_API_KEY=%q = %q, want %q",
					tt.flag, tt.env, got, tt.want)
			}
		})
	}
}
