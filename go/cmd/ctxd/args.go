// The command line of ctxd (#246). ctxd boots on an EMPTY command line and on
// nothing else: every argument either selects a mode that ends the process, or
// is a usage error that ends it before anything is opened — no logger, no
// configuration, no database, no network.
//
// The rule exists because the binary used to ignore what it did not know, and
// "ignore" meant "boot". `docker exec ctx /ctx --version`, run to read the
// version of a live container, started a second daemon against the live
// database (pool, migration bootstrap) until a closed pipe killed it. An
// argument is now either understood or refused, never passed over.
//
// One table, one decision: parseArgs is the only place that reads argv, and
// main hands it the command line as its first statement. A mode takes exactly
// one argument; anything after it is refused like an unknown argument, because
// "ignore the rest" is the habit this file ends.

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/provenance"
)

// daemonName is the program name the command-line answers carry. Fixed rather
// than taken from argv[0]: the image installs the daemon as /ctx, the name of
// the CLI, and a version line reading "ctx v5.x" would describe the wrong
// program.
const daemonName = "ctxd"

// exitUsage is the exit code of a command line ctxd does not understand — 2,
// the code Go's flag package and the Unix tools use for a usage error. Kept
// apart from the 1 of a mode or boot that ran and failed, so a script can tell
// "called wrong" from "ran and failed".
const exitUsage = 2

// argMode is what a command line asks ctxd to do.
type argMode int

const (
	modeBoot argMode = iota // the empty command line: start the daemon
	modeVersion
	modeHelp
	modeHealth
	modeSecretDecrypt
	modeOverviewWorker
	modeUsageError
)

// argModes maps the one argument of every mode. The version and help spellings
// follow Go's flag convention (one dash or two) plus the bare word the issue
// asks for; the three older modes keep the single spelling their callers use —
// the compose healthcheck (`/ctx -health`), break-glass.sh
// (`-secret-decrypt`) and the scheduler's re-exec of its own binary
// (overview.WorkerCommand, events.rebuildViaWorker).
var argModes = map[string]argMode{
	"--version":            modeVersion,
	"-version":             modeVersion,
	"version":              modeVersion,
	"--help":               modeHelp,
	"-help":                modeHelp,
	"-h":                   modeHelp,
	"-health":              modeHealth,
	"-secret-decrypt":      modeSecretDecrypt,
	overview.WorkerCommand: modeOverviewWorker,
}

// usageText is the help (stdout) and the tail of every usage error (stderr).
// The overview worker is deliberately absent: it is the scheduler's internal
// IPC, not an operator's command.
const usageText = `usage: ctxd                   start the daemon; configuration comes from the environment (docs/operations.md)
       ctxd --version         print the build version (the value GET /health reports) and exit
       ctxd --help            print this help and exit
       ctxd -health           probe the local server; exit 0 while it serves (the container healthcheck)
       ctxd -secret-decrypt   decrypt one sealed settings record read from stdin (break-glass.sh)
In the image the binary is /ctx: docker run --rm <image> --version
`

// parseArgs classifies a command line (argv without the program name). No
// argument is the boot; one known argument is its mode; everything else — an
// unknown argument, an empty one, anything after a mode — is a usage error.
func parseArgs(args []string) argMode {
	if len(args) == 0 {
		return modeBoot
	}
	mode, known := argModes[args[0]]
	if !known || len(args) > 1 {
		return modeUsageError
	}
	return mode
}

// answerArgs answers the command lines that need no mode of their own —
// version, help, usage error — and returns the exit code. The version line is
// "ctxd <version>": the program name first like `ctx version`, then exactly
// the value GET /health and the boot line report, so `awk '{print $2}'` reads
// the same string a health probe would.
func answerArgs(mode argMode, args []string, stdout, stderr io.Writer) int {
	switch mode {
	case modeVersion:
		_, _ = fmt.Fprintf(stdout, "%s %s\n", daemonName, provenance.BuildVersion())
		return 0
	case modeHelp:
		_, _ = io.WriteString(stdout, usageText)
		return 0
	default:
		if _, known := argModes[args[0]]; known {
			_, _ = fmt.Fprintf(stderr, "%s: %s takes no further arguments, got %q\n", daemonName, args[0], args[1:])
		} else {
			_, _ = fmt.Fprintf(stderr, "%s: unknown argument %q\n", daemonName, args[0])
		}
		_, _ = io.WriteString(stderr, usageText)
		return exitUsage
	}
}

// dispatchArgs runs what the command line asks for and NEVER RETURNS unless it
// asks for the boot (os.Exit — the -health precedent). It is main's first
// statement, so no mode and no refusal ever finds a logger, a signal handler
// or a pool already set up.
func dispatchArgs(args []string) {
	switch mode := parseArgs(args); mode {
	case modeBoot:
		return
	case modeHealth:
		// Container health probe: serving semantics (E9), see healthmode.go.
		os.Exit(runHealthCheck(healthCheckURL(os.Getenv), os.Stderr))
	case modeSecretDecrypt:
		// Break-glass decrypt: one nonce_b64:ct_b64:name:scope record from
		// stdin, env-only key (CTX_SECRETS_KEY[_PREV]), no DB — see sealbox.go
		// and break-glass.sh at the repo root.
		os.Exit(runSecretDecrypt(os.Stdin, os.Stdout, os.Stderr))
	case modeOverviewWorker:
		// Hidden overview-rebuild worker (E-A, design/05 §4.7), see
		// overviewworker.go.
		os.Exit(overviewWorkerMode())
	default:
		os.Exit(answerArgs(mode, args, os.Stdout, os.Stderr))
	}
}
