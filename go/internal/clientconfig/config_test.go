package clientconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("CTX_BASE_URL", "https://test.example.com")
	t.Setenv("CTX_KEY", "test-key-123")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.BaseURL != "https://test.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://test.example.com")
	}
	if cfg.Key != "test-key-123" {
		t.Errorf("Key = %q, want %q", cfg.Key, "test-key-123")
	}
}

func TestLoad_FileOnly(t *testing.T) {
	// Clear env vars
	t.Setenv("CTX_BASE_URL", "")
	t.Setenv("CTX_KEY", "")
	t.Setenv("WEBHOOK_BASE_URL", "")

	// Create temp config dir
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	configDir := filepath.Join(tmpDir, "ctx")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(configDir, "config")
	content := `CTX_BASE_URL=https://file.example.com
CTX_KEY=file-key-456
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.BaseURL != "https://file.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://file.example.com")
	}
	if cfg.Key != "file-key-456" {
		t.Errorf("Key = %q, want %q", cfg.Key, "file-key-456")
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	t.Setenv("CTX_BASE_URL", "https://env.example.com")
	t.Setenv("CTX_KEY", "env-key-789")

	// Also create a config file
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	configDir := filepath.Join(tmpDir, "ctx")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(configDir, "config")
	content := `CTX_BASE_URL=https://file.example.com
CTX_KEY=file-key-should-not-be-used
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.BaseURL != "https://env.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://env.example.com")
	}
	if cfg.Key != "env-key-789" {
		t.Errorf("Key = %q, want %q", cfg.Key, "env-key-789")
	}
}

func TestLoad_MissingBaseURL(t *testing.T) {
	t.Setenv("CTX_BASE_URL", "")
	t.Setenv("CTX_KEY", "")
	t.Setenv("WEBHOOK_BASE_URL", "")

	// No config file
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing config, got nil")
	}
}

func TestLoad_MissingKey(t *testing.T) {
	t.Setenv("CTX_BASE_URL", "https://test.example.com")
	t.Setenv("CTX_KEY", "")

	// No config file with key
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	configDir := filepath.Join(tmpDir, "ctx")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Config without CTX_KEY
	configFile := filepath.Join(configDir, "config")
	if err := os.WriteFile(configFile, []byte("# empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

func TestLoad_QuotedValues(t *testing.T) {
	t.Setenv("CTX_BASE_URL", "")
	t.Setenv("CTX_KEY", "")
	t.Setenv("WEBHOOK_BASE_URL", "")

	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	configDir := filepath.Join(tmpDir, "ctx")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(configDir, "config")
	content := `CTX_BASE_URL="https://quoted.example.com"
CTX_KEY='single-quoted-key'
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.BaseURL != "https://quoted.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://quoted.example.com")
	}
	if cfg.Key != "single-quoted-key" {
		t.Errorf("Key = %q, want %q", cfg.Key, "single-quoted-key")
	}
}

func TestLoad_CommentsAndBlanks(t *testing.T) {
	t.Setenv("CTX_BASE_URL", "")
	t.Setenv("CTX_KEY", "")
	t.Setenv("WEBHOOK_BASE_URL", "")

	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	configDir := filepath.Join(tmpDir, "ctx")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(configDir, "config")
	content := `# This is a comment

CTX_BASE_URL=https://comments.example.com
# Another comment
CTX_KEY=comments-key

`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.BaseURL != "https://comments.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://comments.example.com")
	}
	if cfg.Key != "comments-key" {
		t.Errorf("Key = %q, want %q", cfg.Key, "comments-key")
	}
}

func TestLoad_WebhookBaseURLFallback(t *testing.T) {
	t.Setenv("CTX_BASE_URL", "")
	t.Setenv("CTX_KEY", "")
	t.Setenv("WEBHOOK_BASE_URL", "https://webhook-fallback.example.com")

	// Need a config file with just the key
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	configDir := filepath.Join(tmpDir, "ctx")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(configDir, "config")
	content := `CTX_KEY=fallback-key
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.BaseURL != "https://webhook-fallback.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://webhook-fallback.example.com")
	}
}
