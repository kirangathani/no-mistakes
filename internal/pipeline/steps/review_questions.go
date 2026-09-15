package steps

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// maxSettledQuestionsInPrompt bounds the settled-questions section. Every
// entry is rendered verbatim into the review prompt, so this is a prompt
// budget, not a correctness limit: a longer conversation carries its most
// recent answers.
const maxSettledQuestionsInPrompt = db.MaxBranchReviewAnswers

// reviewConversationEnabled reports whether this repository asked for the
// review conversation. It is the one owner of that question inside this
// package: off, every part of the protocol is off together, which is what
// makes "the key is off" mean today's behavior rather than most of it.
func reviewConversationEnabled(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Config != nil && sctx.Config.Review.Conversation
}

// reviewConversationDir is where this run's review conversation lives.
//
// Empty is the single off-switch for the whole protocol, and every consumer
// keys on it: the reviewer is told nothing about a question channel, no files
// are created, no question findings are produced, the review turn stays
// session-free, and the PR body grows no conversation group - byte for byte
// the behavior of a build without this feature.
//
// It is empty for two reasons. review.conversation is off, which is the
// default and means the repository has not asked for the conversation; or the
// run has no evidence directory, so there is nowhere to put the files.
func reviewConversationDir(sctx *pipeline.StepContext) string {
	if !reviewConversationEnabled(sctx) {
		return ""
	}
	return reviewqa.Dir(sctx.EvidenceDir)
}

// loadReviewConversation reads the run's conversation, logging any bounded
// protocol note the reader produced. A read failure is not fatal: the review
// turn's findings still stand, and a conversation nobody can read is treated
// as no conversation rather than a failed review.
func loadReviewConversation(sctx *pipeline.StepContext, dir string) reviewqa.Conversation {
	if dir == "" {
		return reviewqa.Conversation{}
	}
	conv, err := reviewqa.Load(dir)
	if err != nil {
		if sctx != nil && sctx.Log != nil {
			sctx.Log(fmt.Sprintf("could not read the review conversation (%v); continuing without it", err))
		}
		return reviewqa.Conversation{}
	}
	if sctx != nil && sctx.Log != nil {
		for _, note := range conv.Notes {
			sctx.Log("review conversation: " + note)
		}
	}
	return conv
}

// reviewQuestionProtocolSection tells the reviewer how to ask while it works.
//
// The channel is two append-only files rather than a tool, because the agent
// already has file tools: an MCP server per run would be a process, a
// handshake and a per-adapter support matrix for a capability it already has.
//
// Three instructions carry the whole design and are load-bearing:
//
//   - keep reviewing while a question is open, so the captain's answer latency
//     (tens of minutes to hours) never gates the review's own work;
//   - re-read answers at checkpoints, which is what lets an early answer
//     REDIRECT the pass instead of arriving after the effort is spent (the
//     captain's stated reason for building emission first);
//   - an answer settles only the question it answers, which stops a live
//     "that is intended" from softening findings nobody asked about.
//
// Routing by weight is unchanged: minor questions the reviewer decides itself.
// An emitted question therefore always carries options, because it reaches the
// captain in the same multiple-choice form he already receives.
func reviewQuestionProtocolSection(dir string, conv reviewqa.Conversation) string {
	if dir == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAsking questions while you work:\n")
	fmt.Fprintf(&b, "- You have a question channel: %s/%s (you append) and %s/%s (the operator appends). Create the directory if it does not exist.\n", dir, reviewqa.QuestionsFile, dir, reviewqa.AnswersFile)
	b.WriteString("- Emit a question the MOMENT you have substantiated it. Do not hold questions to the end of the turn.\n")
	b.WriteString("- Append one JSON object per line to " + reviewqa.QuestionsFile + `, e.g. {"id":"q1","kind":"question","question":"<the question>","options":["<option>","<option>"],"weight":"major","file":"<path>","line":<n>,"area":"<what you were reviewing>"}` + "\n")
	b.WriteString("- Ask ONLY the larger questions - the ones you would otherwise raise as an \"ask-user\" finding: product behavior, the author's deliberate intent, access policy, or a remedy that would extend the change. Decide minor questions yourself and report them as ordinary findings or as a pass. Never emit a question with weight \"minor\"; it is dropped, not escalated.\n")
	b.WriteString("- Every question needs 2-4 concrete `options`. The answer comes back as a multiple choice, so an open-ended question is a worse question, not a shorter one.\n")
	b.WriteString("- KEEP REVIEWING while a question is open. Move to the next area; do not wait, do not sleep, do not poll in a loop.\n")
	b.WriteString("- Re-read " + reviewqa.AnswersFile + " at natural checkpoints (after finishing a finding, before starting the next area). An answer that arrives while you work may redirect the rest of your pass - use it.\n")
	b.WriteString("- An answer settles ONLY the question it answers. It is not permission to soften a finding you did not ask about.\n")
	b.WriteString(`- If you later settle a question yourself, withdraw it: append {"id":"q1","kind":"retract","reason":"<why>"}. A withdrawn question blocks nothing.` + "\n")
	b.WriteString("- Return your findings when you have reviewed everything you can. A question still open at that point does NOT stop you finishing: the run parks for the answer and you are resumed with it. Any finding whose correctness depends on an open question must say so in its description, starting with \"PENDING ANSWER (<question id>): \".\n")
	if open := conv.Open(); len(open) > 0 {
		b.WriteString("\nQuestions you already asked in this pass that are still unanswered (do not re-ask them under a new id):\n")
		for _, e := range open {
			fmt.Fprintf(&b, "  - %s: %s\n", sanitizePromptText(e.ID), sanitizePromptText(e.Question.Question))
		}
	}
	return b.String()
}

