package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This file owns the Test step's diff-class gate: the decision about whether
// the live-evidence agent has to run at all for this run.
//
// The configured commands.test is NOT part of that decision. It runs on every
// run, before this gate is consulted, and this gate can never skip it. What
// the gate bounds is the evidence turn, which a pipeline audit measured at
// ~21 minutes and ~19M tokens per run - 38% of all pipeline tokens - against a
// no-go rate of roughly one round in twenty-three, re-bought in full on every
// decision-only re-run of a branch (median task: four runs).
//
// The gate is OFF unless the repository's trusted .no-mistakes.yaml sets
// test.evidence_gate: diff-class. With it unset the step invokes the agent on
// every run and records no evidence source at all, which is byte-for-byte the
// behavior before this file existed: skipping live validation changes which
// runs get validated, so it is a maintainer's decision rather than a version
// bump's. When it is on, two conditions remove that cost without removing
// evidence:
//
//  1. The run's diff (merge-base with the repository's default branch .. head,
//     the same base the Review and Document steps read; a repository that sets
//     pr.base_branch elsewhere gets a wider diff here, which only ever fails
//     open to running the agent) touches no product file. There is
//     then no live-drivable surface by construction, so the step records the
//     existing no-surface verdict and marks it automatic. Unlike the agent's
//     own no-surface it does not park: the classification is mechanical, so
//     there is nothing for a human to decide.
//  2. This branch's NEWEST recorded test verdict is a go, earned at head H0
//     under the same user intent this run carries, and no product file has
//     changed between H0 and this head. That verdict still describes this
//     head's product behavior against the same acceptance criteria, so it is
//     recorded again with a pointer to the run that earned it.
//
// A reused verdict is recorded AGAINST H0, never restamped onto this head: it
// says "the product behavior these scenarios proved has not changed", which is
// a weaker and true claim, not "these scenarios ran here". Everything that
// asks whether a particular head was live validated keys on the recorded head,
// so that distinction is what stops a run the agent never drove from
// publishing a live-validation claim.
//
// Everything else runs the agent, and so does any failure to establish either
// condition: the gate fails open to today's behavior rather than guessing.

// testEvidenceDecision is the gate's answer. Source is one of the
// types.TestEvidenceSource* constants; Reason is the one-line human account
// that reaches the step log, axi, and the PR's Testing section. Reused carries
// the prior run's verdict and scenarios when Source is
// types.TestEvidenceSourceReused.
type testEvidenceDecision struct {
	Source string
	Reason string
	Reused Findings
}

// skipsAgent reports whether this decision replaces the live-evidence turn.
//
// It is deliberately a positive test for the two skipping sources rather than
// "not agent": the zero decision means the gate is switched off, and that must
// run the agent AND record nothing, so an off repository's findings payload,
// PR body, and axi output stay identical to a build without the gate.
func (d testEvidenceDecision) skipsAgent() bool {
	return d.Source == types.TestEvidenceSourceNoProductChange ||
		d.Source == types.TestEvidenceSourceReused
}

// resolveTestEvidenceGate decides whether the live-evidence agent runs.
//
// baselineFailed is honored rather than ignored: reusing an earlier go verdict
// while this run's configured test command is red would restate a conclusion
// the run has already contradicted, so a failing baseline always drives a
// fresh evidence turn. The no-product-file conclusion is independent of the
// baseline - it is a fact about the diff - and the baseline's own error
// finding still parks the step on its own.
func resolveTestEvidenceGate(sctx *pipeline.StepContext, baseSHA string, baselineFailed bool) testEvidenceDecision {
	agentDecision := testEvidenceDecision{Source: types.TestEvidenceSourceAgent, Reason: "live-evidence agent drove scenarios in this run"}
	if sctx == nil || sctx.Config == nil {
		return agentDecision
	}
	if sctx.Config.Test.EvidenceGate != config.TestEvidenceGateDiffClass {
		// Not opted in. The zero decision runs the agent and records no
		// evidence source or reason, so nothing downstream can tell this build
		// from one without the gate.
		return testEvidenceDecision{}
	}
	nonProduct := sctx.Config.Test.NonProductPaths

	product, err := changedProductPaths(sctx, baseSHA, nonProduct)
	if err != nil {
		// Fail open: an unreadable diff must not silently skip live
		// validation, and running the agent is exactly today's behavior.
		sctx.Log(fmt.Sprintf("could not classify the changed paths (%v); running the live-evidence agent", err))
		return agentDecision
	}
	if len(product) == 0 {
		return testEvidenceDecision{
			Source: types.TestEvidenceSourceNoProductChange,
			Reason: "no product file in diff",
		}
	}
	if baselineFailed {
		return agentDecision
	}
	if reuse, ok := reusableBranchVerdict(sctx, nonProduct); ok {
		return reuse
	}
	return agentDecision
}

