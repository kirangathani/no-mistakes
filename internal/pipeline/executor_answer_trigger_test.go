package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_RecoveredAnswerRoundIsTriggeredAsAnAnswer drives the DAEMON
// RESTART path specifically, which is the only path that got this wrong.
//
// A review parks on the questions its reviewer asked; a park lasts tens of
// minutes to hours, so a restart inside that window is exactly what Resume
// exists for. Resume's ActionAnswer branch sets `answering` and deliberately
// leaves `fixing` false, because no code changed - so a trigger derived from
// Fixing alone labelled the finalize round "initial" and left step_rounds
// claiming the run had two initial review rounds. That is a durable wrong
// label, rendered back into round-history prompt sections and the PR pipeline
// summary, and nothing errors.
//
// The live loop already labels this "answer" when it re-enters the step
// itself, so a test driving the live loop passes on the broken code and proves
// nothing. This one goes through Resume.
func TestExecutor_RecoveredAnswerRoundIsTriggeredAsAnAnswer(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}

	// The gate the reviewer left behind: one open question, parked as the
	// ask-user warning the review step emits for it.
	findings := `{"findings":[{"id":"question-q1","severity":"warning","description":"Review question awaiting an answer: keep the legacy route?","action":"ask-user","category":"review-question"}],"summary":"one open question"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "", 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	finalized := make(chan struct{})
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.Fixing {
				return nil, fmt.Errorf("an answer is not a fix round; Fixing must stay false")
			}
			if !sctx.FinalizingAnswers {
				return nil, fmt.Errorf("recovered answer round did not reach the step as a finalize turn")
			}
			close(finalized)
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 2}}, newFakeSessionAgent(), []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered gate never accepted an answer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-finalized:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered finalize turn never ran")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovered executor timed out")
	}

	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("got %d review rounds, want the parked one plus the finalize round", len(rounds))
	}
	if rounds[1].Trigger != "answer" {
		t.Fatalf("recovered finalize round trigger = %q, want \"answer\"; %q would claim the run had two initial review rounds", rounds[1].Trigger, rounds[1].Trigger)
	}
	// An answer is not a fix round, and nothing else may reclassify it as one.
	if rounds[1].IsFixRound() {
		t.Fatal("the recovered answer round reads as a fix round")
	}
}

// TestExecutor_AnswerActionIsRefusedForAnyStepButReview drives the guard that
// keeps the answer action review-scoped.
//
// Only the review step owns a question channel, so any other step receiving
// this action would re-execute with review semantics it does not implement -
// SkipFixExecution, FinalizingAnswers and an "answer" round on a step that has
// no reviewer to resume. It therefore fails closed.
//
// This replaces a test that asserted only that types.ActionAnswer differs from
// the two verdict constants, which could not fail for any change to the
// behavior its name claimed: deleting the guard entirely left every test in the
// tree green.
func TestExecutor_AnswerActionIsRefusedForAnyStepButReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepDocument, `{"findings":[{"id":"d-1","severity":"warning","description":"waiting","action":"ask-user"}],"summary":"1 issue"}`)
	exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepDocument, types.StepStatusAwaitingApproval)

	if err := exec.Respond(types.StepDocument, types.ActionAnswer, nil); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "only a review response") {
			t.Fatalf("Execute() error = %v, want the answer action refused for a non-review step", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run neither failed nor completed after an out-of-scope answer")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("run status = %s, want %s: an out-of-scope answer must fail closed", got.Status, types.RunFailed)
	}
	// It failed instead of re-executing the step with review semantics.
	if n := step.callCount(); n != 1 {
		t.Fatalf("step executed %d times, want 1: the refused answer must not re-run it", n)
	}
}
