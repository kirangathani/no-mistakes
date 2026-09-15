// Package handoff holds the step-driving tests for the review step's doc and
// lint handoff reports (docs/src/content/docs/concepts/review-handoff-reports.md).
//
// They live outside internal/pipeline/steps for the reason citest and testgate
// do: each case stands up a git repository and executes a whole step, and on
// Windows those subprocess spawns cost roughly ten times what they cost on
// Linux, where the parent package already measured 647s of its 900s budget.
//
// What these tests pin: a wording-only or lint-only review does not park the
// run and does not spend a fix round - the observations leave as notes, and
// the document and lint steps receive them, act on them, and report which
// ones they applied. A defect and a simplification still park exactly as
// before.
package handoff

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func mustParse(t *testing.T, findingsJSON string) types.Findings {
	t.Helper()
	parsed, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		t.Fatalf("parse findings %q: %v", findingsJSON, err)
	}
	return parsed
}

func reviewAgent(payload string) *stepstest.MockAgent {
	return &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(payload)}, nil
		},
	}
}

// TestReviewStep_WordingOnlyReviewHandsOverDocNotesAndDoesNotPark is the
// measured case this whole feature exists for: a round spent on stale README
// prose and an overstated comment. Those observations must cost no park and no
// fix round, because either one buys a full cold re-review of the change.
func TestReviewStep_WordingOnlyReviewHandsOverDocNotesAndDoesNotPark(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := reviewAgent(`{
	  "findings": [],
	  "summary": "wording only",
	  "risk_level": "low",
	  "risk_rationale": "prose only",
	  "risk_scope": "source-or-external",
	  "doc_report": [
	    {"id":"doc1","file":"README.md","line":42,"problem":"README still says the flag defaults to false","right_looks_like":"state the new default (true) in the flag table"},
	    {"id":"doc2","file":"internal/api/router.go","line":88,"problem":"comment overstates what the guard does","right_looks_like":"say it rejects only unauthenticated callers"},
	    {"file":"SETUP.md","line":9,"problem":"setup list is missing the new step","right_looks_like":"add the migrate step"}
	  ]
	}`)
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&steps.ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if outcome.NeedsApproval {
		t.Error("a wording-only review parked the run; the doc report exists so it does not")
	}
	if outcome.AutoFixable {
		t.Error("a wording-only review offered a fix round; every fix round buys a cold re-review")
	}
	parsed := mustParse(t, outcome.Findings)
	if len(parsed.Items) != 0 {
		t.Errorf("findings = %+v, want none: wording is not a finding", parsed.Items)
	}
	if len(parsed.DocReport) != 3 {
		t.Fatalf("doc report = %+v, want 3 notes", parsed.DocReport)
	}
	// A note the reviewer left unlabelled still needs a handle a consuming
	// step can report an outcome for.
	if parsed.DocReport[2].ID == "" {
		t.Errorf("unlabelled note got no id: %+v", parsed.DocReport[2])
	}
	if len(parsed.LintReport) != 0 {
		t.Errorf("lint report = %+v, want empty", parsed.LintReport)
	}

	// The boundary has to be in the prompt, or the reviewer cannot honor it.
	prompt := ag.Calls[len(ag.Calls)-1].Prompt
	for _, want := range []string{"doc_report", "lint_report", "Handoff reports", "WRONG in a way the tool accepts"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt is missing %q", want)
		}
	}
}

// TestReviewStep_LintOnlyReviewHandsOverLintNotes covers the second class:
// what the project's own checker would catch deterministically.
func TestReviewStep_LintOnlyReviewHandsOverLintNotes(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := reviewAgent(`{
	  "findings": [],
	  "summary": "lint only",
	  "risk_level": "low",
	  "risk_rationale": "mechanical",
	  "risk_scope": "source-or-external",
	  "lint_report": [
	    {"id":"lint1","file":"internal/a/a.go","line":7,"problem":"declared return type does not satisfy the interface; the type checker rejects it","right_looks_like":"return the interface type"}
	  ]
	}`)
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&steps.ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome.NeedsApproval || outcome.AutoFixable {
		t.Errorf("a lint-only review parked or offered a fix round: %+v", outcome)
	}
	parsed := mustParse(t, outcome.Findings)
	if len(parsed.Items) != 0 {
		t.Errorf("findings = %+v, want none", parsed.Items)
	}
	if len(parsed.LintReport) != 1 || parsed.LintReport[0].ID != "lint1" {
		t.Fatalf("lint report = %+v, want the one note", parsed.LintReport)
	}
}