// changedProductPaths returns the changed paths this repository counts as
// product or UI files. In fix mode the comparison is against the working tree,
// matching how the review step reads its own changed set, because the fixer's
// edits are not committed to Run.HeadSHA yet.
func changedProductPaths(sctx *pipeline.StepContext, baseSHA string, nonProduct []string) ([]string, error) {
	rangeArg := baseSHA + ".." + sctx.Run.HeadSHA
	if sctx.Fixing {
		rangeArg = baseSHA
	}
	return diffProductPaths(sctx.Ctx, sctx.WorkDir, nonProduct, rangeArg)
}

func diffProductPaths(ctx context.Context, workDir string, nonProduct []string, rangeArg string) ([]string, error) {
	out, err := git.Run(ctx, workDir, "diff", "--name-only", "-z", "--no-renames", rangeArg)
	if err != nil {
		return nil, err
	}
	var product []string
	for _, file := range changedPathList(out) {
		if !isNonProductPath(file, nonProduct) {
			product = append(product, file)
		}
	}
	return product, nil
}

// isNonProductPath reports whether ANY rule in patterns classifies file as
// something the live-evidence agent cannot drive. An empty pattern list means
// the repository opted every path back into being product code.
func isNonProductPath(file string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchNonProductPattern(file, pattern) {
			return true
		}
	}
	return false
}

// matchNonProductPattern extends the ignore_patterns match rules with a
// leading "**/", which matches at any depth. The plain subtree form
// "testdata/**" only matches at the repository root, and nested fixture
// directories (internal/foo/testdata/...) are the common case, so the defaults
// need a way to say "wherever this directory appears". The extension is local
// to this classification on purpose: ignore_patterns and
// review.path_instructions keep their documented semantics unchanged.
func matchNonProductPattern(file, pattern string) bool {
	rest, anyDepth := strings.CutPrefix(pattern, "**/")
	if !anyDepth {
		return matchIgnorePattern(file, pattern)
	}
	segments := strings.Split(file, "/")
	for i := range segments {
		if matchIgnorePattern(strings.Join(segments[i:], "/"), rest) {
			return true
		}
	}
	return false
}

// reusableBranchVerdict returns a reuse decision when this branch's NEWEST
// recorded test verdict is a go that still covers this head's product files.
//
// Only that newest verdict is consulted, and it is never skipped past. An
// older go necessarily spans a wider diff, so if the newest go does not cover
// this head no earlier one can; and a newer no-go, inconclusive, or no-surface
// entry is this branch's own latest evidence contradicting any earlier go, so
// reusing behind it would publish a conclusion the branch has already moved
// past.
//
// The rest of the rules are deliberately narrow for the same reason. Only a go
// verdict is reusable: every other conclusion is about a state a human has not
// resolved, and restating it would skip the decision rather than the cost.
// Only the same branch is consulted, because a verdict is evidence about one
// line of development. And the diff that proves nothing product-relevant moved
// is taken from the head the prior verdict actually names, so a verdict
// recorded for a head that is no longer reachable simply fails the git read
// and does not reuse.
//
// Same-intent is the fourth narrowing condition, and it is about what the
// verdict MEANS rather than what the product does. The evidence turn derives
// its scenarios from the run's user intent, and an --intent supplied one is
// AUTHORITATIVE acceptance criteria the change must satisfy, so a verdict
// earned under intent A says nothing about intent B even at a byte-identical
// product state: republishing it would publish go for criteria no scenario
// ever exercised. sameRunIntent therefore fails open on any absence.
func reusableBranchVerdict(sctx *pipeline.StepContext, nonProduct []string) (testEvidenceDecision, bool) {
	if sctx.DB == nil || sctx.Run == nil {
		return testEvidenceDecision{}, false
	}
	prior, err := sctx.DB.GetBranchTestEvidence(sctx.Run.RepoID, sctx.Run.Branch, sctx.Run.ID)
	if err != nil {
		sctx.Log(fmt.Sprintf("could not read this branch's earlier test evidence (%v); running the live-evidence agent", err))
		return testEvidenceDecision{}, false
	}
	if prior == nil {
		return testEvidenceDecision{}, false
	}
	if !sameRunIntent(sctx.Run.Intent, prior.Intent) {
		return testEvidenceDecision{}, false
	}
	findings, parseErr := types.ParseFindingsJSON(prior.FindingsJSON)
	if parseErr != nil || findings.Verdict != types.TestVerdictGo || findings.TestedHeadSHA == "" {
		return testEvidenceDecision{}, false
	}
	if findings.TestedHeadSHA == sctx.Run.HeadSHA && !sctx.Fixing {
		// Same head, nothing to diff.
		return reuseDecision(prior.RunID, findings), true
	}
	rangeArg := findings.TestedHeadSHA + ".." + sctx.Run.HeadSHA
	if sctx.Fixing {
		rangeArg = findings.TestedHeadSHA
	}
	product, diffErr := diffProductPaths(sctx.Ctx, sctx.WorkDir, nonProduct, rangeArg)
	if diffErr != nil {
		sctx.Log(fmt.Sprintf("could not diff against the head run %s validated (%v); running the live-evidence agent", prior.RunID, diffErr))
		return testEvidenceDecision{}, false
	}
	if len(product) > 0 {
		return testEvidenceDecision{}, false
	}
	return reuseDecision(prior.RunID, findings), true
}

