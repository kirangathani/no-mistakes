package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

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
// Two conditions remove that cost without removing evidence:
//
//  1. The run's diff (merge-base with the trusted default branch .. head, the
//     same base the rebase step establishes) touches no product file. There is
//     then no live-drivable surface by construction, so the step records the
//     existing no-surface verdict and marks it automatic. Unlike the agent's
//     own no-surface it does not park: the classification is mechanical, so
//     there is nothing for a human to decide.
//  2. An earlier run on the SAME branch recorded a go verdict at head H0 and
//     no product file has changed between H0 and this head. That verdict still
//     describes this head's product behavior, so it is recorded again with a
//     pointer to the run and evidence directory that earned it.
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

// isNonProductPath reports whether every rule in patterns classifies file as
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

// reusableBranchVerdict returns a reuse decision when the most recent go
// verdict recorded for this branch still covers this head's product files.
//
// Only that newest go verdict is considered. An older one necessarily spans a
// wider diff, so if the newest does not cover this head no earlier one can.
//
// The rules are deliberately narrow. Only a go verdict is reusable: a no-go,
// inconclusive, or no-surface conclusion is about a state a human has not
// resolved, and restating it would skip the decision rather than the cost.
// Only the same branch is consulted, because a verdict is evidence about one
// line of development. And the diff that proves nothing product-relevant moved
// is taken from the head the prior verdict actually names, so a verdict
// recorded for a head that is no longer reachable simply fails the git read
// and does not reuse.
func reusableBranchVerdict(sctx *pipeline.StepContext, nonProduct []string) (testEvidenceDecision, bool) {
	if sctx.DB == nil || sctx.Run == nil {
		return testEvidenceDecision{}, false
	}
	prior, err := sctx.DB.GetBranchTestEvidence(sctx.Run.RepoID, sctx.Run.Branch, sctx.Run.ID)
	if err != nil {
		sctx.Log(fmt.Sprintf("could not read this branch's earlier test evidence (%v); running the live-evidence agent", err))
		return testEvidenceDecision{}, false
	}
	for _, entry := range prior {
		findings, parseErr := types.ParseFindingsJSON(entry.FindingsJSON)
		if parseErr != nil || findings.Verdict != types.TestVerdictGo || findings.TestedHeadSHA == "" {
			continue
		}
		if findings.TestedHeadSHA == sctx.Run.HeadSHA && !sctx.Fixing {
			// Same head, nothing to diff.
			return reuseDecision(sctx, entry.RunID, findings), true
		}
		rangeArg := findings.TestedHeadSHA + ".." + sctx.Run.HeadSHA
		if sctx.Fixing {
			rangeArg = findings.TestedHeadSHA
		}
		product, diffErr := diffProductPaths(sctx.Ctx, sctx.WorkDir, nonProduct, rangeArg)
		if diffErr != nil {
			sctx.Log(fmt.Sprintf("could not diff against the head run %s validated (%v); running the live-evidence agent", entry.RunID, diffErr))
			return testEvidenceDecision{}, false
		}
		if len(product) > 0 {
			return testEvidenceDecision{}, false
		}
		return reuseDecision(sctx, entry.RunID, findings), true
	}
	return testEvidenceDecision{}, false
}

func reuseDecision(sctx *pipeline.StepContext, priorRunID string, prior Findings) testEvidenceDecision {
	reused := Findings{
		Scenarios: prior.Scenarios,
		Verdict:   types.TestVerdictGo,
	}
	return testEvidenceDecision{
		Source: types.TestEvidenceSourceReused,
		Reason: fmt.Sprintf(
			"product files unchanged since %s; reused from run %s (evidence: %s)",
			shortSHA(prior.TestedHeadSHA),
			priorRunID,
			priorRunEvidenceDir(sctx, priorRunID),
		),
		Reused: reused,
	}
}

// priorRunEvidenceDir names where the reused run's artifacts are on this
// machine. Every run's evidence directory is its run ID under one shared root
// (see Executor.runEvidenceDir), so the prior run's is this run's sibling.
func priorRunEvidenceDir(sctx *pipeline.StepContext, priorRunID string) string {
	dir := testEvidenceDir(sctx)
	if dir == "" {
		return priorRunID
	}
	return filepath.Join(filepath.Dir(dir), priorRunID)
}

// gatedTestOutcome builds the step outcome for a run whose live-evidence agent
// was skipped. It is the tail of TestStep.Execute minus the analyzer: the
// configured command's own result still decides the exit code and its
// findings, new test files a fix round wrote are still recorded, and the
// recorded verdict still runs through verdictFindings so a future verdict that
// must park still parks.
func gatedTestOutcome(
	sctx *pipeline.StepContext,
	gate testEvidenceDecision,
	tested []string,
	baselineFindings []Finding,
	baselineSummary string,
	baselineExitCode int,
	fixSummary string,
) (*pipeline.StepOutcome, error) {
	findings := gate.Reused
	if gate.Source == types.TestEvidenceSourceNoProductChange {
		findings.Verdict = types.TestVerdictNoSurface
	}
	findings.Tested = append([]string(nil), tested...)
	findings.TestedHeadSHA = sctx.Run.HeadSHA
	findings.EvidenceSource = gate.Source
	findings.EvidenceReason = gate.Reason
	findings.TestingSummary = gate.Reason
	findings.Summary = baselineSummary
	findings.Items = append(append([]Finding(nil), baselineFindings...), verdictFindings(findings)...)

	for _, f := range detectNewTestFiles(sctx.Ctx, sctx.WorkDir) {
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
