package db

import (
	"database/sql"
	"fmt"
)

// migrateForkReviewQuestions upgrades a review_questions table created by the
// kirangathani fork's early review conversation (fork #3, before it was
// replaced by upstream #1104) to upstream's shape.
//
// The fork's table has no ask_ordinal column and is keyed (repo_id, branch,
// question_id). schemaSQL's CREATE TABLE IF NOT EXISTS leaves such a table in
// place, and every upstream read and write names ask_ordinal, so without this
// the first review on an upgraded daemon fails on "no such column". SQLite
// cannot change a primary key in place, so the table is rebuilt in one
// transaction. Each old row was the only answer the fork kept for its
// question on that branch, so it becomes ask 1 of the run that recorded it;
// rows are copied in rowid order because GetBranchReviewAnswers breaks
// recency ties on rowid.
//
// It is a no-op on a fresh database and on any table that already has
// ask_ordinal, so it is safe to run on every Open.
func migrateForkReviewQuestions(sqlDB *sql.DB) error {
	has, err := tableHasColumn(sqlDB, "review_questions", "ask_ordinal")
	if err != nil || has {
		return err
	}
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("migrate fork review_questions: %w", err)
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`ALTER TABLE review_questions RENAME TO review_questions_fork_v1`,
		`CREATE TABLE review_questions (
    repo_id      TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch       TEXT NOT NULL,
    question_id  TEXT NOT NULL,
    run_id       TEXT NOT NULL,
    ask_ordinal  INTEGER NOT NULL,
    question     TEXT NOT NULL,
    options_json TEXT,
    file         TEXT,
    line         INTEGER,
    answer       TEXT NOT NULL,
    answered_by  TEXT,
    answered_at  TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (repo_id, branch, question_id, run_id, ask_ordinal)
)`,
		`INSERT INTO review_questions
    (repo_id, branch, question_id, run_id, ask_ordinal, question, options_json, file, line,
     answer, answered_by, answered_at, created_at, updated_at)
 SELECT repo_id, branch, question_id, run_id, 1, question, options_json, file, line,
        answer, answered_by, answered_at, created_at, updated_at
   FROM review_questions_fork_v1 ORDER BY rowid`,
		`DROP TABLE review_questions_fork_v1`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate fork review_questions: %w", err)
		}
	}
	return tx.Commit()
}

func tableHasColumn(sqlDB *sql.DB, table, column string) (bool, error) {
	rows, err := sqlDB.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("inspect %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
