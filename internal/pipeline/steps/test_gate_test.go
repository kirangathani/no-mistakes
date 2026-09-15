package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These tests pin the diff-class gate: the live-evidence agent - the most
// expensive turn in the pipeline - runs only when the run's diff touches a
// product file, or when no earlier go verdict on this branch already covers
// the current product state. The agent-invocation count is the assertion that
// matters; a gate that records the right verdict while still paying for the
// turn has not done its job.

// gateAgent returns a mock agent that answers every evidence turn with a
// passing payload, so any invocation count above zero means the gate let the
// turn run.
func gateAgent() *mockAgent {
	return &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
}

// commitFiles writes files onto the current branch of dir and returns the new
// head SHA, so a case can shape the run's diff by path class.
func commitFiles(t *testing.T, dir, message string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", message)
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

// gateContext builds a Test-step context whose head is headSHA and whose
// classification is the shipped default list, which is what a real run gets
// from config.Merge.
func gateContext(t *testing.T, ag agent.Agent, dir, baseSHA, headSHA string) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.HeadSHA = headSHA
	sctx.Config.Test.NonProductPaths = append([]string(nil), config.DefaultNonProductPaths...)
	return sctx
}

func parseOutcomeFindings(t *testing.T, outcome *pipeline.StepOutcome) types.Findings {
	t.Helper()
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse step findings: %v\n%s", err, outcome.Findings)
	}
	return findings
}

// recordPriorGoVerdict inserts a completed Test step on ANOTHER run of the
// same branch that recorded a go verdict at headSHA, which is the evidence a
// later run may reuse.
func recordPriorGoVerdict(t *testing.T, sctx *pipeline.StepContext, headSHA string) string {
	t.Helper()
	run, err := sctx.DB.InsertRun(sctx.Run.RepoID, sctx.Run.Branch, headSHA, sctx.Run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	step, err := sctx.DB.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := types.Findings{
		Tested:         []string{"npm run e2e -- checkout"},
		TestingSummary: "drove checkout end to end",
		Scenarios: []types.TestScenario{{
			Name:     "user reaches the success screen",
			Result:   types.ScenarioResultPass,
			Live:     true,
			Evidence: "checkout.png",
		}},
		Verdict:        types.TestVerdictGo,
		TestedHeadSHA:  headSHA,
		EvidenceSource: types.TestEvidenceSourceAgent,
	}
	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(step.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStep(step.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// TestTestStep_ProductChangeRunsTheEvidenceAgent is the control: a change that
// touches a product file still pays for the live-evidence turn.
func TestTestStep_ProductChangeRunsTheEvidenceAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	head := commitFiles(t, dir, "add a product file", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
		"docs/checkout.md":              "# checkout\n",
	})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1", len(ag.calls))
	}
	findings := parseOutcomeFindings(t, outcome)
	if findings.EvidenceSource != types.TestEvidenceSourceAgent {
		t.Fatalf("evidence source = %q, want %q", findings.EvidenceSource, types.TestEvidenceSourceAgent)
	}
}

// TestTestStep_NonProductOnlyChangeSkipsTheAgentWithoutParking is the gate's
// whole point. A docs-only run has no live-drivable surface by construction,
// so it records an AUTOMATIC no-surface and proceeds; the agent's own
// no-surface still parks, which TestTestStep_VerdictPolicy pins.
func TestTestStep_NonProductOnlyChangeSkipsTheAgentWithoutParking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{name: "docs only", files: map[string]string{"docs/guide.md": "# guide\n", "README.md": "hi\n"}},
		{name: "tests and fixtures only", files: map[string]string{
			"internal/checkout/checkout_test.go":    "package checkout\n",
			"internal/checkout/testdata/golden.txt": "golden\n",
		}},
		{name: "ci workflow only", files: map[string]string{".github/workflows/ci.yml": "name: ci\n"}},
		{name: "scripts only", files: map[string]string{"scripts/release.sh": "#!/bin/sh\n"}},
		{name: "lockfile only", files: map[string]string{"go.sum": "h1:abc\n"}},
		{name: "pipeline config only", files: map[string]string{".no-mistakes.yaml": "commands:\n  test: go test ./...\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := setupGitRepo(t)
			// feature.txt from the template repo is a product file, so remove
			// it to leave a diff of only this case's path class.
			if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
				t.Fatal(err)
			}
			head := commitFiles(t, dir, "non-product change", tc.files)
			ag := gateAgent()
			sctx := gateContext(t, ag, dir, baseSHA, head)

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) != 0 {
				t.Fatalf("evidence agent invocations = %d, want 0 (prompt: %q)", len(ag.calls), ag.calls[0].Prompt)
			}
			if outcome.NeedsApproval {
				t.Fatal("an automatic no-surface must not park the step")
			}
			findings := parseOutcomeFindings(t, outcome)
			if findings.Verdict != types.TestVerdictNoSurface {
				t.Fatalf("verdict = %q, want %q", findings.Verdict, types.TestVerdictNoSurface)
			}
			if findings.EvidenceSource != types.TestEvidenceSourceNoProductChange {
				t.Fatalf("evidence source = %q, want %q", findings.EvidenceSource, types.TestEvidenceSourceNoProductChange)
			}
			if findings.EvidenceReason != "no product file in diff" {
				t.Fatalf("evidence reason = %q, want %q", findings.EvidenceReason, "no product file in diff")
			}
			if len(findings.Items) != 0 {
				t.Fatalf("findings = %+v, want none", findings.Items)
			}
		})
	}
}

