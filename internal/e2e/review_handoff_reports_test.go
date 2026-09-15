//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeHandoffReportScenario is a reviewer whose ONLY observations are wording
// and a deterministic type error: exactly the round the handoff reports exist
// to stop paying for. It reports no findings, records three doc notes and one
// lint note, and the housekeeping pass then reports what it applied.
func writeHandoffReportScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "handoff-report-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "wording and lint only"
    structured:
      findings: []
      summary: "nothing blocking"
      risk_level: low
      risk_rationale: "prose and a mechanical type error only"
      risk_scope: source-or-external
      doc_report:
        - id: doc1
          file: README.md
          line: 12
          problem: "README still says the flag defaults to false"
          right_looks_like: "state the new default in the flag table"
        - id: doc2
          file: internal/example/flag.go
          line: 3
          problem: "the comment overstates what the guard rejects"
          right_looks_like: "say it rejects only unauthenticated callers"
        - id: doc3
          file: SETUP.md
          line: 4
          problem: "the setup list is missing the new step"
          right_looks_like: "add the migrate step"
      lint_report:
        - id: lint1
          file: internal/example/flag.go
          line: 9
          problem: "the declared return type does not satisfy the interface"
          right_looks_like: "return the interface type"
  - match: "Perform the combined documentation and lint housekeeping pass for this change."
    text: "housekeeping applied the notes"
    edits:
      - path: "README.md"
        new: "# Example\n\nThe flag defaults to true.\n"
    structured:
      findings: []
      summary: "apply review handoff notes"
      applied_notes:
        - id: doc1
          applied: true
          note: "stated the new default in the flag table"
        - id: doc2
          applied: true
          note: "narrowed the comment to unauthenticated callers"
        - id: doc3
          applied: false
          note: "SETUP.md documents a different install path and is not stale"
        - id: lint1
          applied: true
          note: "returned the interface type"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: example flag"
      body: |
        ## Summary

        Add the example flag.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write handoff report scenario: %v", err)
	}
	return path
}

// TestReviewHandoffReportsJourney is the end-to-end proof of the reports: a
// review round whose only observations are wording and a deterministic type
// error completes the whole pipeline without ever parking, the notes reach the
// step that owns the remedy, and the PR body records what each step did with
// them.
//
// The measured alternative is what this replaces: the same observations
// reported as findings park the run for a human, and any fix then buys a full
// cold re-review of the change, because a reviewer session never spans a code
// change.
func TestReviewHandoffReportsJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeHandoffReportScenario(t)})

	// A GitHub-shaped origin plus the gh stub, so the PR step really creates a
	// pull request and its body can be read off the create invocation.
	parentURL := "https://github.com/example/no-mistakes.git"
	forkURL := "https://github.com/example-fork/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(t.Context(), forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set GitHub origin: %v\n%s", err, out)
	}
	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-handoff-reports.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")

	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/handoff-reports"
	h.CommitChange(branch, "internal/example/flag.go", "package example\n\n// the guard rejects every caller\n", "add flag behavior")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	// The whole point: wording and lint-catchable observations must not park.
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed without a park (error=%v)", run.Status, deref(run.Error))
	}

	// The reviewer has to be told the boundary, and the owning step has to
	// receive the notes.
	invocations := h.AgentInvocations()
	reviewTurn := findInvocationContaining(invocations, "Review the code changes and return structured findings")
	for _, want := range []string{"Handoff reports", "doc_report", "lint_report"} {
		if !strings.Contains(reviewTurn, want) {
			t.Fatalf("review prompt missing %q:\n%s", want, reviewTurn)
		}
	}
	housekeeping := findInvocationContaining(invocations, "Perform the combined documentation and lint housekeeping pass")
	for _, want := range []string{
		"Doc report from the review step",
		"README still says the flag defaults to false",
		"Lint report from the review step",
		"the declared return type does not satisfy the interface",
		"applied_notes",
	} {
		if !strings.Contains(housekeeping, want) {
			t.Fatalf("housekeeping prompt missing %q:\n%s", want, housekeeping)
		}
	}

	// And the PR records the handoff, including the note the step declined.
	body := createdPRBody(t, readGHStubInvocations(t, ghLog))
	for _, want := range []string{
		"### Review handoff reports",
		"**Doc report** (3 note(s), 2 applied by the document step)",
		"**Lint report** (1 note(s), 1 applied by the lint step)",
		"stated the new default in the flag table",
		"**Not applied:** SETUP.md documents a different install path and is not stale",
		"returned the interface type",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PR body missing %q:\n%s", want, body)
		}
	}
}