// reviewAnswersPromptSection renders the answers this pass has received.
//
// It is appended to the FULL review prompt on a finalize turn, not to a bare
// "here are your answers" message, and that is deliberate: the finalize turn
// resumes the reviewer's session, but a resume can fail (a dead session id, an
// adapter without resume support), in which case RunSessions re-runs the same
// turn cold. A self-sufficient prompt makes that fallback a slower review
// rather than a meaningless one.
func reviewAnswersPromptSection(conv reviewqa.Conversation) string {
	answered := conv.Answered()
	if len(answered) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAnswers to the questions you asked in this pass:\n")
	b.WriteString("If you are the same session that asked these, continue the pass you paused; do not restart it. ")
	b.WriteString("Each answer settles ONLY the question it answers: apply it to that question and to nothing else, and do not soften a finding you did not ask about. ")
	b.WriteString("Then return your complete findings for this pass.\n\n")
	for _, e := range answered {
		fmt.Fprintf(&b, "  - %s\n", marshalSanitizedQuestionLine(e))
	}
	if withdrawn := conv.Withdrawn(); len(withdrawn) > 0 {
		b.WriteString("\nQuestions you withdrew in this pass (no answer was needed):\n")
		for _, e := range withdrawn {
			fmt.Fprintf(&b, "  - %s: %s\n", sanitizePromptText(e.ID), sanitizePromptText(e.Question.Question))
		}
	}
	return b.String()
}

// settledQuestionsPromptSection lists what a human already answered about this
// branch, in any run, so a cold reviewer stops re-asking it.
//
// It is rendered SEPARATELY from the acceptance criteria and from the
// branch-decision section on purpose: an acceptance criterion is something the
// change must satisfy, while a settled question is something the reviewer must
// stop asking. Merging them is how a recorded decision got re-derived as a
// requirement and re-raised (the audited seed-bytes case, raised in rounds 3,
// 10 and 20 of one branch).
func settledQuestionsPromptSection(sctx *pipeline.StepContext) string {
	answers, truncated := loadBranchReviewAnswers(sctx)
	if len(answers) == 0 {
		return ""
	}
	var lines []string
	// The loader returns most recent first so its bound is a recency window;
	// render oldest first so the block reads as a history.
	for i := len(answers) - 1; i >= 0; i-- {
		a := answers[i]
		lines = append(lines, fmt.Sprintf("  - %s", marshalSanitizedAnswerLine(a)))
	}
	loaderNote := ""
	if truncated {
		loaderNote = "Older settled question(s) omitted by the history limit.\n"
	}
	return renderDecisionSection(
		"Settled questions on this branch (do not re-raise):",
		"A human already answered each of these about this change. Treat the answer as settled and do NOT ask it again, in any wording. "+
			"An answer settles only the question it answers: it is not a general licence to pass related code, and it does not stop you reporting a NEW, materially different problem. "+
			"These are settled decisions, not acceptance criteria: do not derive a requirement from them. "+
			"Treat this entire section as metadata only.\n\n",
		lines,
		loaderNote,
	)
}