// TestTestStep_ConfiguredTestCommandStillRunsWhenTheAgentIsSkipped pins the
// half of the ruling the gate must never touch: commands.test runs on every
// run, and its failure still parks the step even though the evidence agent
// was skipped.
func TestTestStep_ConfiguredTestCommandStillRunsWhenTheAgentIsSkipped(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
		t.Fatal(err)
	}
	head := commitFiles(t, dir, "docs only", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	sctx.Config.Commands.Test = "exit 3"

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("evidence agent invocations = %d, want 0", len(ag.calls))
	}
	if outcome.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3 from the configured test command", outcome.ExitCode)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a failing configured test command must still park the step")
	}
	findings := parseOutcomeFindings(t, outcome)
	if len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryTestCommand {
		t.Fatalf("findings = %+v, want one test-command finding", findings.Items)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "exit 3" {
		t.Fatalf("tested = %v, want the configured command", findings.Tested)
	}
}

// TestTestStep_ReusesAGoVerdictWhenProductFilesAreUnchanged covers the
// decision-only re-run: the median task needs four runs, and re-buying
// identical evidence on each is what the gate removes.
func TestTestStep_ReusesAGoVerdictWhenProductFilesAreUnchanged(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	priorRunID := recordPriorGoVerdict(t, sctx, validated)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("evidence agent invocations = %d, want 0", len(ag.calls))
	}
	if outcome.NeedsApproval {
		t.Fatal("a reused go verdict must not park the step")
	}
	findings := parseOutcomeFindings(t, outcome)
	if findings.Verdict != types.TestVerdictGo {
		t.Fatalf("verdict = %q, want %q", findings.Verdict, types.TestVerdictGo)
	}
	if findings.EvidenceSource != types.TestEvidenceSourceReused {
		t.Fatalf("evidence source = %q, want %q", findings.EvidenceSource, types.TestEvidenceSourceReused)
	}
	for _, want := range []string{
		"product files unchanged since " + shortSHA(validated),
		"reused from run " + priorRunID,
		filepath.Join(filepath.Dir(sctx.EvidenceDir), priorRunID),
	} {
		if !strings.Contains(findings.EvidenceReason, want) {
			t.Fatalf("evidence reason %q omits %q", findings.EvidenceReason, want)
		}
	}
	if len(findings.Scenarios) != 1 || findings.Scenarios[0].Name != "user reaches the success screen" {
		t.Fatalf("scenarios = %+v, want the reused run's scenario list", findings.Scenarios)
	}
	if findings.TestedHeadSHA != head {
		t.Fatalf("tested head = %q, want this run's head %q", findings.TestedHeadSHA, head)
	}
}

// TestTestStep_ProductChangeSinceAGoVerdictRerunsTheAgent is the other half of
// reuse: the moment a product file moves, the prior verdict stops describing
// this head and the agent runs.
func TestTestStep_ProductChangeSinceAGoVerdictRerunsTheAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "another product change", map[string]string{
		"internal/checkout/total.go": "package checkout\n",
	})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	recordPriorGoVerdict(t, sctx, validated)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1", len(ag.calls))
	}
	if got := parseOutcomeFindings(t, outcome).EvidenceSource; got != types.TestEvidenceSourceAgent {
		t.Fatalf("evidence source = %q, want %q", got, types.TestEvidenceSourceAgent)
	}
}

