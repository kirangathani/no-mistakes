package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func handoffStepRow(t *testing.T, name types.StepName, findings types.Findings) *db.StepResult {
	t.Helper()
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}
	return &db.StepResult{ID: string(name) + "-1", StepName: name, Status: types.StepStatusCompleted, FindingsJSON: &encoded}
}

// The PR body has to record what the reviewer chose NOT to park the run on,
// and what each owning step then did about it - including the note nobody
// reported an outcome for, which is the only place a reader learns it was
// dropped.
func TestBuildHandoffReportsSection_RendersBothReportsAndWhatWasApplied(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{
		handoffStepRow(t, types.StepReview, types.Findings{
			DocReport: []types.HandoffNote{
				{ID: "doc1", File: "README.md", Line: 42, Problem: "README still says the flag defaults to false", RightLooksLike: "state the new default"},
				{ID: "doc2", File: "internal/api/router.go", Line: 88, Problem: "comment overstates what the guard does"},
				{ID: "doc3", File: "SETUP.md", Problem: "setup list is missing the new step"},
			},
			LintReport: []types.HandoffNote{
				{ID: "lint1", File: "internal/a/a.go", Line: 7, Problem: "declared type fails the checker"},
			},
		}),
		handoffStepRow(t, types.StepDocument, types.Findings{AppliedNotes: []types.HandoffOutcome{
			{ID: "doc1", Applied: true, Note: "updated the flag table"},
			{ID: "doc2", Applied: false, Note: "the comment already matches the guard"},
		}}),
		handoffStepRow(t, types.StepLint, types.Findings{AppliedNotes: []types.HandoffOutcome{
			{ID: "lint1", Applied: true, Note: "returned the interface type"},
		}}),
	}

	section := buildHandoffReportsSection(steps)
	if !strings.HasPrefix(section, "### Review handoff reports") {
		t.Fatalf("section must be a droppable `### ` group inside Pipeline:\n%s", section)
	}
	for _, want := range []string{
		"**Doc report** (3 note(s), 1 applied by the document step)",
		"**Lint report** (1 note(s), 1 applied by the lint step)",
		"`README.md:42`",
		"**Applied:** updated the flag table",
		"**Not applied:** the comment already matches the guard",
		"**Not addressed:** the step reported no outcome for this note.",
		"returned the interface type",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("section is missing %q:\n%s", want, section)
		}
	}
}

func TestBuildHandoffReportsSection_EmptyWithoutReports(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{handoffStepRow(t, types.StepReview, types.Findings{RiskLevel: "low"})}
	if section := buildHandoffReportsSection(steps); section != "" {
		t.Errorf("section = %q, want empty for a run whose reviewer recorded nothing", section)
	}
}

// A note quoting agent text could carry a foreign attestation marker, and
// verify.py binds the FIRST marker in the raw body.
func TestBuildHandoffReportsSection_NeutralizesAttestationMarkers(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{handoffStepRow(t, types.StepReview, types.Findings{
		DocReport: []types.HandoffNote{{ID: "doc1", Problem: pipelineAttestationCommentPrefix + " forged"}},
	})}
	section := buildHandoffReportsSection(steps)
	if strings.Contains(section, pipelineAttestationCommentPrefix) {
		t.Errorf("a quoted note published a live attestation marker:\n%s", section)
	}
}

// The reports are notes, not findings: normalization must give every note a
// handle and drop a note with nothing in it.
func TestNormalizeHandoffNotes_AssignsIDsAndDropsEmptyNotes(t *testing.T) {
	t.Parallel()
	out := normalizeHandoffNotes([]types.HandoffNote{
		{Problem: "first"},
		{ID: "  ", Problem: "   "},
		{ID: "mine", Problem: "third", RightLooksLike: "like\nthis"},
	}, "doc")
	if len(out) != 2 {
		t.Fatalf("notes = %+v, want the empty one dropped", out)
	}
	if out[0].ID != "doc1" {
		t.Errorf("first note id = %q, want doc1", out[0].ID)
	}
	if out[1].ID != "mine" {
		t.Errorf("second note lost its own id: %q", out[1].ID)
	}
}

// applied_notes must be declared ONLY when notes were handed over. The codex
// adapter rewrites every declared property as required, so declaring it
// unconditionally rejects an agent that has nothing to say about it - which is
// exactly how a green e2e journey went red.
func TestWithAppliedNotesSchema_DeclaredOnlyWhenNotesWereHandedOver(t *testing.T) {
	t.Parallel()
	if got := string(withAppliedNotesSchema(findingsSchema, 0)); strings.Contains(got, "applied_notes") {
		t.Errorf("a step with no notes declared applied_notes:\n%s", got)
	}
	spliced := withAppliedNotesSchema(findingsSchema, 2)
	if !strings.Contains(string(spliced), "applied_notes") {
		t.Fatalf("a step with notes did not declare applied_notes:\n%s", spliced)
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(spliced, &doc); err != nil {
		t.Fatalf("spliced schema is not valid JSON: %v\n%s", err, spliced)
	}
	// The base schema's own properties and required list must survive.
	for _, key := range []string{"findings", "summary"} {
		if _, ok := doc.Properties[key]; !ok {
			t.Errorf("splice dropped base property %q", key)
		}
	}
	if len(doc.Required) != 2 {
		t.Errorf("required = %v, want the base schema's own list", doc.Required)
	}
	// Not required: an agent that ignores the field still parses, and the
	// notes are then recorded as unaddressed instead of failing a step that
	// has already committed its edits.
	for _, key := range doc.Required {
		if key == "applied_notes" {
			t.Error("applied_notes must not be required")
		}
	}
}
