// Package clientconfig resolves the credentials a ctx client talks to a ctxd
// with: the base URL and the API key. ENV beats the config file, the config
// file lives under $XDG_CONFIG_HOME/ctx, %APPDATA%\ctx or ~/.config/ctx, and
// WEBHOOK_BASE_URL remains the legacy base-URL fallback.
//
// One resolution rule for the whole toolchain — internal/cli for `ctx`,
// cmd/ctx-armsweep directly — so an operator never has to remember which
// binary reads the config file and which does not. Stdlib-only on purpose: a
// tool that merely needs to find those two values must not link the command
// tree, and with it cobra and the whole charm stack, to get them.
package clientconfig

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the CLI configuration.
type Config struct {
	BaseURL string // CTX_BASE_URL
	Key     string // CTX_KEY
}

// Load loads configuration with priority: ENV > ~/.config/ctx/config.
func Load() (Config, error) {
	cfg := Config{
		BaseURL: os.Getenv("CTX_BASE_URL"),
		Key:     os.Getenv("CTX_KEY"),
	}

	// If env vars are set, use them
	if cfg.BaseURL != "" && cfg.Key != "" {
		return cfg, nil
	}

	// Try config file
	configPath := FilePath()
	if err := loadConfigFile(configPath, &cfg); err != nil {
		// File not found is ok, other errors are not
		if !os.IsNotExist(err) {
			return cfg, fmt.Errorf("reading config %s: %w", configPath, err)
		}
	}

	// Legacy fallback: WEBHOOK_BASE_URL
	if cfg.BaseURL == "" {
		if v := os.Getenv("WEBHOOK_BASE_URL"); v != "" {
			cfg.BaseURL = v
		}
	}

	if cfg.BaseURL == "" {
		return cfg, fmt.Errorf("no config found. Create %s with CTX_BASE_URL and CTX_KEY", configPath)
	}
	if cfg.Key == "" {
		return cfg, fmt.Errorf("CTX_KEY not set. Add it to %s", configPath)
	}

	return cfg, nil
}

// FilePath is the ctx config FILE inside BaseDir.
func FilePath() string {
	return filepath.Join(BaseDir(), "config")
}

// BaseDir is the ctx config DIRECTORY (parent of the config file):
// $XDG_CONFIG_HOME/ctx, %APPDATA%\ctx on Windows, else ~/.config/ctx. It is the
// single source of truth for where ctx keeps user state — the per-project
// repo-agent keys (`ctx project init`, I-I) live UNDER it in projects/, never in
// the working directory (a secret in the CWD is a commit-into-the-repo hazard,
// design/02 §4.6 step 4).
func BaseDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "ctx")
	}
	// Windows: %APPDATA%\ctx
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		return filepath.Join(appdata, "ctx")
	}
	// Unix: ~/.config/ctx
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.Getenv("HOME"), ".config", "ctx")
	}
	return filepath.Join(home, ".config", "ctx")
}

// loadConfigFile reads a Key=Value file (bash source-compatible).
func loadConfigFile(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip quotes
		val = strings.Trim(val, `"'`)

		switch key {
		case "CTX_BASE_URL":
			if cfg.BaseURL == "" {
				cfg.BaseURL = val
			}
		case "CTX_KEY":
			if cfg.Key == "" {
				cfg.Key = val
			}
		}
	}
	return scanner.Err()
}
