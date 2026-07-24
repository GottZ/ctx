package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/GottZ/ctx/internal/cli"
	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	root := &cobra.Command{
		Use:   "ctx",
		Short: "Your AI's save game",
		Long:  "ctx — The memory your LLM pretends to have. By GottZ.",
	}

	cli.Version = version
	cli.RegisterCommands(root)

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(versionString())
		},
	})

	if err := root.Execute(); err != nil {
		// design/03 §7 W03-4 Gate 5: `ctx contract` needs differentiated
		// exit codes (0/1/2/3), not cobra's uniform exit(1) — an
		// ExitCodeError carries its own code through here. ADDITIVE: every
		// command that does not return this type keeps the prior exit(1)
		// (cli.ExitCodeError doc has the full rationale).
		var ece *cli.ExitCodeError
		if errors.As(err, &ece) {
			os.Exit(ece.Code)
		}
		os.Exit(1)
	}
}

func versionString() string {
	if version != "dev" {
		return fmt.Sprintf("ctx %s (%s, %s)", version, commit, date)
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return fmt.Sprintf("ctx %s (go install)", info.Main.Version)
	}
	return "ctx dev (unknown build)"
}
