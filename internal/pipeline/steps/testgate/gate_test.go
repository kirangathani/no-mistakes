// Package testgate holds the Test step's diff-class gate tests that drive a
// whole step execution.
//
// They live outside internal/pipeline/steps for the reason citest does: each
// case stands up a git repository and runs the step, and on Windows those
// subprocess spawns cost roughly ten times what they cost on Linux. The
// parent package measured 647s of a 900s budget on the windows-steps shard
// before this gate existed, so seventeen more step executions there put it
// over the cap with no test failing. A sibling package gets its own budget
// and runs concurrently with the parent.
//
// The gate's cheap white-box unit tests (the pattern matcher, the fail-open
// classification, the PR line renderer) stay in the parent package, where
// they cost nothing and can reach package internals.
//
// What these tests pin: the live-evidence agent - the most expensive turn in
// the pipeline - runs only when the run's diff touches a product file, or
// when no earlier go verdict on this branch already covers the current
// product state. The agent-invocation count is the assertion that matters; a
// gate that records the right verdict while still paying for the turn has not
// done its job.
package testgate

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
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// passingScenarioFindingsJSON is a go verdict with one live passing scenario:
// the answer a real evidence turn gives when it ran and everything worked.
const passingScenarioFindingsJSON = `{
  "findings": [],
  "summary": "",
  "tested": ["npm run e2e -- checkout"],
  "testing_summary": "drove checkout end to end",
  "artifacts": [],
  "scenarios": [{"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""}],
  "verdict": "go"
}`

// gateAgent returns a mock agent that answers every evidence turn with a
// passing payload, so any invocation count above zero means the gate let the
// turn run.
func gateAgent() *stepstest.MockAgent {
	return &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
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
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", message)
	return stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
}

// gateContext builds a Test-step context whose head is headSHA and whose
// classification is the shipped default list, which is what a real run gets
// from config.Merge.
func gateContext(t *testing.T, ag agent.Agent, dir, baseSHA, headSHA string) *pipeline.StepContext {
	t.Helper()
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
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

// shortSHA mirrors the steps package's own abbreviation, which the reuse
// reason is rendered with.
func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
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
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	head := commitFiles(t, dir, "add a product file", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
		"docs/checkout.md":              "# checkout\n",
	})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1", len(ag.Calls))
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
			dir, baseSHA, _ := stepstest.SetupGitRepo(t)
			// feature.txt from the template repo is a product file, so remove
			// it to leave a diff of only this case's path class.
			if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
				t.Fatal(err)
			}
			head := commitFiles(t, dir, "non-product change", tc.files)
			ag := gateAgent()
			sctx := gateContext(t, ag, dir, baseSHA, head)

			outcome, err := (&steps.TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.Calls) != 0 {
				t.Fatalf("evidence agent invocations = %d, want 0 (prompt: %q)", len(ag.Calls), ag.Calls[0].Prompt)
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
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
		t.Fatal(err)
	}
	head := commitFiles(t, dir, "docs only", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	sctx.Config.Commands.Test = "exit 3"

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("evidence agent invocations = %d, want 0", len(ag.Calls))
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
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	priorRunID := recordPriorGoVerdict(t, sctx, validated)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("evidence agent invocations = %d, want 0", len(ag.Calls))
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
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "another product change", map[string]string{
		"internal/checkout/total.go": "package checkout\n",
	})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	recordPriorGoVerdict(t, sctx, validated)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1", len(ag.Calls))
	}
	if got := parseOutcomeFindings(t, outcome).EvidenceSource; got != types.TestEvidenceSourceAgent {
		t.Fatalf("evidence source = %q, want %q", got, types.TestEvidenceSourceAgent)
	}
}

// TestTestStep_NonGoVerdictIsNeverReused keeps the gate from restating a
// conclusion a human has not resolved.
func TestTestStep_NonGoVerdictIsNeverReused(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
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

	if _, err := (&steps.TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (an inconclusive verdict is not reusable)", len(ag.Calls))
	}
}

// TestTestStep_AnotherBranchesGoVerdictIsNeverReused keeps evidence scoped to
// the branch that earned it.
func TestTestStep_AnotherBranchesGoVerdictIsNeverReused(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
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

	if _, err := (&steps.TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (another branch's verdict is not reusable)", len(ag.Calls))
	}
}

// TestTestStep_FailingBaselineDefeatsReuse: restating an earlier go while this
// run's configured command is red would contradict the run's own evidence.
func TestTestStep_FailingBaselineDefeatsReuse(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	sctx.Config.Commands.Test = "exit 1"
	recordPriorGoVerdict(t, sctx, validated)

	if _, err := (&steps.TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (a red baseline defeats reuse)", len(ag.Calls))
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
			dir, baseSHA, _ := stepstest.SetupGitRepo(t)
			if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
				t.Fatal(err)
			}
			head := commitFiles(t, dir, "generated change", map[string]string{"gen/api.pb.go": "package gen\n"})
			ag := gateAgent()
			sctx := gateContext(t, ag, dir, baseSHA, head)
			sctx.Config.Test.NonProductPaths = tc.nonProduct

			outcome, err := (&steps.TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.Calls) != tc.wantCalls {
				t.Fatalf("evidence agent invocations = %d, want %d", len(ag.Calls), tc.wantCalls)
			}
			if got := parseOutcomeFindings(t, outcome).Verdict; got != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q", got, tc.wantVerdict)
			}
		})
	}
}