// sameRunIntent reports whether two runs were validated against the same
// acceptance criteria. An absent intent on EITHER side is a difference, not a
// match: "no recorded intent" is an unknown, and two unknowns are not evidence
// of being the same. Whitespace is trimmed so reflowing the same text is not
// read as a changed criterion.
func sameRunIntent(current *string, prior string) bool {
	if current == nil {
		return false
	}
	mine := strings.TrimSpace(*current)
	return mine != "" && mine == strings.TrimSpace(prior)
}

func reuseDecision(priorRunID string, prior Findings) testEvidenceDecision {
	reused := Findings{
		Scenarios: prior.Scenarios,
		Verdict:   types.TestVerdictGo,
		// The head is the one whose product behavior these scenarios were
		// actually driven against, carried forward rather than restamped onto
		// this run's head. It is what keeps the reuse honest: every consumer
		// that asks "was THIS head live validated" compares against this
		// field, so restamping would make a run that drove nothing claim a
		// live turn. See gatedTestOutcome.
		TestedHeadSHA: prior.TestedHeadSHA,
	}
	// Reuse chains: a reused verdict is itself reusable, which is the whole
	// point on a branch that re-runs several times, and only a run that
	// actually drove the agent holds artifacts. So when the predecessor is
	// itself a reuse we carry ITS reason forward, which keeps naming the
	// originating run instead of the intermediate one that holds nothing.
	provenance := fmt.Sprintf("reused from run %s", priorRunID)
	if prior.EvidenceSource == types.TestEvidenceSourceReused {
		provenance = carriedProvenance(prior.EvidenceReason, priorRunID)
	}
	return testEvidenceDecision{
		Source: types.TestEvidenceSourceReused,
		Reason: fmt.Sprintf(
			"product files unchanged since %s; %s",
			shortSHA(prior.TestedHeadSHA),
			provenance,
		),
		Reused: reused,
	}
}

// carriedProvenance extracts the originating run's pointer from a reused
// verdict's own recorded reason, so a chain keeps naming the run that actually
// drove the agent instead of the intermediate run it read the verdict from,
// which holds no artifacts. An unreadable predecessor reason degrades to
// naming the immediate predecessor, which is the most this run can vouch for.
func carriedProvenance(priorReason, priorRunID string) string {
	if _, carried, found := strings.Cut(priorReason, "; "); found && strings.HasPrefix(carried, "reused from run ") {
		return carried
	}
	return fmt.Sprintf("reused from run %s", priorRunID)
}

// gatedTestOutcome builds the step outcome for a run whose live-evidence agent
// was skipped. It is the tail of TestStep.Execute minus the analyzer: the
// configured command's own result still decides the exit code and its
// findings, new test files a fix round wrote are still recorded, and the
// recorded verdict still runs through verdictFindings so a future verdict that
// must park still parks.
//
// newTestsFromFix is what the fix turn saw before commitAgentFixes ran, and it
// cannot be recomputed here: detectNewTestFiles reads uncommitted status only,
// so by this point the fixer's own files are committed and invisible to it.
func gatedTestOutcome(
	sctx *pipeline.StepContext,
	gate testEvidenceDecision,
	tested []string,
	baselineFindings []Finding,
	baselineSummary string,
	baselineExitCode int,
	fixSummary string,
	newTestsFromFix []string,
) (*pipeline.StepOutcome, error) {
	findings := gate.Reused
	if gate.Source == types.TestEvidenceSourceNoProductChange {
		findings.Verdict = types.TestVerdictNoSurface
	}
	findings.Tested = append([]string(nil), tested...)
	// Only stamp this run's head when the decision does not already name the
	// head its evidence belongs to. A reused verdict carries the head its
	// scenarios were driven at, and that difference is load-bearing:
	// attestedLiveValidation omits live_validation entirely when the recorded
	// head is not the published one, so a run that drove nothing publishes no
	// live-validation claim rather than a restamped one. An automatic
	// no-surface DOES belong to this head - it is a fact about this diff, and
	// it carries no scenarios - so it takes the stamp.
	if findings.TestedHeadSHA == "" {
		findings.TestedHeadSHA = sctx.Run.HeadSHA
	}
	findings.EvidenceSource = gate.Source
	findings.EvidenceReason = gate.Reason
	findings.TestingSummary = gate.Reason
	findings.Summary = baselineSummary
	findings.Items = append(append([]Finding(nil), baselineFindings...), verdictFindings(findings)...)

	for _, f := range mergeNewTestFiles(newTestsFromFix, detectNewTestFiles(sctx.Ctx, sctx.WorkDir)) {
		findings.Items = append(findings.Items, Finding{
			Severity:    types.FindingSeverityInfo,
			Action:      types.ActionNoOp,
			File:        f,
			Description: fmt.Sprintf("new test file written by agent: %s", f),
		})
	}

	needsApproval := hasBlockingFindings(findings.Items)
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return nil, fmt.Errorf("marshal test findings: %w", err)
	}
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   needsApproval,
		Findings:      string(findingsJSON),
		ExitCode:      baselineExitCode,
		FixSummary:    fixSummary,
	}, nil
}
