package fedtest

import (
	"log/slog"
	"os"
	"testing"
)

// TestMain keeps the mesh runs readable: every failed attempt of a walk logs
// a WARN (llm.WalkChain), and a fan-out probe makes hundreds of them on
// purpose. Errors still show.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	os.Exit(m.Run())
}
