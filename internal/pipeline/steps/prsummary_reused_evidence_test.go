package steps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

// The reuse path exists to avoid re-deriving evidence, not to publish a
// verdict with nothing behind it. So a reused verdict carries the originating
// run's artifacts, and gatedTestOutcome copies that run's evidence directory
// into this run's - the only directory publication reads - so the scenario
// table's citations still resolve.
func TestReuseCarriesTheOriginatingRunsArtifactsAndEvidenceFiles(t *testing.T) {
	prior := Findings{
		TestedHeadSHA: reusedDrivenHeadSHA,
		Scenarios:     []types.TestScenario{{Name: "s", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"}},
		Artifacts:     []types.TestArtifact{{Label: "checkout", Path: "checkout.png"}},
	}
	decision := reuseDecision("run-1", prior)
	if len(decision.Reused.Artifacts) != 1 || decision.Reused.Artifacts[0].Path != "checkout.png" {
		t.Fatalf("artifacts = %+v, want the originating run's list carried forward", decision.Reused.Artifacts)
	}
	if decision.Reused.EvidenceOriginRunID != "run-1" {
		t.Fatalf("origin run = %q, want run-1", decision.Reused.EvidenceOriginRunID)
	}

	// A reuse OF a reuse keeps naming the run that actually drove the agent,
	// because that is the only run whose directory holds anything.
	chained := reuseDecision("run-2", Findings{
		TestedHeadSHA:       reusedDrivenHeadSHA,
		EvidenceSource:      types.TestEvidenceSourceReused,
		EvidenceOriginRunID: "run-1",
		Artifacts:           prior.Artifacts,
	})
	if chained.Reused.EvidenceOriginRunID != "run-1" {
		t.Fatalf("chained origin = %q, want the originating run run-1", chained.Reused.EvidenceOriginRunID)
	}
	if !strings.Contains(chained.Reason, "reused from run run-1") {
		t.Fatalf("chained reason %q does not name the originating run", chained.Reason)
	}
}

// carryOriginEvidence is the copy itself. Its failure modes are ordinary -
// retention ages directories out - so each one has to be a clean error the
// caller can turn into "publish the verdict without the table", never a panic
// and never a silent success.
func TestCarryOriginEvidence(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "run-2")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{EvidenceDir: dest}

	if err := carryOriginEvidence(sctx, ""); err == nil {
		t.Error("an unrecorded originating run must be an error")
	}
	if err := carryOriginEvidence(sctx, "run-1"); err == nil {
		t.Error("a missing originating directory must be an error, not a silent success")
	}

	src := filepath.Join(root, "run-1")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := carryOriginEvidence(sctx, "run-1"); err == nil {
		t.Error("an empty originating directory must be an error")
	}

	if err := os.WriteFile(filepath.Join(src, "checkout.png"), []byte("\x89PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := carryOriginEvidence(sctx, "run-1"); err != nil {
		t.Fatalf("carrying a populated directory forward: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "checkout.png")); err != nil {
		t.Fatalf("evidence file did not reach this run's directory: %v", err)
	}
}

// The table and its evidence stand or fall together: it renders when the
// artifacts were carried, and is dropped when they could not be, so a PR never
// cites a file it does not carry. A freshly driven run is unaffected either
// way, including one whose scenarios cite commands rather than files.
func TestPublishedScenarioTableFollowsTheCarriedArtifacts(t *testing.T) {
	scenarios := []types.TestScenario{{Name: "checkout", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"}}

	if table := publishedScenarioTable(scenarios, types.TestEvidenceSourceReused, true, prBodyMarkdown); table == "" {
		t.Error("a reuse whose artifacts were carried must still show its scenario table")
	}
	if table := publishedScenarioTable(scenarios, types.TestEvidenceSourceReused, false, prBodyMarkdown); table != "" {
		t.Errorf("a reuse with no carried artifacts must not cite evidence it lacks:\n%s", table)
	}
	for _, source := range []string{types.TestEvidenceSourceAgent, types.TestEvidenceSourceNoProductChange, ""} {
		if table := publishedScenarioTable(scenarios, source, false, prBodyMarkdown); table == "" {
			t.Errorf("source %q must render its table regardless of artifacts", source)
		}
	}
}
