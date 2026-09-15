package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Bounds on the published handoff reports, for the same reason the review
// conversation has them: the PR body competes for a host-specific budget
// (Azure DevOps caps a whole description at 4000 characters), and a report is
// a RECORD rather than a gate. It rides inside the Pipeline section as an
// ordinary `### ` group the existing budget logic can drop whole.
const (
	maxPublishedHandoffNotes = 10
	maxPublishedHandoffChars = 200
)

// buildHandoffReportsSection renders what the review step handed to the later
// steps and what each of them did with it.
//
// Both halves come from the run's own step rows: the reports from the review
// step's findings payload, and the per-note outcomes from the document and
// lint steps' payloads. A note nobody reported an outcome for is published as
// unaddressed - the reviewer deliberately did not park the run on it, so the
// PR is where a reader learns it was dropped.
func buildHandoffReportsSection(steps []*db.StepResult) string {
	doc, lint := handoffReportsFromSteps(steps)
	if len(doc) == 0 && len(lint) == 0 {
		return ""
	}
	docOutcomes := appliedNotesFromStep(steps, types.StepDocument)
	lintOutcomes := appliedNotesFromStep(steps, types.StepLint)
	// The combined document+lint housekeeping pass records the lint half's
	// outcomes on the lint step's row, but a repository with no lint command
	// and a fallback pass can record them on either, so both rows are
	// consulted for each report.
	all := append(append([]types.HandoffOutcome(nil), docOutcomes...), lintOutcomes...)

	var b strings.Builder
	b.WriteString("### Review handoff reports\n\n")
	b.WriteString("The reviewer recorded these instead of blocking the run; the document and lint steps own the remedies.\n")
	writeHandoffReport(&b, "Doc report", "document", doc, all)
	writeHandoffReport(&b, "Lint report", "lint", lint, all)
	// The notes quote agent text, so they can carry a foreign attestation
	// marker; verify.py binds the FIRST marker in the raw body, and this
	// section is appended after the real one.
	return neutralizeAttestationMarkers(strings.TrimRight(b.String(), "\n"))
}

func writeHandoffReport(b *strings.Builder, title, owner string, notes []types.HandoffNote, outcomes []types.HandoffOutcome) {
	if len(notes) == 0 {
		return
	}
	applied := 0
	for _, note := range notes {
		if o, ok := handoffOutcomeNote(outcomes, note.ID); ok && o.Applied {
			applied++
		}
	}
	fmt.Fprintf(b, "\n**%s** (%d note(s), %d applied by the %s step):\n\n", title, len(notes), applied, owner)
	shown := 0
	omitted := 0
	for _, note := range notes {
		if shown >= maxPublishedHandoffNotes {
			omitted++
			continue
		}
		shown++
		where := publishedHandoffText(note.File)
		if where != "" && note.Line > 0 {
			where = fmt.Sprintf("%s:%d", where, note.Line)
		}
		if where != "" {
			where = " `" + where + "`"
		}
		fmt.Fprintf(b, "- **%s**%s %s\n", publishedHandoffText(note.ID), where, publishedHandoffText(note.Problem))
		outcome, ok := handoffOutcomeNote(outcomes, note.ID)
		switch {
		case !ok:
			b.WriteString("  **Not addressed:** the step reported no outcome for this note.\n")
		case outcome.Applied:
			detail := publishedHandoffText(outcome.Note)
			if detail == "" {
				detail = "applied."
			}
			fmt.Fprintf(b, "  **Applied:** %s\n", detail)
		default:
			detail := publishedHandoffText(outcome.Note)
			if detail == "" {
				detail = "no reason given."
			}
			fmt.Fprintf(b, "  **Not applied:** %s\n", detail)
		}
	}
	if omitted > 0 {
		fmt.Fprintf(b, "\n%d further note(s) omitted for length.\n", omitted)
	}
}

// handoffReportsFromSteps reads the reports off the review step's row, which
// holds the last round's full report.
func handoffReportsFromSteps(steps []*db.StepResult) (doc, lint []types.HandoffNote) {
	for _, sr := range steps {
		if sr == nil || sr.StepName != types.StepReview || sr.FindingsJSON == nil {
			continue
		}
		parsed, err := types.ParseFindingsJSON(*sr.FindingsJSON)
		if err != nil {
			continue
		}
		doc, lint = parsed.DocReport, parsed.LintReport
	}
	return doc, lint
}

func appliedNotesFromStep(steps []*db.StepResult, name types.StepName) []types.HandoffOutcome {
	var out []types.HandoffOutcome
	for _, sr := range steps {
		if sr == nil || sr.StepName != name || sr.FindingsJSON == nil {
			continue
		}
		parsed, err := types.ParseFindingsJSON(*sr.FindingsJSON)
		if err != nil {
			continue
		}
		out = parsed.AppliedNotes
	}
	return out
}

// publishedHandoffText flattens and bounds one quoted note field, so a long
// note cannot dominate the body and a newline cannot break out of its list
// item.
func publishedHandoffText(s string) string {
	s = sanitizePromptText(s)
	if len(s) <= maxPublishedHandoffChars {
		return s
	}
	return s[:maxPublishedHandoffChars] + fmt.Sprintf("… (truncated, %d chars total)", len(s))
}
