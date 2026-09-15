package db

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PreviousReviewRounds is the review step of the most recent OTHER run on a
// branch, with the rounds it recorded.
type PreviousReviewRounds struct {
	RunID  string
	Rounds []*StepRound
}

// GetPreviousRunReviewRounds returns the review rounds of the most recently
// created run on this repo and branch other than excludeRunID, or nil when
// there is no such run or it recorded no review round.
//
// It exists for the supersede case. When the change author fixes review
// findings in their own worktree and pushes, that push supersedes the parked
// run and a NEW run starts, whose review step has no round history at all:
// step_rounds is scoped to one step_result, and the uncertified-range channel
// covers only PIPELINE-authored commits a previous run left uncertified. The
// next reviewer would otherwise be cold not only on the code - which is
// correct and deliberate - but on the fact that a conversation happened at
// all.
//
// "Most recent other run" is the right selector rather than "the superseded
// run": a superseded run is not marked as such, the branch's own run history
// is the record, and the immediately preceding run is the one whose rounds
// describe the findings this push was answering.
func (d *DB) GetPreviousRunReviewRounds(repoID, branch, excludeRunID string) (*PreviousReviewRounds, error) {
	rows, err := d.sql.Query(
		`SELECT res.run_id, res.id
		   FROM step_results res
		   JOIN runs r ON r.id = res.run_id
		  WHERE r.repo_id = ? AND r.branch = ? AND r.id != ? AND res.step_name = ?
		  ORDER BY r.created_at DESC, r.id DESC
		  LIMIT 1`,
		repoID, branch, excludeRunID, string(types.StepReview),
	)
	if err != nil {
		return nil, fmt.Errorf("get previous run review step: %w", err)
	}
	defer rows.Close()
	var runID, stepResultID string
	if !rows.Next() {
		return nil, rows.Err()
	}
	if err := rows.Scan(&runID, &stepResultID); err != nil {
		return nil, fmt.Errorf("scan previous run review step: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return nil, fmt.Errorf("get previous run review rounds: %w", err)
	}
	if len(rounds) == 0 {
		return nil, nil
	}
	return &PreviousReviewRounds{RunID: runID, Rounds: rounds}, nil
}
