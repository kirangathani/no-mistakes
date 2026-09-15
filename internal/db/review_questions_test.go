package db

import "testing"

func TestReviewAnswersAreKeyedByBranchAndSurviveANewRun(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.InsertRun(repo.ID, "feature", "head-1", "base")
	if err != nil {
		t.Fatal(err)
	}

	answers, truncated, err := d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 0 || truncated {
		t.Fatalf("empty branch = %#v truncated=%v", answers, truncated)
	}

	if err := d.RecordReviewAnswer(ReviewAnswer{
		RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: first.ID,
		Question: "keep /v1?", Options: []string{"keep", "drop"},
		File: "internal/api/router.go", Line: 88,
		Answer: "keep behind a flag", AnsweredBy: "captain", AnsweredAt: "2026-09-15T13:31:40Z",
	}); err != nil {
		t.Fatal(err)
	}

	// A later run on the same branch must read it: the whole point is that the
	// next COLD reviewer does not re-ask a settled question.
	second, err := d.InsertRun(repo.ID, "feature", "head-2", "base")
	if err != nil {
		t.Fatal(err)
	}
	answers, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 {
		t.Fatalf("branch answers = %#v, want 1", answers)
	}
	got := answers[0]
	if got.Answer != "keep behind a flag" || got.AnsweredBy != "captain" || got.RunID != first.ID {
		t.Fatalf("answer = %#v", got)
	}
	if len(got.Options) != 2 || got.Options[0] != "keep" || got.File != "internal/api/router.go" || got.Line != 88 {
		t.Fatalf("round-trip lost fields: %#v", got)
	}

	// A correction replaces rather than accumulating, matching the file
	// protocol where the last answers.ndjson line for an id wins.
	if err := d.RecordReviewAnswer(ReviewAnswer{
		RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: second.ID,
		Question: "keep /v1?", Answer: "drop it", AnsweredBy: "firstmate",
	}); err != nil {
		t.Fatal(err)
	}
	answers, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Answer != "drop it" || answers[0].RunID != second.ID {
		t.Fatalf("correction = %#v", answers)
	}

	// Another branch's conversation is not visible.
	if err := d.RecordReviewAnswer(ReviewAnswer{
		RepoID: repo.ID, Branch: "other", QuestionID: "q1", RunID: second.ID,
		Question: "unrelated", Answer: "yes",
	}); err != nil {
		t.Fatal(err)
	}
	answers, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 {
		t.Fatalf("branch scoping broken: %#v", answers)
	}
}

func TestGetBranchReviewAnswersBoundsAndReportsTruncation(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"q1", "q2", "q3"} {
		if err := d.RecordReviewAnswer(ReviewAnswer{
			RepoID: repo.ID, Branch: "feature", QuestionID: id, RunID: run.ID,
			Question: "q " + id, Answer: "a " + id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	answers, truncated, err := d.GetBranchReviewAnswers(repo.ID, "feature", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || !truncated {
		t.Fatalf("answers = %d truncated = %v, want 2 and true", len(answers), truncated)
	}
}

func TestGetRunReviewAnswersIsScopedToOneRun(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	a, err := d.InsertRun(repo.ID, "feature", "head-1", "base")
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.InsertRun(repo.ID, "feature", "head-2", "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct{ id, runID string }{{"q1", a.ID}, {"q2", b.ID}} {
		if err := d.RecordReviewAnswer(ReviewAnswer{
			RepoID: repo.ID, Branch: "feature", QuestionID: spec.id, RunID: spec.runID,
			Question: "q", Answer: "a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.GetRunReviewAnswers(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].QuestionID != "q2" {
		t.Fatalf("run answers = %#v", got)
	}
}

func TestRecordReviewAnswerRequiresItsKey(t *testing.T) {
	d := openTestDB(t)
	if err := d.RecordReviewAnswer(ReviewAnswer{Branch: "feature", QuestionID: "q1", Answer: "a"}); err == nil {
		t.Fatal("want error with no repo id")
	}
	if err := d.RecordReviewAnswer(ReviewAnswer{RepoID: "r", QuestionID: "q1", Answer: "a"}); err == nil {
		t.Fatal("want error with no branch")
	}
	if err := d.RecordReviewAnswer(ReviewAnswer{RepoID: "r", Branch: "feature", Answer: "a"}); err == nil {
		t.Fatal("want error with no question id")
	}
}
