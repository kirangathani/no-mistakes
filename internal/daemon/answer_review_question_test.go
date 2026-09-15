package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// answerFixture registers a real Executor for one run id, which is what the
// answer handler needs: the executor is the single owner of where the run's
// review conversation lives, because that path depends on effective config.
func answerFixture(t *testing.T) (*RunManager, *paths.Paths, string) {
	t.Helper()
	return answerFixtureWithConversation(t, true)
}

func answerFixtureWithConversation(t *testing.T, conversation bool) (*RunManager, *paths.Paths, string) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	m := NewRunManager(database, p, nil)
	const runID = "run-answer-1"
	cfg := &config.Config{Review: config.Review{Conversation: conversation}}
	exec := pipeline.NewExecutor(database, p, cfg, nil, nil, nil)
	m.mu.Lock()
	m.executors[runID] = exec
	m.mu.Unlock()
	return m, p, runID
}

func conversationDir(p *paths.Paths, runID string) string {
	return reviewqa.Dir(p.RunEvidenceDir("", runID))
}

// TestAnswerReviewQuestionRecordsBeforeItDecidesToRelease is the property that
// makes the answer channel safe: the answer is on disk before anything is
// attempted with the gate, so a reviewer that is still mid-pass reads it at its
// next checkpoint and an unreleasable gate never costs the operator the answer.
func TestAnswerReviewQuestionRecordsBeforeItDecidesToRelease(t *testing.T) {
	m, p, runID := answerFixture(t)
	dir := conversationDir(p, runID)
	for _, id := range []string{"q1", "q2"} {
		if err := reviewqa.AppendQuestion(dir, reviewqa.Question{
			ID: id, Question: "question " + id, Options: []string{"a", "b"},
		}); err != nil {
			t.Fatalf("append question: %v", err)
		}
	}

	// First answer: one question still open, so the gate is deliberately not
	// touched and the caller is told how many remain.
	result, err := m.HandleAnswerReviewQuestion(runID, "q1", "a", "captain")
	if err != nil {
		t.Fatalf("answer q1: %v", err)
	}
	if !result.OK || result.Open != 1 || result.Resumed {
		t.Fatalf("q1 result = %+v, want ok with 1 open and not resumed", result)
	}
	if len(result.OpenIDs) != 1 || result.OpenIDs[0] != "q2" {
		t.Fatalf("open ids = %v, want [q2]", result.OpenIDs)
	}

	// Second answer: nothing is open, so a release is attempted. This fixture
	// has no parked gate, so the release fails - and that must still be a
	// recorded answer and a successful call, because it is the ordinary
	// mid-turn case (the reviewer is still working, there is no gate yet).
	result, err = m.HandleAnswerReviewQuestion(runID, "q2", "b", "firstmate")
	if err != nil {
		t.Fatalf("answer q2: %v", err)
	}
	if !result.OK || result.Open != 0 {
		t.Fatalf("q2 result = %+v, want ok with nothing open", result)
	}
	if result.Resumed {
		t.Fatalf("q2 result = %+v, want not resumed: there is no parked gate here", result)
	}
	if !strings.Contains(result.Note, "not released") {
		t.Fatalf("note = %q, want it to say the gate was not released", result.Note)
	}

	// Both answers survived, attributed, whatever the gate did.
	conv, err := reviewqa.Load(dir)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if len(conv.Open()) != 0 || len(conv.Answered()) != 2 {
		t.Fatalf("conversation = %+v, want 2 answered and 0 open", conv)
	}
	byID := map[string]reviewqa.Answer{}
	for _, e := range conv.Answered() {
		byID[e.ID] = *e.Answer
	}
	if byID["q1"].Answer != "a" || byID["q1"].AnsweredBy != "captain" {
		t.Fatalf("q1 = %+v", byID["q1"])
	}
	if byID["q2"].Answer != "b" || byID["q2"].AnsweredBy != "firstmate" {
		t.Fatalf("q2 = %+v", byID["q2"])
	}
}

