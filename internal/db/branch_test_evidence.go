package db

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// maxBranchTestEvidenceScan bounds how many earlier test steps on a branch are
// examined for a reusable verdict. Only the most recent one that still carries
// a readable verdict is ever reused, so this is just how far back to look
// before giving up and letting the evidence agent run.
const maxBranchTestEvidenceScan = 5

// BranchTestEvidence is one earlier run's recorded test findings on this
// branch, paired with the run it belongs to so a reuse can point at it.
type BranchTestEvidence struct {
	RunID        string
	FindingsJSON string
}

// GetBranchTestEvidence returns the completed test steps of OTHER runs on the
// same repo and branch that still carry a findings payload, most recently
// completed first.
//
// Branch scope is the whole point: a verdict is evidence about one branch's
// head, so it is never visible to another branch. A fix round can clear a
// step's final findings, in which case that run simply has nothing to offer
// and the caller falls through to running the evidence agent.
func (d *DB) GetBranchTestEvidence(repoID, branch, excludeRunID string) ([]BranchTestEvidence, error) {
	rows, err := d.sql.Query(
		`SELECT res.run_id, res.findings_json
		   FROM step_results res
		   JOIN runs r ON r.id = res.run_id
		  WHERE r.repo_id = ? AND r.branch = ? AND r.id != ?
		    AND res.step_name = ? AND res.status = ?
		    AND res.findings_json IS NOT NULL
		  ORDER BY res.completed_at DESC, res.id DESC
		  LIMIT ?`,
		repoID, branch, excludeRunID,
		string(types.StepTest), string(types.StepStatusCompleted),
		maxBranchTestEvidenceScan,
	)
	if err != nil {
		return nil, fmt.Errorf("get branch test evidence: %w", err)
	}
	defer rows.Close()

	var evidence []BranchTestEvidence
	for rows.Next() {
		var entry BranchTestEvidence
		if err := rows.Scan(&entry.RunID, &entry.FindingsJSON); err != nil {
			return nil, fmt.Errorf("scan branch test evidence: %w", err)
		}
		evidence = append(evidence, entry)
	}
	return evidence, rows.Err()
}
