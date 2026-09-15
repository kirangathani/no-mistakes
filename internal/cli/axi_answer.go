package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	toon "github.com/toon-format/toon-go"
)

// newAxiAnswerCmd answers one question the run's reviewer asked while it
// worked.
//
// It is deliberately a separate verb from `respond`. `respond` is a verdict on
// a round - approve, fix, skip - and its actions are the operator's. An answer
// is neither: it settles one question the reviewer itself raised, no code
// changes, and the round's findings are not being accepted or declined. Folding
// it into `respond --action answer` would let an operator release a review gate
// while questions were still open, which is exactly what this command refuses
// to do: the daemon releases the gate only when nothing is left open, and does
// it by resuming the reviewer's own session.
func newAxiAnswerCmd() *cobra.Command {
	var questionID, answer, answeredBy, runID string
	cmd := &cobra.Command{
		Use:   "answer",
		Short: "Answer a question the reviewer asked while reviewing",
		Long: "Records an answer to one question the run's reviewer asked. Questions\n" +
			"appear in `no-mistakes axi status` under the review gate's\n" +
			"review_questions, each with its id and its options.\n\n" +
			"This is not a gate response. The answer is appended to the run's review\n" +
			"conversation immediately, so a reviewer that is still working reads it at\n" +
			"its next checkpoint and can redirect the rest of its pass. Once no\n" +
			"question is left open, the daemon resumes that same reviewer session with\n" +
			"the answers so it can finish - you do not approve or fix to release it.\n\n" +
			"Answer with one of the question's stated options wherever you can; the\n" +
			"reviewer wrote them so the answer would be unambiguous.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-answer", "/axi/answer", nil, func() error {
				return runAxiAnswer(cmd, answerArgs{
					questionID: strings.TrimSpace(questionID),
					answer:     strings.TrimSpace(answer),
					answeredBy: strings.TrimSpace(answeredBy),
					runID:      strings.TrimSpace(runID),
				})
			})
		},
	}
	cmd.Flags().StringVar(&questionID, "question", "", "question id as shown in the review gate's review_questions (required)")
	cmd.Flags().StringVar(&answer, "answer", "", "the answer, ideally one of the question's stated options (required)")
	cmd.Flags().StringVar(&answeredBy, "by", "", "who answered, recorded on the PR and in the branch's settled questions")
	cmd.Flags().StringVar(&runID, "run", "", "answer against this run id instead of resolving the current branch's active run")
	return cmd
}

type answerArgs struct {
	questionID string
	answer     string
	answeredBy string
	runID      string
}

func runAxiAnswer(cmd *cobra.Command, aa answerArgs) error {
	if aa.questionID == "" || aa.answer == "" {
		return emitError(cmd, 2, "--question and --answer are both required",
			`Run `+"`no-mistakes axi status`"+` to list the reviewer's open questions and their ids`)
	}

	ctx := cmd.Context()
	env, err := openAxiDaemonEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	runID := aa.runID
	if runID == "" {
		branch, err := git.CurrentBranch(ctx, ".")
		if err != nil {
			return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
		}
		var active ipc.GetActiveRunResult
		if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
			return emitError(cmd, 1, fmt.Sprintf("get active run: %v", err))
		}
		if active.Run == nil {
			return emitError(cmd, 1, "no active run to answer",
				"A review question can only be answered while its run is still active; run `no-mistakes axi status` to check")
		}
		runID = active.Run.ID
	}

	var result ipc.AnswerReviewQuestionResult
	if err := env.client.Call(ipc.MethodAnswerReview, &ipc.AnswerReviewQuestionParams{
		RunID:      runID,
		QuestionID: aa.questionID,
		Answer:     aa.answer,
		AnsweredBy: aa.answeredBy,
	}, &result); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("answer review question: %v", err))
	}

	fields := []toon.Field{
		{Key: "answered", Value: result.OK},
		{Key: "run", Value: runID},
		{Key: "question", Value: aa.questionID},
		{Key: "open_questions", Value: result.Open},
	}
	if len(result.OpenIDs) > 0 {
		fields = append(fields, toon.Field{Key: "open_question_ids", Value: result.OpenIDs})
	}
	fields = append(fields, toon.Field{Key: "reviewer_resumed", Value: result.Resumed})
	if result.Note != "" {
		fields = append(fields, toon.Field{Key: "detail", Value: result.Note})
	}
	var help []string
	switch {
	case result.Open > 0:
		help = append(help, "Answer the remaining questions with `no-mistakes axi answer --question <id> --answer \"...\"`; the review stays parked until none are open")
	case result.Resumed:
		help = append(help, "The reviewer is finishing its pass with your answers; run `no-mistakes axi status` for its findings and the next gate")
	default:
		help = append(help, "The answer is recorded and the reviewer will read it at its next checkpoint; run `no-mistakes axi status` to follow the run")
	}
	fields = append(fields, toon.Field{Key: "help", Value: help})
	emitDoc(cmd, fields...)
	return nil
}
