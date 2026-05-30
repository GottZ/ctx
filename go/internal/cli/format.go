package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// StdoutIsTTY reports whether stdout is an interactive terminal (not piped or
// redirected). Mirrors ReadStdin's stdin probe via os.ModeCharDevice — stdlib
// only, no x/term dependency. Used to switch between human-readable rendering
// (TTY) and machine-readable JSON (pipe), so `ctx dream stats` stays scriptable.
func StdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// PrintJSON writes raw JSON to stdout with optional pretty-printing.
// If the data is valid JSON, it pretty-prints it; otherwise writes raw.
func PrintJSON(data []byte) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		// Not valid JSON, write raw
		_, _ = os.Stdout.Write(data)
		return
	}
	buf.WriteByte('\n')
	_, _ = os.Stdout.Write(buf.Bytes())
}

// PrintRaw writes raw bytes to stdout.
func PrintRaw(data []byte) {
	_, _ = os.Stdout.Write(data)
	// Ensure trailing newline
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Println()
	}
}

// Errorf prints to stderr.
func Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// ReadStdin reads all of stdin if it's piped (not a terminal).
func ReadStdin() (string, bool) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return "", false
	}
	if (fi.Mode() & os.ModeCharDevice) != 0 {
		// Terminal, not piped
		return "", false
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", false
	}
	s := string(data)
	// Trim trailing newline like bash
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s, len(s) > 0
}
