package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A reused verdict is the one shape where the Test step records a go for a
// head no live-evidence turn ever touched. Its scenarios really were driven
// live - in an earlier run, at another head - so the danger is not the
// scenario records themselves but the two places they turn into a claim about
// the commit the PR is showing: the attestation's live_validation object and
// the PR body's live-validation line.
//
// These cases pin both, and they pin the discrimination rather than mere
// absence: the same fixture with the head the agent actually drove must still
// publish its claim in full, or a "fix" that simply stopped emitting
// live_validation would pass.

const reusedDrivenHeadSHA = "fedcba9876543210fedcba9876543210fedcba98"

func reusedEvidenceFindingsJSON(t *testing.T, source, testedHead string) string {
	t.Helper()
	raw, err := json.Marshal(types.Findings{
		Tested:         []string{"`npm run e2e -- checkout`"},
		TestingSummary: "product files unchanged since fedcba9; reused from run run-1",
		Scenarios: []types.TestScenario{
			{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"},
			{Name: "expired token is refused", Result: types.ScenarioResultPass, Live: true, Evidence: "token.png"},
		},
		Verdict:        types.TestVerdictGo,
		TestedHeadSHA:  testedHead,
		EvidenceSource: source,
		EvidenceReason: "product files unchanged since fedcba9; reused from run run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func attestedLiveValidationObject(t *testing.T, findingsJSON, headSHA string) map[string]any {
	t.Helper()
	steps, rounds := testStepWithFindings(t, findingsJSON)
	raw := buildPipelineAttestation(steps, rounds, headSHA)
	payload := strings.TrimSuffix(strings.TrimPrefix(raw, pipelineAttestationCommentPrefix), pipelineAttestationCommentClosingToken)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("attestation payload did not parse: %v (%s)", err, payload)
	}
	if live, present := decoded["live_validation"]; present {
		obj, ok := live.(map[string]any)
		if !ok {
			t.Fatalf("live_validation is not an object: %v", live)
		}
		return obj
	}
	return nil
}

// TestAttestationMakesNoLiveClaimForAHeadNoTurnDrove guards the MECHANISM the
// reuse fix leans on, not the fix itself: that a recorded head which is not
// the published head yields no live_validation object at all. The step-driving
// half - that a reused verdict is never restamped onto this run's head - is
// pinned by TestTestStep_ReusesAGoVerdictWhenProductFilesAreUnchanged in the
// testgate package. Both have to hold; either one alone leaves a run that
// drove nothing able to publish a live count.
func TestAttestationMakesNoLiveClaimForAHeadNoTurnDrove(t *testing.T) {
	reused := reusedEvidenceFindingsJSON(t, types.TestEvidenceSourceReused, reusedDrivenHeadSHA)
	if got := attestedLiveValidationObject(t, reused, testPipelineHeadSHA); got != nil {
		t.Fatalf("a reused verdict published live_validation for a head it never drove: %v", got)
	}

	// The same scenarios, published against the head they were actually
	// driven at, still carry the full claim including the producer.
	driven := attestedLiveValidationObject(t, reused, reusedDrivenHeadSHA)
	if driven == nil {
		t.Fatal("the head the scenarios were driven at must still carry live_validation")
	}
	if driven["live"] != float64(2) || driven["total"] != float64(2) {
		t.Errorf("live_validation coverage = %v of %v, want 2 of 2", driven["live"], driven["total"])
	}
	if driven["source"] != types.TestEvidenceSourceReused {
		t.Errorf("live_validation.source = %v, want %q", driven["source"], types.TestEvidenceSourceReused)
	}

	// An agent turn is the unchanged case: it drove this head, so it claims it.
	agent := attestedLiveValidationObject(t, reusedEvidenceFindingsJSON(t, types.TestEvidenceSourceAgent, testPipelineHeadSHA), testPipelineHeadSHA)
	if agent == nil {
		t.Fatal("an agent-driven verdict must still carry live_validation for its own head")
	}
	if agent["live"] != float64(2) {
		t.Errorf("live count = %v, want 2", agent["live"])
	}
}

// TestLiveValidationLineSaysWhichRunDroveTheScenarios: the PR body's prose has
// to agree with the attestation. "2 of 2 scenarios driven live against the
// product" under a reused verdict reads as this run's work, so the sentence
// itself has to place the driving in the earlier run.
func TestLiveValidationLineSaysWhichRunDroveTheScenarios(t *testing.T) {
	scenarios := []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"},
	}
	reason := "product files unchanged since fedcba9; reused from run run-1"

	reused := renderLiveValidationLine(scenarios, types.TestVerdictGo, reason, types.TestEvidenceSourceReused)
	if !strings.Contains(reused, "not in this run") {
		t.Fatalf("reused line does not place the live driving in the earlier run: %q", reused)
	}
	if !strings.Contains(reused, "reused from run run-1") {
		t.Fatalf("reused line drops the provenance: %q", reused)
	}

	// Every other source keeps today's wording exactly, including a run
	// recorded with the gate off, which carries no source at all.
	for _, source := range []string{types.TestEvidenceSourceAgent, types.TestEvidenceSourceNoProductChange, ""} {
		line := renderLiveValidationLine(scenarios, types.TestVerdictGo, "", source)
		if !strings.Contains(line, "1 of 1 scenarios driven live against the product") {
			t.Fatalf("source %q changed the unqualified wording: %q", source, line)
		}
		if strings.Contains(line, "not in this run") {
			t.Fatalf("source %q wrongly qualified the count: %q", source, line)
		}
	}
}

// The reuse reason reaches a public PR body, and the publication redactor
// (safepath.RedactText, called from redactPRContent) only removes home
// directories while test.evidence.local_root is documented as any absolute
// path. So the reason must name the prior RUN, never a directory on the host.
func TestReuseReasonNamesTheRunNotAHostPath(t *testing.T) {
	decision := reuseDecision("run-1", Findings{
		TestedHeadSHA: reusedDrivenHeadSHA,
		Scenarios:     []types.TestScenario{{Name: "s", Result: types.ScenarioResultPass, Live: true}},
	})
	if !strings.Contains(decision.Reason, "reused from run run-1") {
		t.Fatalf("reason %q does not name the run holding the evidence", decision.Reason)
	}
	// No path separator at all is the assertion that survives a refactor: any
	// reintroduced directory, under any evidence root, contains one.
	if strings.ContainsAny(decision.Reason, `/\`) {
		t.Fatalf("reason %q publishes a host filesystem path", decision.Reason)
	}
	if decision.Reused.TestedHeadSHA != reusedDrivenHeadSHA {
		t.Fatalf("reused head = %q, want the head the scenarios were driven at", decision.Reused.TestedHeadSHA)
	}
}