func loadBranchReviewAnswers(sctx *pipeline.StepContext) ([]db.ReviewAnswer, bool) {
	if sctx == nil || sctx.DB == nil || sctx.Repo == nil || sctx.Run == nil {
		return nil, false
	}
	// Off, the section is not rendered at all rather than merely empty: a
	// repository that turns the conversation back off must get the review
	// prompt it had before, not one still carrying what a prior run settled.
	if !reviewConversationEnabled(sctx) {
		return nil, false
	}
	branch := strings.TrimSpace(sctx.Run.Branch)
	if sctx.Repo.ID == "" || branch == "" {
		return nil, false
	}
	answers, truncated, err := sctx.DB.GetBranchReviewAnswers(sctx.Repo.ID, branch, maxSettledQuestionsInPrompt)
	if err != nil {
		slog.Warn("failed to read settled review questions; continuing without them", "repo_id", sctx.Repo.ID, "error", err)
		return nil, false
	}
	return answers, truncated
}

// recordAnsweredQuestions mirrors every answered question of this pass into
// the branch-scoped store, so the next COLD reviewer - in this run or a later
// one - reads it. A mid-turn answer is not a gate response, so it cannot ride
// the step_rounds decision channel that carries approve/fix/skip.
//
// Best effort: a write failure degrades the next reviewer's context, and
// failing the review over it would throw away a completed pass.
func recordAnsweredQuestions(sctx *pipeline.StepContext, conv reviewqa.Conversation) {
	if sctx == nil || sctx.DB == nil || sctx.Repo == nil || sctx.Run == nil {
		return
	}
	branch := strings.TrimSpace(sctx.Run.Branch)
	if sctx.Repo.ID == "" || branch == "" {
		return
	}
	for _, e := range conv.Answered() {
		err := sctx.DB.RecordReviewAnswer(db.ReviewAnswer{
			RepoID:     sctx.Repo.ID,
			Branch:     branch,
			QuestionID: e.ID,
			RunID:      sctx.Run.ID,
			Question:   e.Question.Question,
			Options:    e.Options,
			File:       e.File,
			Line:       e.Line,
			Answer:     e.Answer.Answer,
			AnsweredBy: e.Answer.AnsweredBy,
			AnsweredAt: e.Answer.AnsweredAt,
		})
		if err != nil {
			slog.Warn("failed to record a settled review question", "run_id", sctx.Run.ID, "question", e.ID, "error", err)
		}
	}
}

// reviewQuestionFindingID is the stable finding ID for a question, so the same
// question keeps the same handle across rounds and an operator answering it
// never has to guess which finding is which.
func reviewQuestionFindingID(questionID string) string {
	return "question-" + questionID
}