// TestReviewStep_DefectStillParks and its simplification sibling are the
// other half of the contract: nothing about the reports weakens what the
// review still blocks on.
func TestReviewStep_DefectStillParks(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := reviewAgent(`{
	  "findings": [{"id":"f-1","severity":"error","file":"internal/a/a.go","line":12,"description":"the comment says it returns nil on error but it returns a zero value, so a caller checking nil misses the failure","action":"auto-fix","review_scope":"source"}],
	  "summary": "one defect",
	  "risk_level": "high",
	  "risk_rationale": "silent wrong result",
	  "risk_scope": "source-or-external",
	  "doc_report": [{"id":"doc1","file":"README.md","line":3,"problem":"stale sentence","right_looks_like":"update it"}]
	}`)
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&steps.ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Error("a defect did not park: a misleading claim the tool accepts is a finding, not wording")
	}
	parsed := mustParse(t, outcome.Findings)
	if len(parsed.Items) != 1 {
		t.Fatalf("findings = %+v, want the defect", parsed.Items)
	}
	// The reports travel beside the findings, not instead of them.
	if len(parsed.DocReport) != 1 {
		t.Errorf("doc report lost on a parking round: %+v", parsed.DocReport)
	}
}

func TestReviewStep_SimplificationStillParks(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := reviewAgent(`{
	  "findings": [{"id":"f-1","severity":"warning","file":"internal/a/a.go","line":30,"description":"the second acceptance path is not required by the intent; remove it","action":"ask-user","review_scope":"source"}],
	  "summary": "one unrequired component",
	  "risk_level": "medium",
	  "risk_rationale": "extra surface",
	  "risk_scope": "source-or-external"
	}`)
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&steps.ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Error("a simplification did not park; simplifications were deliberately kept as findings")
	}
}

// recordReviewReports writes a completed review step row carrying the two
// reports, which is how the document and lint steps receive them: the DB row,
// not an in-memory handoff, so a daemon restart between Test and Document
// cannot lose them.
func recordReviewReports(t *testing.T, sctx *pipeline.StepContext, doc, lint []types.HandoffNote) {
	t.Helper()
	row, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := types.MarshalFindingsJSON(types.Findings{
		Summary: "reviewed", RiskLevel: "low", RiskRationale: "bounded",
		RiskScope: types.FindingsRiskScopeSourceOrExternal,
		DocReport: doc, LintReport: lint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(row.ID, encoded); err != nil {
		t.Fatal(err)
	}
}

// TestDocumentStep_ReceivesTheDocReportAndReportsWhatItApplied proves the
// handoff actually lands: the notes reach the prompt, and the step's own row
// records per-note outcomes, including the note it declined.
func TestDocumentStep_ReceivesTheDocReportAndReportsWhatItApplied(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
			  "findings": [],
			  "summary": "refresh stale docs",
			  "applied_notes": [
			    {"id":"doc1","applied":true,"note":"updated the flag table"},
			    {"id":"doc2","applied":false,"note":"the comment already matches the guard"}
			  ]
			}`)}, nil
		},
	}
	// A configured lint command keeps this a documentation-only pass, so the
	// doc report is the only thing under test here.
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	recordReviewReports(t,
		sctx,
		[]types.HandoffNote{
			{ID: "doc1", File: "README.md", Line: 42, Problem: "README still says the flag defaults to false", RightLooksLike: "state the new default"},
			{ID: "doc2", File: "internal/api/router.go", Line: 88, Problem: "comment overstates what the guard does"},
			{ID: "doc3", File: "SETUP.md", Problem: "setup list is missing the new step"},
		},
		nil,
	)

	outcome, err := (&steps.DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	prompt := ag.Calls[len(ag.Calls)-1].Prompt
	for _, want := range []string{"Doc report from the review step", "README still says the flag defaults to false", "doc3", "applied_notes"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("document prompt is missing %q", want)
		}
	}
	// The widened scope: comment wording is this step's material now.
	if !strings.Contains(prompt, "test-name, and descriptive-string WORDING") {
		t.Error("document prompt does not claim comment wording; the reviewer stopped reporting it")
	}

	parsed := mustParse(t, outcome.Findings)
	if len(parsed.AppliedNotes) != 3 {
		t.Fatalf("applied notes = %+v, want one entry per handed-over note", parsed.AppliedNotes)
	}
	byID := map[string]types.HandoffOutcome{}
	for _, o := range parsed.AppliedNotes {
		byID[o.ID] = o
	}
	if !byID["doc1"].Applied {
		t.Errorf("doc1 = %+v, want applied", byID["doc1"])
	}
	if byID["doc2"].Applied || byID["doc2"].Note == "" {
		t.Errorf("doc2 = %+v, want not applied with a reason", byID["doc2"])
	}
	// A note the step never mentioned must read as unaddressed, not as done.
	if byID["doc3"].Applied || byID["doc3"].Note == "" {
		t.Errorf("doc3 = %+v, want an explicit unaddressed record", byID["doc3"])
	}
}

// TestLintStep_RunsTheFixerForTheReportEvenWhenTheLintCommandPasses is the
// reason the lint report is worth having: a green `commands.lint` says nothing
// about the checks it does not run, and the alternative was parking the run.
func TestLintStep_RunsTheFixerForTheReportEvenWhenTheLintCommandPasses(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
			  "findings": [],
			  "summary": "fix type check",
			  "applied_notes": [{"id":"lint1","applied":true,"note":"returned the interface type"}]
			}`)}, nil
		},
	}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	stepstest.RecordReviewApproval(t, sctx, headSHA)
	recordReviewReports(t, sctx, nil, []types.HandoffNote{
		{ID: "lint1", File: "internal/a/a.go", Line: 7, Problem: "declared type fails the checker", RightLooksLike: "return the interface type"},
	})

	outcome, err := (&steps.LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("agent invocations = %d, want exactly one pass over the lint report", len(ag.Calls))
	}
	prompt := ag.Calls[0].Prompt
	for _, want := range []string{"Lint report from the review step", "declared type fails the checker"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("lint report prompt is missing %q", want)
		}
	}
	if outcome.NeedsApproval {
		t.Error("a resolved lint report parked the run")
	}
	parsed := mustParse(t, outcome.Findings)
	if len(parsed.AppliedNotes) != 1 || !parsed.AppliedNotes[0].Applied {
		t.Errorf("applied notes = %+v, want lint1 applied", parsed.AppliedNotes)
	}
}