// A correction is another append, and the last one wins - matching the file
// protocol, so the operator never has to undo an answer.
func TestAnswerReviewQuestionCorrectionReplacesTheEarlierAnswer(t *testing.T) {
	m, p, runID := answerFixture(t)
	dir := conversationDir(p, runID)
	if err := reviewqa.AppendQuestion(dir, reviewqa.Question{
		ID: "q1", Question: "keep it?", Options: []string{"keep", "drop"},
	}); err != nil {
		t.Fatalf("append question: %v", err)
	}
	for _, answer := range []string{"keep", "drop, on reflection"} {
		if _, err := m.HandleAnswerReviewQuestion(runID, "q1", answer, "captain"); err != nil {
			t.Fatalf("answer: %v", err)
		}
	}
	conv, err := reviewqa.Load(dir)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if len(conv.Answered()) != 1 || conv.Answered()[0].Answer.Answer != "drop, on reflection" {
		t.Fatalf("conversation = %+v, want the last answer to win", conv)
	}
}

func TestAnswerReviewQuestionRefusesIncompleteOrUnknownInput(t *testing.T) {
	m, _, runID := answerFixture(t)

	if _, err := m.HandleAnswerReviewQuestion(runID, "", "a", ""); err == nil {
		t.Fatal("want an error with no question id")
	}
	if _, err := m.HandleAnswerReviewQuestion(runID, "q1", "   ", ""); err == nil {
		t.Fatal("want an error with a blank answer")
	}
	// No active executor means no run that could be resumed, and no owner of
	// the conversation path either.
	if _, err := m.HandleAnswerReviewQuestion("no-such-run", "q1", "a", ""); err == nil {
		t.Fatal("want an error for a run with no active executor")
	}
}

// An answer for a question nobody asked is recorded and ignored rather than
// failing: the writer may be racing a question it has not read yet. It must not
// be counted as open, or it would park a run on a question the reviewer never
// asked.
func TestAnswerReviewQuestionForAnUnknownQuestionDoesNotOpenOne(t *testing.T) {
	m, p, runID := answerFixture(t)
	result, err := m.HandleAnswerReviewQuestion(runID, "ghost", "a", "captain")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if result.Open != 0 {
		t.Fatalf("result = %+v, want nothing open", result)
	}
	conv, err := reviewqa.Load(conversationDir(p, runID))
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if len(conv.Entries) != 0 {
		t.Fatalf("entries = %+v, want none", conv.Entries)
	}
	if !strings.Contains(strings.Join(conv.Notes, "\n"), "unknown question") {
		t.Fatalf("orphan answer not disclosed: %v", conv.Notes)
	}
}

// The handler is review-scoped by construction: it is the only sender of
// types.ActionAnswer, and the executor refuses that action for any other step.
func TestAnswerActionIsReviewScoped(t *testing.T) {
	if types.ActionAnswer == types.ActionApprove || types.ActionAnswer == types.ActionFix {
		t.Fatal("the answer action must be distinct from a gate verdict")
	}
}

// TestAnswerReviewQuestionRefusesWhenTheConversationIsOff is the opt-in half of
// the answer channel. A repository that has not set review.conversation has no
// reviewer that was ever told to ask, so an answer has nothing to settle and
// nothing to release - and the refusal has to name the setting that would
// accept one, or an operator reading "no review conversation directory" would
// go looking for a missing directory instead of an unset key.
func TestAnswerReviewQuestionRefusesWhenTheConversationIsOff(t *testing.T) {
	m, p, runID := answerFixtureWithConversation(t, false)

	// Seeded exactly as the enabled path would seed it, so the refusal is the
	// setting's doing rather than an empty channel's.
	if err := reviewqa.AppendQuestion(conversationDir(p, runID), reviewqa.Question{
		ID: "q1", Question: "keep the legacy route?", Options: []string{"keep", "remove"},
	}); err != nil {
		t.Fatalf("seed question: %v", err)
	}

	result, err := m.HandleAnswerReviewQuestion(runID, "q1", "keep", "captain")
	if err == nil {
		t.Fatalf("answering with the conversation off must fail, got %+v", result)
	}
	if !strings.Contains(err.Error(), "review.conversation") {
		t.Fatalf("refusal does not name the setting that would accept an answer: %v", err)
	}

	// Nothing recorded: a refused answer must not leave a half-written channel
	// a later enabled run would read as settled.
	if answers, readErr := os.ReadFile(filepath.Join(conversationDir(p, runID), reviewqa.AnswersFile)); readErr == nil {
		t.Fatalf("a refused answer was written to disk: %s", answers)
	}
}
