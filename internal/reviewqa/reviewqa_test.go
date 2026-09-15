package reviewqa

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingDirectoryIsAnEmptyConversation(t *testing.T) {
	conv, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Entries) != 0 || len(conv.Open()) != 0 {
		t.Fatalf("want empty conversation, got %+v", conv)
	}
	if conv, err := Load(""); err != nil || len(conv.Entries) != 0 {
		t.Fatalf("empty dir: %+v %v", conv, err)
	}
}

func TestOpenAnsweredAndWithdrawnPartitionTheConversation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, QuestionsFile,
		`{"id":"q1","kind":"question","question":"keep /v1?","options":["keep","drop"],"weight":"major"}`,
		`{"id":"q2","kind":"question","question":"flag default?","options":["on","off"]}`,
		`{"id":"q3","kind":"question","question":"rename table?","options":["yes","no"]}`,
		`{"id":"q3","kind":"retract","reason":"answered by the migration note"}`,
	)
	write(t, dir, AnswersFile,
		`{"id":"q2","answer":"off","answered_by":"captain"}`,
	)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(conv.Entries); got != 3 {
		t.Fatalf("entries = %d, want 3", got)
	}
	open := conv.Open()
	if len(open) != 1 || open[0].ID != "q1" {
		t.Fatalf("open = %+v, want only q1", open)
	}
	answered := conv.Answered()
	if len(answered) != 1 || answered[0].ID != "q2" || answered[0].Answer.Answer != "off" {
		t.Fatalf("answered = %+v, want q2=off", answered)
	}
	withdrawn := conv.Withdrawn()
	if len(withdrawn) != 1 || withdrawn[0].ID != "q3" {
		t.Fatalf("withdrawn = %+v, want only q3", withdrawn)
	}
	// Order is first-asked, so a consumer renders the conversation as it happened.
	if conv.Entries[0].ID != "q1" || conv.Entries[2].ID != "q3" {
		t.Fatalf("order = %v", []string{conv.Entries[0].ID, conv.Entries[1].ID, conv.Entries[2].ID})
	}
}

func TestLaterLinesSupersedeEarlierOnesForTheSameID(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, QuestionsFile,
		`{"id":"q1","question":"first wording","options":["a"]}`,
		`{"id":"q1","question":"sharper wording","options":["a","b"]}`,
		`{"id":"q2","question":"withdrawn then re-asked","options":["a"]}`,
		`{"id":"q2","kind":"retract","reason":"thought it was settled"}`,
		`{"id":"q2","question":"withdrawn then re-asked","options":["a"]}`,
	)
	write(t, dir, AnswersFile,
		`{"id":"q1","answer":"a"}`,
		`{"id":"q1","answer":"b, on reflection"}`,
	)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (an edit is not a duplicate)", len(conv.Entries))
	}
	if conv.Entries[0].Question.Question != "sharper wording" {
		t.Fatalf("question = %q, want the last wording", conv.Entries[0].Question.Question)
	}
	if conv.Entries[0].Answer.Answer != "b, on reflection" {
		t.Fatalf("answer = %q, want the last answer", conv.Entries[0].Answer.Answer)
	}
	// Re-asking revives a retracted question: it is how the reviewer says the
	// retraction was wrong, and it must block again.
	if !conv.Entries[1].Open() {
		t.Fatalf("re-asked question should be open again, got %+v", conv.Entries[1])
	}
}

func TestMalformedMinorAndOrphanLinesAreNotedNotFatal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, QuestionsFile,
		`{"id":"q1","question":"real","options":["a"]}`,
		`not json at all`,
		`{"question":"no id","options":["a"]}`,
		`{"id":"q9","question":"minor thing","weight":"minor","options":["a"]}`,
		`{"id":"q8","question":"","options":["a"]}`,
		`{"id":"q7","kind":"shout","question":"odd kind"}`,
		`{"id":"q6","kind":"retract"}`,
	)
	write(t, dir, AnswersFile,
		`{"id":"q1","answer":"a"}`,
		`{"id":"nope","answer":"whatever"}`,
		`{"id":"q1","answer":""}`,
		`garbage`,
	)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Entries) != 1 || conv.Entries[0].ID != "q1" {
		t.Fatalf("entries = %+v, want only q1", conv.Entries)
	}
	if !conv.Entries[0].Answered() {
		t.Fatalf("q1 should be answered")
	}
	// A minor question is a protocol violation, never an escalation: it must
	// not become an open question the run parks on.
	if len(conv.Open()) != 0 {
		t.Fatalf("open = %+v, want none", conv.Open())
	}
	joined := strings.Join(conv.Notes, "\n")
	for _, want := range []string{"malformed questions", "no id", "minor-weight question", "no question text", "unknown kind", "retraction for unknown question", "answer for unknown question", "malformed answers"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("notes missing %q:\n%s", want, joined)
		}
	}
}

func TestAppendRoundTripsAndValidates(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := AppendQuestion(dir, Question{ID: "q1", Question: "keep /v1?", Options: []string{"keep", "drop"}}); err != nil {
		t.Fatalf("AppendQuestion: %v", err)
	}
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "drop", AnsweredBy: "captain"}); err != nil {
		t.Fatalf("AppendAnswer: %v", err)
	}
	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Answered()) != 1 {
		t.Fatalf("conversation = %+v", conv)
	}
	entry := conv.Answered()[0]
	if entry.Weight != WeightMajor || entry.AskedAt == "" || entry.Answer.AnsweredAt == "" {
		t.Fatalf("defaults not applied: %+v", entry)
	}

	if err := AppendQuestion(dir, Question{Question: "no id"}); err == nil {
		t.Fatal("want error for a question with no id")
	}
	if err := AppendQuestion(dir, Question{ID: "q2"}); err == nil {
		t.Fatal("want error for a question with no text")
	}
	if err := AppendAnswer(dir, Answer{ID: "q2"}); err == nil {
		t.Fatal("want error for an answer with no text")
	}
	if err := AppendAnswer("", Answer{ID: "q2", Answer: "x"}); err == nil {
		t.Fatal("want error for an unset directory")
	}
	if err := AppendQuestion(dir, Question{ID: "big", Question: strings.Repeat("x", maxLineBytes)}); err == nil {
		t.Fatal("want error for an overlong line")
	}
}

