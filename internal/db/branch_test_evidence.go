package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BranchTestEvidence is one earlier run's recorded test findings on this
// branch, paired with the run it belongs to so a reuse can point at it.
type BranchTestEvidence struct {
	RunID        string
	FindingsJSON string
}

// GetBranchTestEvidence returns the single most recently completed test step
// of another run on the same repo and branch that still carries a findings
// payload, or nil when there is none.
//
// Only the newest one is returned because only the newest one is evidence
// about where this branch now stands: an older verdict that a later run has
// already superseded must never outrank it.
//
// Branch scope is the whole point: a verdict is evidence about one branch's
// head, so it is never visible to another branch. A fix round can clear a
// step's final findings, in which case that run simply has nothing to offer
// and the caller falls through to running the evidence agent.
func (d *DB) GetBranchTestEvidence(repoID, branch, excludeRunID string) (*BranchTestEvidence, error) {
	var entry BranchTestEvidence
	err := d.sql.QueryRow(
		`SELECT res.run_id, res.findings_json
		   FROM step_results res
		   JOIN runs r ON r.id = res.run_id
		  WHERE r.repo_id = ? AND r.branch = ? AND r.id != ?
		    AND res.step_name = ? AND res.status = ?
		    AND res.findings_json IS NOT NULL
		  ORDER BY res.completed_at DESC, res.id DESC
		  LIMIT 1`,
		repoID, branch, excludeRunID,
		string(types.StepTest), string(types.StepStatusCompleted),
	).Scan(&entry.RunID, &entry.FindingsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get branch test evidence: %w", err)
	}
	return &entry, nil
}
