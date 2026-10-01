package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func weakModeAgent() *mockAgent {
	return &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
}

func TestTestStep_WeakModeSkipsTheSuiteButStillRunsEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	suite := "go env GOOS > suite.ran"
	ag := weakModeAgent()
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: suite})
	sctx.Config.Test.Mode = config.TestModeWeak

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "suite.ran")); !os.IsNotExist(err) {
		t.Fatalf("weak mode must not run commands.test locally (stat err %v)", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("evidence agent calls = %d, want 1", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "was NOT run locally and must not be run") {
		t.Fatalf("evidence prompt must say the suite is delegated to CI:\n%s", ag.calls[0].Prompt)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	for _, tested := range findings.Tested {
		if tested == suite {
			t.Fatalf("Tested lists the skipped suite: %v", findings.Tested)
		}
	}
	note := "Full test suite (`" + suite + "`) not run locally: test.mode is weak, so it is delegated to CI."
	if !strings.HasPrefix(findings.TestingSummary, note) {
		t.Fatalf("TestingSummary = %q, want the delegation note first", findings.TestingSummary)
	}
	if findings.Verdict != types.TestVerdictGo {
		t.Fatalf("verdict = %q, want the evidence agent's go", findings.Verdict)
	}
	steps, rounds := testStepWithFindings(t, outcome.Findings)
	if md := BuildTestingSummary(steps, rounds); !strings.Contains(md, "not run locally: test.mode is weak, so it is delegated to CI.") {
		t.Fatalf("PR Testing section must state the delegation:\n%s", md)
	}
}

func TestTestStep_WeakModeRunsTheRelatedCommandWithTheChangedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell expansion")
	}
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	related := `printf '%s\n%s' "$NO_MISTAKES_BASE_SHA" "$NO_MISTAKES_CHANGED_FILES" > related.log`
	sctx := newTestContextWithDBRecords(t, weakModeAgent(), dir, baseSHA, headSHA, config.Commands{Test: "touch suite.ran", TestRelated: related})
	sctx.Config.Test.Mode = config.TestModeWeak

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "suite.ran")); !os.IsNotExist(err) {
		t.Fatal("weak mode must not run commands.test locally")
	}
	data, err := os.ReadFile(filepath.Join(dir, "related.log"))
	if err != nil {
		t.Fatalf("commands.test_related did not run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := strings.Fields(gitCmd(t, dir, "diff", "--name-only", baseSHA+".."+headSHA))
	if len(lines) < 2 || lines[0] == "" || strings.Join(lines[1:], " ") != strings.Join(want, " ") {
		t.Fatalf("related env = %q, want base sha then changed files %v", lines, want)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Tested) == 0 || findings.Tested[0] != related {
		t.Fatalf("Tested = %v, want the related command first", findings.Tested)
	}

	// A red related command is the configured-command failure it always was.
	sctx2 := newTestContextWithDBRecords(t, weakModeAgent(), dir, baseSHA, headSHA, config.Commands{Test: "true", TestRelated: "exit 3"})
	sctx2.Config.Test.Mode = config.TestModeWeak
	outcome, err = (&TestStep{}).Execute(sctx2)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode != 3 || !outcome.NeedsApproval {
		t.Fatalf("failing related command: exit=%d needsApproval=%v, want 3/true", outcome.ExitCode, outcome.NeedsApproval)
	}
}

func TestTestStep_FullModeIgnoresTheRelatedCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, weakModeAgent(), dir, baseSHA, headSHA, config.Commands{Test: "go env GOOS > suite.ran", TestRelated: "go env GOOS > related.ran"})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "suite.ran")); err != nil {
		t.Fatalf("full mode must run commands.test: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "related.ran")); !os.IsNotExist(err) {
		t.Fatal("full mode must not run commands.test_related")
	}
	if strings.Contains(outcome.Findings, "delegated to CI") {
		t.Fatalf("full mode must not claim delegation: %s", outcome.Findings)
	}
}
