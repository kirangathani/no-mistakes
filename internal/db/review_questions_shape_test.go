package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenRefusesAReviewQuestionsTableThatPredatesAskOrdinal covers the one
// database shape this feature cannot work against: review_questions gained
// ask_ordinal and a new primary key inside this feature's own branch, and
// CREATE TABLE IF NOT EXISTS is a no-op against a table that already exists.
//
// Every write names ask_ordinal in its column list and its ON CONFLICT target,
// and every read selects it, so against the old shape both fail - and both
// callers degrade rather than error, which made a human's settled decision
// vanish from the do-not-re-raise section and from the PR body with nothing
// reported. Opening has to say so instead.
func TestOpenRefusesAReviewQuestionsTableThatPredatesAskOrdinal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")

	// The table exactly as the earlier commit of this branch created it: no
	// ask_ordinal column and the three-column key.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE review_questions (
	    repo_id     TEXT NOT NULL,
	    branch      TEXT NOT NULL,
	    question_id TEXT NOT NULL,
	    run_id      TEXT NOT NULL,
	    question    TEXT NOT NULL,
	    answer      TEXT NOT NULL,
	    created_at  INTEGER NOT NULL,
	    updated_at  INTEGER NOT NULL,
	    PRIMARY KEY (repo_id, branch, question_id)
	)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(path)
	if err == nil {
		d.Close()
		t.Fatal("Open accepted a review_questions table that cannot store an ask ordinal")
	}
	for _, want := range []string{"review_questions", "ask_ordinal", "DROP TABLE review_questions"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q, so an operator cannot act on it: %v", want, err)
		}
	}

	// Refusing must not touch the database: the operator drops the table
	// themselves, and a tool that had already deleted it would take the rows
	// with it.
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var name string
	if err := check.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='review_questions'`).Scan(&name); err != nil {
		t.Fatalf("the refused open altered the database: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the refused open removed the database: %v", err)
	}
}

// A database this version created opens normally, which is every database an
// upstream installation can have: the table is new in this change, so
// CREATE TABLE gives it ask_ordinal and the five-column key.
func TestOpenAcceptsTheCurrentReviewQuestionsShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a database this version created was refused: %v", err)
	}
	second.Close()
}