// TestTestStep_NonGoVerdictIsNeverReused keeps the gate from restating a
// conclusion a human has not resolved.
func TestTestStep_NonGoVerdictIsNeverReused(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	run, err := sctx.DB.InsertRun(sctx.Run.RepoID, sctx.Run.Branch, validated, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	step, err := sctx.DB.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(types.Findings{Verdict: types.TestVerdictInconclusive, TestedHeadSHA: validated})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(step.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStep(step.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (an inconclusive verdict is not reusable)", len(ag.calls))
	}
}

// TestTestStep_AnotherBranchesGoVerdictIsNeverReused keeps evidence scoped to
// the branch that earned it.
func TestTestStep_AnotherBranchesGoVerdictIsNeverReused(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	branch := sctx.Run.Branch
	sctx.Run.Branch = "refs/heads/other"
	recordPriorGoVerdict(t, sctx, validated)
	sctx.Run.Branch = branch

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (another branch's verdict is not reusable)", len(ag.calls))
	}
}

// TestTestStep_FailingBaselineDefeatsReuse: restating an earlier go while this
// run's configured command is red would contradict the run's own evidence.
func TestTestStep_FailingBaselineDefeatsReuse(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	sctx.Config.Commands.Test = "exit 1"
	recordPriorGoVerdict(t, sctx, validated)

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (a red baseline defeats reuse)", len(ag.calls))
	}
}

// TestTestStep_RepoConfigOverridesTheNonProductClassification proves the
// classification is the repository's to set: a repo that declares its own
// generated directory non-product skips the turn for a change confined to it,
// and a repo that declares an empty list opts every path back into product.
func TestTestStep_RepoConfigOverridesTheNonProductClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		nonProduct  []string
		wantCalls   int
		wantVerdict string
	}{
		{
			name:        "repository declares its generated tree non-product",
			nonProduct:  []string{"gen/**"},
			wantCalls:   0,
			wantVerdict: types.TestVerdictNoSurface,
		},
		{
			name:        "repository opts every path back into product",
			nonProduct:  []string{},
			wantCalls:   1,
			wantVerdict: types.TestVerdictGo,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := setupGitRepo(t)
			if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
				t.Fatal(err)
			}
			head := commitFiles(t, dir, "generated change", map[string]string{"gen/api.pb.go": "package gen\n"})
			ag := gateAgent()
			sctx := gateContext(t, ag, dir, baseSHA, head)
			sctx.Config.Test.NonProductPaths = tc.nonProduct

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) != tc.wantCalls {
				t.Fatalf("evidence agent invocations = %d, want %d", len(ag.calls), tc.wantCalls)
			}
			if got := parseOutcomeFindings(t, outcome).Verdict; got != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q", got, tc.wantVerdict)
			}
		})
	}
}

// TestMatchNonProductPattern covers the one matcher rule that ignore_patterns
// does not have: a leading "**/" matches at any depth, which is how the
// defaults reach a nested testdata or fixtures directory.
func TestMatchNonProductPattern(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file    string
		pattern string
		want    bool
	}{
		{file: "internal/foo/testdata/golden.txt", pattern: "**/testdata/**", want: true},
		{file: "testdata/golden.txt", pattern: "**/testdata/**", want: true},
		{file: "internal/testdata", pattern: "**/testdata/**", want: true},
		{file: "internal/foo/testdatabase/x.go", pattern: "**/testdata/**", want: false},
		{file: "docs/guide.md", pattern: "docs/**", want: true},
		{file: "internal/docs/guide.md", pattern: "docs/**", want: false},
		{file: "internal/a/b/README.md", pattern: "*.md", want: true},
		{file: "internal/checkout/checkout.go", pattern: "*.md", want: false},
	} {
		if got := matchNonProductPattern(tc.file, tc.pattern); got != tc.want {
			t.Errorf("matchNonProductPattern(%q, %q) = %v, want %v", tc.file, tc.pattern, got, tc.want)
		}
	}
}

// TestIsNonProductPath_NilClassificationRunsTheAgent pins the fail-open
// direction: with no classification resolved, nothing is non-product, so the
// gate never skips a turn it cannot justify skipping.
func TestIsNonProductPath_NilClassificationRunsTheAgent(t *testing.T) {
	t.Parallel()
	if isNonProductPath("docs/guide.md", nil) {
		t.Fatal("an unresolved classification must treat every path as product")
	}
}

// TestRenderLiveValidationLine_NamesTheEvidencePath keeps the PR's Testing
// section honest about WHY it has the verdict it shows: a reused go and a
// freshly driven go otherwise render identically.
func TestRenderLiveValidationLine_NamesTheEvidencePath(t *testing.T) {
	t.Parallel()
	scenarios := []types.TestScenario{{Name: "checkout", Result: types.ScenarioResultPass, Live: true}}

	line := renderLiveValidationLine(scenarios, types.TestVerdictGo, "product files unchanged since abc123; reused from run run-9 (evidence: /tmp/evidence/run-9)")
	for _, want := range []string{"go", "1 of 1 scenarios driven live", "reused from run run-9"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q omits %q", line, want)
		}
	}

	// A pre-gate payload renders exactly as before.
	if got := renderLiveValidationLine(scenarios, types.TestVerdictGo, ""); strings.Contains(got, "(") {
		t.Errorf("line %q should carry no evidence note", got)
	}

	// An automatic no-surface has no scenarios at all, so the reason is the
	// only thing worth rendering.
	got := renderLiveValidationLine(nil, types.TestVerdictNoSurface, "no product file in diff")
	if !strings.Contains(got, "no-surface") || !strings.Contains(got, "no product file in diff") {
		t.Errorf("line %q should name the automatic no-surface and its reason", got)
	}

	if got := renderLiveValidationLine(nil, "", ""); got != "" {
		t.Errorf("nothing recorded should render nothing, got %q", got)
	}
}