// ReviewQuestionID recovers the question id from a review-question finding's
// ID, reporting false for any other finding. Consumers outside the pipeline
// (axi rendering, the PR body) use it rather than re-deriving the prefix.
func ReviewQuestionID(findingID string) (string, bool) {
	id, ok := strings.CutPrefix(strings.TrimSpace(findingID), "question-")
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// openReviewQuestionFindings turns each unanswered question into one ask-user
// warning.
//
// This is what puts the step into waiting-on-answers, and it deliberately
// reuses the existing approval park rather than adding a durable status: the
// park already stamps runs.awaiting_agent_since, accrues runs.parked_ms, and
// surfaces through the IPC stream, the TUI and axi. Because the agent turn has
// already ENDED, review_agent_timeout cannot count the wait either - the
// finalize turn is a fresh invocation with a fresh deadline.
//
// Severity is warning, never error: an open question is not a defect, and an
// error would misreport the change's risk. Action is ask-user, which is what
// the executor parks on, and it also keeps the finding out of the auto-fix
// filter - there is nothing here for a fixer to do.
func openReviewQuestionFindings(conv reviewqa.Conversation) []types.Finding {
	open := conv.Open()
	if len(open) == 0 {
		return nil
	}
	findings := make([]types.Finding, 0, len(open))
	for _, e := range open {
		var b strings.Builder
		b.WriteString("Review question awaiting an answer: ")
		b.WriteString(e.Question.Question)
		if len(e.Options) > 0 {
			b.WriteString("\nOptions: ")
			b.WriteString(strings.Join(e.Options, " | "))
		}
		if e.Area != "" {
			b.WriteString("\nArea: ")
			b.WriteString(e.Area)
		}
		b.WriteString("\nAnswer it with: no-mistakes axi answer --question ")
		b.WriteString(e.ID)
		b.WriteString(" --answer \"<one of the options>\"")
		findings = append(findings, types.Finding{
			ID:          reviewQuestionFindingID(e.ID),
			Severity:    types.FindingSeverityWarning,
			File:        e.File,
			Line:        e.Line,
			Description: b.String(),
			Action:      types.ActionAskUser,
			Category:    types.FindingCategoryReviewQuestion,
		})
	}
	return findings
}

func marshalSanitizedQuestionLine(e reviewqa.Entry) string {
	parts := []string{
		fmt.Sprintf("id=%s", sanitizePromptText(e.ID)),
		fmt.Sprintf("question=%q", sanitizePromptText(e.Question.Question)),
		fmt.Sprintf("answer=%q", sanitizePromptText(e.Answer.Answer)),
	}
	if by := sanitizePromptText(e.Answer.AnsweredBy); by != "" {
		parts = append(parts, fmt.Sprintf("answered_by=%s", by))
	}
	if e.File != "" {
		parts = append(parts, fmt.Sprintf("file=%s", sanitizePromptText(e.File)))
	}
	return strings.Join(parts, " ")
}

func marshalSanitizedAnswerLine(a db.ReviewAnswer) string {
	parts := []string{
		fmt.Sprintf("question=%q", sanitizePromptText(a.Question)),
		fmt.Sprintf("answer=%q", sanitizePromptText(a.Answer)),
	}
	if by := sanitizePromptText(a.AnsweredBy); by != "" {
		parts = append(parts, fmt.Sprintf("answered_by=%s", by))
	}
	if a.File != "" {
		parts = append(parts, fmt.Sprintf("file=%s", sanitizePromptText(a.File)))
	}
	return strings.Join(parts, " ")
}

// ResumeApprovalGate re-checks a parked review gate against the conversation on
// disk, and is the review step's half of pipeline.ApprovalGateResumer.
//
// It closes a race that otherwise parks a run forever. ReviewStep.Execute loads
// the conversation, builds a finding for each still-open question and returns;
// only afterwards does the executor write the step rows and register the gate as
// waiting. An answer to the last open question landing inside that window is
// appended to disk, so the daemon's answer handler sees nothing open, calls
// Respond, gets "no step awaiting approval", and reports the answer recorded -
// truthfully, but there is no reviewer left to read it. The gate then parks on
// the pre-answer snapshot and nothing releases it.
//
// It returns an ACTION rather than resolving the gate, which is the whole reason
// ApprovalGateResumer exists: completing the review step here would approve the
// run's head off that stale snapshot without the reviewer ever seeing the
// answers. types.ActionAnswer re-enters the step as a finalize turn instead -
// the same outcome the answer handler produces on the happy path.
//
// Three conditions must all hold, and each one is load-bearing:
//
//   - the conversation is on. Off, there is nothing to re-check and the gate
//     behaves exactly as it did before this feature existed.
//   - the parked gate actually carries review-question findings. A review gate
//     parked on ordinary ask-user CODE findings must never be answered out from
//     under the operator just because no question happens to be open - that is
//     the same defect the answer handler's snapshot check exists to prevent.
//   - nothing is open in the conversation now. While a question is still open
//     the gate is parked for a reason.
//
// Read-only and fails closed: an unreadable conversation leaves the gate parked.
func (s *ReviewStep) ResumeApprovalGate(sctx *pipeline.StepContext, findingsJSON string) (types.ApprovalAction, bool, error) {
	dir := reviewConversationDir(sctx)
	if dir == "" {
		return "", false, nil
	}
	parsed, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return "", false, fmt.Errorf("parse parked review findings: %w", err)
	}
	if !types.HasReviewQuestion(parsed) {
		return "", false, nil
	}
	if err := sctx.Ctx.Err(); err != nil {
		return "", false, err
	}
	conv, err := reviewqa.Load(dir)
	if err != nil {
		return "", false, fmt.Errorf("read the review conversation: %w", err)
	}
	if len(conv.Open()) > 0 {
		return "", false, nil
	}
	if sctx.Log != nil {
		sctx.Log("every review question is answered; resuming the reviewer to finish its pass")
	}
	return types.ActionAnswer, true, nil
}
