package config

import (
	"strings"
	"testing"
)

func TestEffectiveRepoConfig_TestModeTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Test: TestRaw{Mode: TestModeWeak}}
	for _, allow := range []bool{false, true} {
		if got := EffectiveRepoConfig(pushed, &RepoConfig{}, allow).Test.Mode; got != "" {
			t.Fatalf("allow_repo_commands=%v: a pushed test.mode weak must be ignored, got %q", allow, got)
		}
	}
	if got := EffectiveRepoConfig(pushed, nil, false).Test.Mode; got != "" {
		t.Fatalf("without a trusted copy test.mode must be dropped, got %q", got)
	}
	trusted := &RepoConfig{Test: TestRaw{Mode: TestModeWeak}}
	if got := EffectiveRepoConfig(&RepoConfig{}, trusted, false).Test.Mode; got != TestModeWeak {
		t.Fatalf("trusted test.mode = %q, want weak", got)
	}
}

func TestMerge_TestModeDefaultsFullAndComesFromTheRepo(t *testing.T) {
	global := DefaultGlobalConfig()
	if got := Merge(global, &RepoConfig{}).Test.Mode; got != "" {
		t.Fatalf("default test mode = %q, want full (\"\")", got)
	}
	if got := Merge(global, &RepoConfig{Test: TestRaw{Mode: "full"}}).Test.Mode; got != "" {
		t.Fatalf("explicit full resolved to %q, want \"\"", got)
	}
	if got := Merge(global, &RepoConfig{Test: TestRaw{Mode: TestModeWeak}}).Test.Mode; got != TestModeWeak {
		t.Fatalf("repo weak resolved to %q", got)
	}
}

func TestLoadRepo_TestMode(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("test:\n  mode: weak\ncommands:\n  test: go test ./...\n  test_related: ./related.sh\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Test.Mode != TestModeWeak || cfg.Commands.TestRelated != "./related.sh" {
		t.Fatalf("parsed mode=%q test_related=%q", cfg.Test.Mode, cfg.Commands.TestRelated)
	}
	if _, err := LoadRepoFromBytes([]byte("test:\n  mode: lax\n")); err == nil || !strings.Contains(err.Error(), "test.mode") {
		t.Fatalf("unknown test.mode must fail closed, got %v", err)
	}
	if _, err := LoadRepoFromBytes([]byte("no_ci: true\ntest:\n  mode: weak\n")); err == nil || !strings.Contains(err.Error(), "no_ci") {
		t.Fatalf("weak with no_ci must be refused, got %v", err)
	}
}