func TestLoadKeepsTheNewestLinesWhenTheFileIsOverBound(t *testing.T) {
	dir := t.TempDir()
	lines := make([]string, 0, maxLines+10)
	for i := 0; i < maxLines+10; i++ {
		lines = append(lines, fmt.Sprintf(`{"id":"q%d","question":"q %d","options":["a"]}`, i, i))
	}
	write(t, dir, QuestionsFile, lines...)

	conv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conv.Entries) != maxLines {
		t.Fatalf("entries = %d, want the %d newest", len(conv.Entries), maxLines)
	}
	// Newest kept: a later line supersedes an earlier one, so dropping the
	// oldest is the only truncation that cannot lose a supersede.
	if conv.Entries[0].ID != "q10" {
		t.Fatalf("first kept = %q, want q10", conv.Entries[0].ID)
	}
	if !strings.Contains(strings.Join(conv.Notes, "\n"), "size bound") {
		t.Fatalf("truncation not disclosed: %v", conv.Notes)
	}
}

func TestDirIsUnderTheRunEvidenceDirectory(t *testing.T) {
	if got := Dir("/var/evidence/run1"); got != filepath.Join("/var/evidence/run1", "review") {
		t.Fatalf("Dir = %q", got)
	}
	if got := Dir("   "); got != "" {
		t.Fatalf("Dir(blank) = %q, want empty", got)
	}
}

// TestLoadSupersedingQuestionReopensAnAnsweredEntry is the regression for the
// way a major question could be silently discarded.
//
// Question ids are chosen by the agent - the protocol's worked example is
// literally "q1" - and the conversation directory is per RUN, so every review
// turn of a run appends to one file. A cold rereview in a fix round is shown
// only the OPEN questions, so it reuses "q1" for a genuinely different
// question. While the edit branch kept the earlier answer attached, that new
// question arrived pre-answered: Open() was empty, no ask-user finding was
// emitted, the gate never parked, and the reviewer's question reached nobody.
func TestLoadSupersedingQuestionReopensAnAnsweredEntry(t *testing.T) {
	dir := t.TempDir()

	if err := AppendQuestion(dir, Question{
		ID: "q1", Question: "keep the legacy /v1 route?", Options: []string{"keep", "remove"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "keep", AnsweredBy: "captain"}); err != nil {
		t.Fatal(err)
	}
	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Open()) != 0 || len(conv.Answered()) != 1 {
		t.Fatalf("answered question is not settled: open=%d answered=%d", len(conv.Open()), len(conv.Answered()))
	}

	// A later turn reuses the id for a different question.
	if err := AppendQuestion(dir, Question{
		ID: "q1", Question: "should the new /v3 route answer too?", Options: []string{"yes", "no"},
	}); err != nil {
		t.Fatal(err)
	}
	conv, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	open := conv.Open()
	if len(open) != 1 {
		t.Fatalf("a superseding question did not re-open the entry: open=%d answered=%d", len(open), len(conv.Answered()))
	}
	if open[0].Question.Question != "should the new /v3 route answer too?" {
		t.Fatalf("open question text = %q", open[0].Question.Question)
	}
	if open[0].Answer != nil {
		t.Fatalf("the superseded answer is still attached: %#v", open[0].Answer)
	}
	if len(conv.Answered()) != 0 {
		t.Fatalf("the new question still reads as answered: %#v", conv.Answered())
	}

	// Answering it again settles the NEW question, with the new text.
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "no", AnsweredBy: "captain"}); err != nil {
		t.Fatal(err)
	}
	conv, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	answered := conv.Answered()
	if len(answered) != 1 || answered[0].Answer.Answer != "no" || answered[0].Question.Question != "should the new /v3 route answer too?" {
		t.Fatalf("re-answer did not settle the new question: %#v", answered)
	}
}

// Re-asking a question the reviewer had retracted still revives it, and it
// comes back OPEN rather than carrying whatever answer preceded the
// retraction.
func TestLoadReAskAfterRetractionComesBackOpen(t *testing.T) {
	dir := t.TempDir()
	for _, q := range []Question{
		{ID: "q1", Question: "keep the legacy route?", Options: []string{"keep", "remove"}},
		{ID: "q1", Kind: KindRetract, Reason: "the migration note answers it"},
	} {
		if err := AppendQuestion(dir, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := AppendAnswer(dir, Answer{ID: "q1", Answer: "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendQuestion(dir, Question{
		ID: "q1", Question: "keep the legacy route after all?", Options: []string{"keep", "remove"},
	}); err != nil {
		t.Fatal(err)
	}
	conv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Withdrawn()) != 0 {
		t.Fatalf("re-asking did not revive the question: %#v", conv.Withdrawn())
	}
	if len(conv.Open()) != 1 {
		t.Fatalf("a revived question must be open, not pre-answered: open=%d answered=%d", len(conv.Open()), len(conv.Answered()))
	}
}
