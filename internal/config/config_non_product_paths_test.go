package config

import (
	"slices"
	"strings"
	"testing"
)

// TestEffectiveRepoConfig_NonProductPathsTrustedOnly proves the diff-class
// classification is honored only from the trusted default-branch copy.
//
// It decides whether the live-evidence gate runs at all for a change, so a
// pushed branch that could set it could declare its own product code
// non-product and skip the live validation of exactly that code.
func TestEffectiveRepoConfig_NonProductPathsTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Test: TestRaw{NonProductPaths: []string{"internal/**"}}}
	trusted := &RepoConfig{Test: TestRaw{NonProductPaths: []string{"gen/**"}}}

	for _, allowRepoCommands := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
		if !slices.Equal(effective.Test.NonProductPaths, trusted.Test.NonProductPaths) {
			t.Fatalf("NonProductPaths = %v under allow_repo_commands=%v, want the trusted list", effective.Test.NonProductPaths, allowRepoCommands)
		}
	}

	// Without a trusted copy the pushed list is discarded, which restores the
	// built-in defaults rather than the branch's own classification.
	if got := EffectiveRepoConfig(pushed, nil, false).Test.NonProductPaths; got != nil {
		t.Fatalf("without a trusted copy the pushed classification must be dropped, got %v", got)
	}
	if !slices.Equal(pushed.Test.NonProductPaths, []string{"internal/**"}) {
		t.Fatal("pushed config was mutated")
	}
}

func TestMerge_NonProductPathsDefaultWhenUnset(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{})
	if !slices.Equal(got.Test.NonProductPaths, DefaultNonProductPaths) {
		t.Fatalf("NonProductPaths = %v, want the defaults", got.Test.NonProductPaths)
	}
	// The resolved slice must be a copy: a step that mutated it would rewrite
	// the defaults for every later run in the process.
	got.Test.NonProductPaths[0] = "mutated"
	if DefaultNonProductPaths[0] == "mutated" {
		t.Fatal("the resolved list aliases DefaultNonProductPaths")
	}
}

// An explicitly empty list is a deliberate opt-out - every path is product
// code - and must not silently fall back to the defaults.
func TestMerge_NonProductPathsEmptyListIsHonored(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{Test: TestRaw{NonProductPaths: []string{}}})
	if len(got.Test.NonProductPaths) != 0 {
		t.Fatalf("NonProductPaths = %v, want an empty classification", got.Test.NonProductPaths)
	}
}

func TestMerge_NonProductPathsTrimsAndDropsBlanks(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{Test: TestRaw{NonProductPaths: []string{"  gen/**  ", "   "}}})
	if !slices.Equal(got.Test.NonProductPaths, []string{"gen/**"}) {
		t.Fatalf("NonProductPaths = %v, want the trimmed single entry", got.Test.NonProductPaths)
	}
}

// The classification describes ONE repository's layout, so a global value has
// no repository to describe and must never leak into a run.
func TestMerge_GlobalNonProductPathsAreNotUsed(t *testing.T) {
	global := &GlobalConfig{Test: TestRaw{NonProductPaths: []string{"src/**"}}}
	got := Merge(global, &RepoConfig{})
	if !slices.Equal(got.Test.NonProductPaths, DefaultNonProductPaths) {
		t.Fatalf("global classification leaked into the resolved config: %v", got.Test.NonProductPaths)
	}
}

func TestLoadRepo_NonProductPaths(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("test:\n  non_product_paths:\n    - \"gen/**\"\n    - \"**/testdata/**\"\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !slices.Equal(cfg.Test.NonProductPaths, []string{"gen/**", "**/testdata/**"}) {
		t.Fatalf("NonProductPaths = %v", cfg.Test.NonProductPaths)
	}
}

// A glob that Match would reject matches nothing, which quietly turns the
// whole repository back into product code and restores the per-run evidence
// bill the gate exists to remove. Fail the config instead.
func TestLoadRepo_NonProductPathsRejectsUnusablePatterns(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{name: "malformed glob", yaml: "test:\n  non_product_paths:\n    - \"gen/[\"\n", want: "test.non_product_paths"},
		{name: "empty entry", yaml: "test:\n  non_product_paths:\n    - \"  \"\n", want: "must not be empty"},
		{name: "bare any-depth prefix", yaml: "test:\n  non_product_paths:\n    - \"**/\"\n", want: "needs a path after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected the config to fail closed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The defaults are the shipped answer to "what cannot carry a live-drivable
// change". A regression here silently changes which runs pay for the
// ~21-minute evidence turn, so the classes named in the ruling are pinned.
func TestDefaultNonProductPathsCoverEveryDeclaredClass(t *testing.T) {
	for _, pattern := range []string{
		"*.md", "docs/**", // docs and markdown
		"*_test.go", "**/testdata/**", // test files and fixtures
		".github/workflows/**", // CI workflow files
		"scripts/**",           // scripts and tooling directories
		"go.sum",               // lockfiles
		".no-mistakes.yaml",    // the pipeline's own config
	} {
		if !slices.Contains(DefaultNonProductPaths, pattern) {
			t.Errorf("DefaultNonProductPaths is missing %q", pattern)
		}
	}
	// Every default must itself be a usable pattern.
	if err := validateTestRaw(TestRaw{NonProductPaths: DefaultNonProductPaths}); err != nil {
		t.Fatalf("the shipped defaults do not validate: %v", err)
	}
}
