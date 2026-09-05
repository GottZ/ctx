package cli

import "github.com/GottZ/ctx/internal/clientconfig"

// Config is the client configuration of the ctx CLI. An alias, not a wrapper
// type: the resolution itself lives in internal/clientconfig — the stdlib-only
// leaf the whole toolchain shares — while NewClient, stepServer, stepBackends
// and fetchBackendData keep spelling their parameter cli.Config.
type Config = clientconfig.Config
