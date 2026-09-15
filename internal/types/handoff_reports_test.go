package types

import "testing"

// A handoff report must survive the wire round trip: findingsWire copies every
// field explicitly, so a field missing from it is silently dropped on parse.
func TestParseFindingsJSON_RoundTripsHandoffReports(t *testing.T) {
	encoded, err := MarshalFindingsJSON(Findings{
		Summary:       "reviewed",
		RiskLevel:     "low",
		RiskRationale: "bounded",
		RiskScope:     FindingsRiskScopeSourceOrExternal,
		DocReport: []HandoffNote{{
			ID: "doc1", File: "README.md", Line: 42,
			Problem: "README still claims the flag defaults to false", RightLooksLike: "state the new default",
		}},
		LintReport:   []HandoffNote{{ID: "lint1", File: "internal/a/a.go", Line: 7, Problem: "unused import"}},
		AppliedNotes: []HandoffOutcome{{ID: "doc1", Applied: true, Note: "updated the flag table"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseFindingsJSON(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.DocReport) != 1 || parsed.DocReport[0].ID != "doc1" || parsed.DocReport[0].Line != 42 {
		t.Errorf("doc report lost in the round trip: %+v", parsed.DocReport)
	}
	if parsed.DocReport[0].RightLooksLike != "state the new default" {
		t.Errorf("right_looks_like lost: %+v", parsed.DocReport[0])
	}
	if len(parsed.LintReport) != 1 || parsed.LintReport[0].Problem != "unused import" {
		t.Errorf("lint report lost in the round trip: %+v", parsed.LintReport)
	}
	if len(parsed.AppliedNotes) != 1 || !parsed.AppliedNotes[0].Applied || parsed.AppliedNotes[0].ID != "doc1" {
		t.Errorf("applied notes lost in the round trip: %+v", parsed.AppliedNotes)
	}
}

// FindingsMetadata keeps the whole evidence payload when items are re-selected,
// so a fix round that filters findings must not drop the reports with them.
func TestFindingsMetadata_KeepsHandoffReports(t *testing.T) {
	kept := FindingsMetadata(Findings{
		Items:      []Finding{{Severity: FindingSeverityError, Description: "boom", Action: ActionAutoFix}},
		DocReport:  []HandoffNote{{ID: "doc1", Problem: "stale line"}},
		LintReport: []HandoffNote{{ID: "lint1", Problem: "unused import"}},
	})
	if len(kept.Items) != 0 {
		t.Errorf("expected items dropped, got %d", len(kept.Items))
	}
	if len(kept.DocReport) != 1 || len(kept.LintReport) != 1 {
		t.Errorf("handoff reports dropped by FindingsMetadata: %+v", kept)
	}
}
