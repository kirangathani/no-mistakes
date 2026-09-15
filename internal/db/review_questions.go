package db

import (
	"encoding/json"
	"fmt"
	"time"
)

// MaxBranchReviewAnswers bounds how many settled review questions are loaded
// for one branch. Every loaded answer is rendered verbatim into a review
// prompt, so this is a prompt-budget ceiling, not a correctness limit: a
// branch with a longer conversation carries its most recent answers.
const MaxBranchReviewAnswers = 40

// ReviewAnswer is one question a review turn asked on this branch and the
// answer a human gave it.
type ReviewAnswer struct {
	RepoID     string
	Branch     string
	QuestionID string
	RunID      string
	Question   string
	Options    []string
	File       string
	Line       int
	Answer     string
	AnsweredBy string
	AnsweredAt string
	UpdatedAt  int64
}

// RecordReviewAnswer persists one answered review question for a branch.
//
// The write is an upsert keyed by (repo, branch, question): a corrected answer
// replaces the earlier one rather than accumulating, which matches the file
// protocol where the last answers.ndjson line for an id wins. run_id records
// which run's reviewer asked it and is deliberately not part of the key - the
// answer is about the branch, and a later run must not re-ask it.
func (d *DB) RecordReviewAnswer(a ReviewAnswer) error {
	if a.RepoID == "" || a.Branch == "" || a.QuestionID == "" {
		return fmt.Errorf("record review answer: repo, branch and question id are required")
	}
	var optionsJSON *string
	if len(a.Options) > 0 {
		encoded, err := json.Marshal(a.Options)
		if err != nil {
			return fmt.Errorf("record review answer: encode options: %w", err)
		}
		s := string(encoded)
		optionsJSON = &s
	}
	now := time.Now().Unix()
	_, err := d.sql.Exec(
		`INSERT INTO review_questions
		    (repo_id, branch, question_id, run_id, question, options_json, file, line,
		     answer, answered_by, answered_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id, branch, question_id) DO UPDATE SET
		    run_id = excluded.run_id,
		    question = excluded.question,
		    options_json = excluded.options_json,
		    file = excluded.file,
		    line = excluded.line,
		    answer = excluded.answer,
		    answered_by = excluded.answered_by,
		    answered_at = excluded.answered_at,
		    updated_at = excluded.updated_at`,
		a.RepoID, a.Branch, a.QuestionID, a.RunID, a.Question, optionsJSON,
		nullableText(a.File), nullableInt(a.Line),
		a.Answer, nullableText(a.AnsweredBy), nullableText(a.AnsweredAt), now, now,
	)
	if err != nil {
		return fmt.Errorf("record review answer: %w", err)
	}
	return nil
}

// GetBranchReviewAnswers returns the settled review questions for a branch,
// most recently answered first, bounded by limit. The truncated result reports
// whether older answers exist beyond the returned recency window.
func (d *DB) GetBranchReviewAnswers(repoID, branch string, limit int) ([]ReviewAnswer, bool, error) {
	if limit <= 0 {
		limit = MaxBranchReviewAnswers
	}
	rows, err := d.sql.Query(
		`SELECT repo_id, branch, question_id, run_id, question, options_json, file, line,
		        answer, answered_by, answered_at, updated_at
		   FROM review_questions
		  WHERE repo_id = ? AND branch = ?
		  ORDER BY updated_at DESC, question_id DESC
		  LIMIT ?`,
		repoID, branch, limit+1,
	)
	if err != nil {
		return nil, false, fmt.Errorf("get branch review answers: %w", err)
	}
	defer rows.Close()

	var answers []ReviewAnswer
	for rows.Next() {
		var a ReviewAnswer
		var optionsJSON, file, answeredBy, answeredAt *string
		var line *int64
		if err := rows.Scan(
			&a.RepoID, &a.Branch, &a.QuestionID, &a.RunID, &a.Question, &optionsJSON,
			&file, &line, &a.Answer, &answeredBy, &answeredAt, &a.UpdatedAt,
		); err != nil {
			return nil, false, fmt.Errorf("scan branch review answer: %w", err)
		}
		if optionsJSON != nil {
			_ = json.Unmarshal([]byte(*optionsJSON), &a.Options)
		}
		if file != nil {
			a.File = *file
		}
		if line != nil {
			a.Line = int(*line)
		}
		if answeredBy != nil {
			a.AnsweredBy = *answeredBy
		}
		if answeredAt != nil {
			a.AnsweredAt = *answeredAt
		}
		answers = append(answers, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(answers) > limit
	if truncated {
		answers = answers[:limit]
	}
	return answers, truncated, nil
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}
