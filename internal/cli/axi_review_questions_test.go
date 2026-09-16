package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func reviewQuestionGate(t *testing.T) stepView {
	t.Helper()
	return stepView{
		Name:   string(types.StepReview),
		Status: string(types.StepStatusAwaitingApproval),
		FindingsJSON: findingsJSON(t, []types.Finding{
			{
				ID: "review-1", Severity: "warning", File: "main.go",
				Action: types.ActionAskUser, Description: "calls os.Exit",
			},
			{
				ID:       "question-q1",
				Severity: types.FindingSeverityWarning,
				File:     "internal/api/router.go",
				Line:     88,
				Action:   types.ActionAskUser,
				Category: types.FindingCategoryReviewQuestion,
				Description: "Review question awaiting an answer: Should the legacy /v1 route keep answering?" +
					"\nOptions: Keep answering | Remove it" +
					"\nArea: routing" +
					"\nAnswer it with: no-mistakes axi answer --question q1 --answer \"<one of the options>\"",
			},
		}, "1 issue and 1 open question"),
	}
}

// TestReviewQuestionGate_ShowsQuestionsDistinctlyAndLeadsWithAnswering covers
// the `axi` half of waiting-on-answers: an agent reading the gate must be able
// to tell "this review wants an answer" from "this review wants a verdict",
// and must not reach for approve or fix, which would discard the paused pass
// instead of answering it.
func TestReviewQuestionGate_ShowsQuestionsDistinctlyAndLeadsWithAnswering(t *testing.T) {
	got := axiDoc(gateFields(reviewQuestionGate(t))...)

	for _, want := range []string{
		"waiting_on",
		"answers",
		"review_questions",
		"q1",
		"Should the legacy /v1 route keep answering?",
		"Keep answering | Remove it",
		"no-mistakes axi answer --question",
		"Do not approve or fix to get past a review question",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("review gate output is missing %q:\n%s", want, got)
		}
	}

	// The answering guidance must come before the approve/fix guidance, or an
	// agent that acts on the first help line takes the wrong action.
	answerAt := strings.Index(got, "no-mistakes axi answer --question")
	approveAt := strings.Index(got, "axi respond --action approve")
	if answerAt < 0 || approveAt < 0 || answerAt > approveAt {
		t.Fatalf("answering guidance must lead the review-question gate help:\n%s", got)
	}
}

// A review gate with no open question keeps exactly today's shape: no
// waiting_on marker, no answering guidance, approve/fix leading as before.
func TestReviewGateWithoutQuestionsIsUnchanged(t *testing.T) {
	gate := reviewQuestionGate(t)
	gate.FindingsJSON = findingsJSON(t, []types.Finding{
		{ID: "review-1", Severity: "warning", File: "main.go", Action: types.ActionAskUser, Description: "calls os.Exit"},
	}, "1 blocking issue")

	got := axiDoc(gateFields(gate)...)
	for _, unwanted := range []string{"waiting_on", "review_questions", "axi answer --question"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("ordinary review gate leaked %q:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "axi respond --action approve") {
		t.Fatalf("ordinary review gate lost its approve guidance:\n%s", got)
	}
}

func TestSplitReviewQuestionDescription(t *testing.T) {
	question, options := splitReviewQuestionDescription(
		"Review question awaiting an answer: Keep /v1?\nOptions: keep | drop\nArea: routing\nAnswer it with: no-mistakes axi answer --question q1 --answer \"keep\"",
	)
	if question != "Keep /v1?" {
		t.Fatalf("question = %q", question)
	}
	if options != "keep | drop" {
		t.Fatalf("options = %q", options)
	}

	// A description that does not carry the markers degrades to a less
	// structured row, never an empty one.
	question, options = splitReviewQuestionDescription("just some text")
	if question != "just some text" || options != "" {
		t.Fatalf("degraded parse = %q / %q", question, options)
	}
}

// A finding whose id does not carry a question id is not rendered as a
// question: the id is how `axi answer --question` addresses it, so a row
// without one would be unanswerable.
func TestReviewQuestionRowsIgnoreFindingsWithNoQuestionID(t *testing.T) {
	gate := stepView{
		Name:   string(types.StepReview),
		Status: string(types.StepStatusAwaitingApproval),
		FindingsJSON: findingsJSON(t, []types.Finding{
			{
				ID: "review-7", Severity: types.FindingSeverityWarning, Action: types.ActionAskUser,
				Category: types.FindingCategoryReviewQuestion, Description: "Review question awaiting an answer: odd",
			},
		}, "1 issue"),
	}
	if rows := reviewQuestionRows(gate.FindingsJSON); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
	if rows := reviewQuestionRows("not json"); len(rows) != 0 {
		t.Fatalf("unparseable findings produced rows: %+v", rows)
	}
}

// TestAxiAnswerHasNoRunFlag pins the removal. `axi`'s run resolution is
// branch-scoped by design, and answer MUTATES: a --run would be a second
// selection path that could land an answer meant for one branch's reviewer on
// another's. Reintroducing the flag is a deliberate scope decision, so it
// should have to change this test to happen.
func TestAxiAnswerHasNoRunFlag(t *testing.T) {
	cmd := newAxiAnswerCmd()
	if f := cmd.Flags().Lookup("run"); f != nil {
		t.Fatalf("axi answer must not take --run; its run is the current branch's active run")
	}
	for _, want := range []string{"question", "answer", "by"} {
		if cmd.Flags().Lookup(want) == nil {
			t.Fatalf("axi answer is missing --%s", want)
		}
	}
}
