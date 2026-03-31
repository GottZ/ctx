package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatBriefHuman(t *testing.T) {
	blocks := []briefBlock{
		{Title: "Architecture", Content: "Go monolith with PG18+pgvector."},
		{Title: "Scale Target", Content: "1M+ blocks, production-grade."},
	}

	out := formatBriefHuman(blocks)

	if !strings.Contains(out, "2 blocks") {
		t.Error("expected block count in header")
	}
	if !strings.Contains(out, "--- Architecture ---") {
		t.Error("expected title separator")
	}
	if !strings.Contains(out, "Go monolith") {
		t.Error("expected block content")
	}
	if !strings.Contains(out, "1M+ blocks") {
		t.Error("expected second block content")
	}
}

func TestFormatBriefHook(t *testing.T) {
	blocks := []briefBlock{
		{Title: "Architecture", Content: "Go monolith with PG18+pgvector."},
		{Title: "Constraints", Content: "24GB VRAM budget."},
	}

	out := formatBriefHook(blocks)

	var parsed hookOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if !parsed.Continue {
		t.Error("expected continue=true")
	}
	if parsed.HookSpecificOutput.HookEventName != "SubagentStart" {
		t.Errorf("wrong hookEventName: %s", parsed.HookSpecificOutput.HookEventName)
	}

	ctx := parsed.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "Go monolith") {
		t.Error("expected block content in additionalContext")
	}
	if !strings.Contains(ctx, "24GB VRAM") {
		t.Error("expected second block in additionalContext")
	}
}

func TestFormatBriefHumanEmpty(t *testing.T) {
	out := formatBriefHuman(nil)
	if !strings.Contains(out, "0 blocks") {
		t.Error("expected zero block count")
	}
}

func TestFormatBriefHookEmpty(t *testing.T) {
	out := formatBriefHook(nil)

	var parsed hookOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed.HookSpecificOutput.HookEventName != "SubagentStart" {
		t.Errorf("wrong hookEventName: %s", parsed.HookSpecificOutput.HookEventName)
	}
}

func TestResolveProject(t *testing.T) {
	// This test runs inside the ctx repo, so resolveProject should find .git
	cwd, err := os.Getwd()
	if err != nil {
		t.Skip("cannot get cwd")
	}

	project := resolveProject(cwd)

	// Should find a .git somewhere above us
	gitDir := filepath.Join(project, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		t.Errorf("resolveProject(%s) = %s, but %s does not exist", cwd, project, gitDir)
	}
}

func TestResolveProjectNonGit(t *testing.T) {
	// /tmp is unlikely to be a git repo
	project := resolveProject("/tmp")
	if project != "/tmp" {
		t.Errorf("expected /tmp fallback, got %s", project)
	}
}
