package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func handoffFindingsJSON(t *testing.T, findings types.Findings) string {
	t.Helper()
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}
	return encoded
}

// An agent reading `axi status` has to see what the reviewer handed to a later
// step instead of parking on it - otherwise the work is invisible until the PR
// body exists, and a note nobody applied reads as nothing at all.
func TestRunObjectRendersTheReviewHandoffReports(t *testing.T) {
	rv := runView{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  string(types.RunRunning),
		HeadSHA: "abcdef1234567890",
		Steps: []stepView{
			{Name: string(types.StepReview), Status: "completed", FindingsJSON: handoffFindingsJSON(t, types.Findings{
				DocReport: []types.HandoffNote{
					{ID: "doc1", File: "README.md", Line: 42, Problem: "stale default", RightLooksLike: "state the new default"},
					{ID: "doc2", File: "SETUP.md", Problem: "missing step"},
				},
				LintReport: []types.HandoffNote{{ID: "lint1", File: "internal/a/a.go", Line: 7, Problem: "unused import"}},
			})},
			{Name: string(types.StepDocument), Status: "completed", FindingsJSON: handoffFindingsJSON(t, types.Findings{
				AppliedNotes: []types.HandoffOutcome{
					{ID: "doc1", Applied: true, Note: "updated the flag table"},
					// What the document step records for a note its agent
					// never mentioned.
					{ID: "doc2", Applied: false, Note: "the step reported no outcome for this note"},
				},
			})},
		},
	}

	out := axiDoc(runObjectField(rv))
	for _, want := range []string{
		"doc_report[2]{id,file,problem,right_looks_like,outcome}:\n",
		"README.md:42",
		"applied: updated the flag table",
		// doc2 was dropped by the document step and the lint step has not run
		// yet: both must read honestly rather than as done.
		"not applied: the step reported no outcome for this note",
		"lint_report[1]{id,file,problem,right_looks_like,outcome}:\n",
		"pending the owning step",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run object missing %q in:\n%s", want, out)
		}
	}
}

// A run whose reviewer recorded nothing - and every run recorded before the
// reports existed - renders exactly as before.
func TestRunObjectOmitsEmptyHandoffReports(t *testing.T) {
	rv := runView{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  string(types.RunRunning),
		HeadSHA: "abcdef1234567890",
		Steps:   []stepView{{Name: string(types.StepReview), Status: "completed", FindingsJSON: `{"findings":[],"summary":"clean"}`}},
	}
	out := axiDoc(runObjectField(rv))
	if strings.Contains(out, "doc_report") || strings.Contains(out, "lint_report") {
		t.Errorf("empty reports were rendered:\n%s", out)
	}
}
