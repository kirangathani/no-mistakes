package db

import (
	"path/filepath"
	"testing"
)

// The fleet's daemon database was created by the fork's early review
// conversation, whose review_questions table has no ask_ordinal and a
// (repo, branch, question) key. Opening it with upstream's code must rebuild
// the table in place, keep every settled answer, and leave upstream's reads
// and writes working.
func TestOpenMigratesTheForkReviewQuestionsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head-1", "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE review_questions`,
		`CREATE TABLE review_questions (
    repo_id      TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch       TEXT NOT NULL,
    question_id  TEXT NOT NULL,
    run_id       TEXT NOT NULL,
    question     TEXT NOT NULL,
    options_json TEXT,
    file         TEXT,
    line         INTEGER,
    answer       TEXT NOT NULL,
    answered_by  TEXT,
    answered_at  TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (repo_id, branch, question_id)
)`,
	} {
		if _, err := d.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.sql.Exec(
		`INSERT INTO review_questions (repo_id, branch, question_id, run_id, question, options_json, file, line, answer, answered_by, answered_at, created_at, updated_at)
		 VALUES (?, 'feature', 'q1', ?, 'keep /v1?', '["keep","drop"]', 'api.go', 7, 'keep', 'captain', '2026-09-15T13:31:40Z', 10, 10),
		        (?, 'feature', 'q2', ?, 'rename?', NULL, NULL, NULL, 'no', NULL, NULL, 11, 11)`,
		repo.ID, run.ID, repo.ID, run.ID,
	); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen fork-shaped db: %v", err)
	}
	if has, err := tableHasColumn(d.sql, "review_questions", "ask_ordinal"); err != nil || !has {
		t.Fatalf("ask_ordinal present = %v, err = %v", has, err)
	}

	answers, _, err := d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || answers[0].QuestionID != "q2" || answers[1].QuestionID != "q1" {
		t.Fatalf("migrated answers = %#v", answers)
	}
	q1 := answers[1]
	if q1.AskOrdinal != 1 || q1.RunID != run.ID || q1.Answer != "keep" || q1.AnsweredBy != "captain" ||
		len(q1.Options) != 2 || q1.File != "api.go" || q1.Line != 7 {
		t.Fatalf("migrated q1 lost fields: %#v", q1)
	}

	// Upstream's keyed upsert works on the rebuilt table: the same ask is
	// corrected in place, a second ask of the id gets its own row.
	for _, a := range []ReviewAnswer{
		{RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: run.ID, AskOrdinal: 1, Question: "keep /v1?", Answer: "drop"},
		{RepoID: repo.ID, Branch: "feature", QuestionID: "q1", RunID: run.ID, AskOrdinal: 2, Question: "other", Answer: "yes"},
	} {
		if err := d.RecordReviewAnswer(a); err != nil {
			t.Fatal(err)
		}
	}
	answers, _, err = d.GetBranchReviewAnswers(repo.ID, "feature", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 3 {
		t.Fatalf("answers after upsert = %#v, want 3", answers)
	}

	// A third Open is a no-op.
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	d.Close()
}