// A passing lint command with no report must stay exactly as cheap as before:
// no agent pass at all.
func TestLintStep_PassingCommandWithNoReportRunsNoAgent(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	stepstest.RecordReviewApproval(t, sctx, headSHA)
	recordReviewReports(t, sctx, []types.HandoffNote{{ID: "doc1", Problem: "stale line"}}, nil)

	if _, err := (&steps.LintStep{}).Execute(sctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("agent invocations = %d, want none: the doc report is not the lint step's work", len(ag.Calls))
	}
}

// TestLintStep_FailingCommandCarriesTheReportIntoTheFixRound covers the other
// configured-command path: one fix round addresses both.
func TestLintStep_FailingCommandCarriesTheReportIntoTheFixRound(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	var fixPrompt string
	ag := &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixPrompt = opts.Prompt
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix lint"}`)}, nil
		},
	}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "false"})
	stepstest.RecordReviewApproval(t, sctx, headSHA)
	recordReviewReports(t, sctx, nil, []types.HandoffNote{
		{ID: "lint1", File: "internal/a/a.go", Line: 7, Problem: "declared type fails the checker"},
	})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"f-1","severity":"warning","description":"linter found issues (exit code 1)","action":"auto-fix"}],"summary":""}`

	if _, err := (&steps.LintStep{}).Execute(sctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(fixPrompt, "declared type fails the checker") {
		t.Errorf("lint fix prompt did not carry the report:\n%s", fixPrompt)
	}
}

// The combined document+lint housekeeping pass is one agent invocation, so it
// must receive BOTH reports and the lint half's outcomes must ride the stash
// to the lint step's own row.
func TestHousekeepingPass_ReceivesBothReportsAndRoutesTheLintOutcomes(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
			  "findings": [],
			  "summary": "housekeeping",
			  "applied_notes": [
			    {"id":"doc1","applied":true,"note":"updated the README"},
			    {"id":"lint1","applied":true,"note":"removed the unused import"}
			  ]
			}`)}, nil
		},
	}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	stepstest.RecordReviewApproval(t, sctx, headSHA)
	recordReviewReports(t,
		sctx,
		[]types.HandoffNote{{ID: "doc1", File: "README.md", Problem: "stale default"}},
		[]types.HandoffNote{{ID: "lint1", File: "internal/a/a.go", Problem: "unused import"}},
	)

	docOutcome, err := (&steps.DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("document execute: %v", err)
	}
	prompt := ag.Calls[len(ag.Calls)-1].Prompt
	for _, want := range []string{"Doc report from the review step", "Lint report from the review step", "unused import"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("housekeeping prompt is missing %q", want)
		}
	}
	docNotes := mustParse(t, docOutcome.Findings).AppliedNotes
	if len(docNotes) != 1 || docNotes[0].ID != "doc1" {
		t.Errorf("document row outcomes = %+v, want only the doc note", docNotes)
	}

	lintOutcome, err := (&steps.LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("lint execute: %v", err)
	}
	lintNotes := mustParse(t, lintOutcome.Findings).AppliedNotes
	if len(lintNotes) != 1 || lintNotes[0].ID != "lint1" || !lintNotes[0].Applied {
		t.Errorf("lint row outcomes = %+v, want lint1 applied via the housekeeping stash", lintNotes)
	}
}

// A run whose review left no reports must behave exactly as it did before they
// existed: no prompt section, no outcome rows.
func TestDocumentStep_NoReportLeavesThePromptUnchanged(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"nothing stale"}`)}, nil
		},
	}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	recordReviewReports(t, sctx, nil, nil)

	outcome, err := (&steps.DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(ag.Calls[len(ag.Calls)-1].Prompt, "Doc report from the review step") {
		t.Error("an empty report still rendered its prompt section")
	}
	if notes := mustParse(t, outcome.Findings).AppliedNotes; len(notes) != 0 {
		t.Errorf("applied notes = %+v, want none", notes)
	}
}
