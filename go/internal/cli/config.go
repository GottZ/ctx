package cli

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

// LoadConfig loads configuration with priority: ENV > ~/.config/ctx/config.
func LoadConfig() (Config, error) {
	cfg := Config{
		BaseURL: os.Getenv("CTX_BASE_URL"),
		Key:     os.Getenv("CTX_KEY"),
	}

	// If env vars are set, use them
	if cfg.BaseURL != "" && cfg.Key != "" {
		return cfg, nil
	}

	// Try config file
	configPath := configFilePath()
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

func configFilePath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "ctx", "config")
	}
	// Windows: %APPDATA%\ctx\config
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		return filepath.Join(appdata, "ctx", "config")
	}
	// Unix: ~/.config/ctx/config
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.Getenv("HOME"), ".config", "ctx", "config")
	}
	return filepath.Join(home, ".config", "ctx", "config")
}

// loadConfigFile reads a Key=Value file (bash source-compatible).
func loadConfigFile(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

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
