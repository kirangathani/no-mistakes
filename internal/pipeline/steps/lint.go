package steps

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// LintStep runs linters and asks the agent to fix issues.
type LintStep struct{}

func (s *LintStep) Name() types.StepName { return types.StepLint }

func (s *LintStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	lintCmd := sctx.Config.Commands.Lint

	// The review step's lint report: the deterministic cases it recorded
	// instead of parking the run. Every path through this step carries it,
	// including the one where the configured command passes - the report
	// exists precisely for what that command does not cover.
	_, lintNotes := reviewHandoffReports(sctx)
	notesSection := handoffNotesPromptSection(lintReportPromptHeading, lintReportPromptDuty, lintNotes)

	if lintCmd == "" {
		// The combined document+lint housekeeping pass already performed the
		// agent-driven lint duty for this round; consume its result instead
		// of paying a second cold agent invocation. Fix rounds and any round
		// without a stashed result fall through to a full agent pass, so the
		// lint responsibility is never silently skipped.
		if !sctx.Fixing {
			if stash, ok := sctx.Shared.TakeHousekeepingLint(); ok {
				return lintOutcomeFromHousekeeping(sctx, stash)
			}
		}
		sctx.Log("no lint command configured, asking agent to lint and fix...")
		reassessHistory := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
		prompt := fmt.Sprintf(
			`Detect the linting and formatting tools for this project, run the relevant checks yourself, apply safe fixes, and verify the result.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Task:
- Discover the configured linters and formatters for this repository.
- Only lint or format the relevant changed files when possible.
- Apply safe formatter, linter, and static-analysis fixes yourself.
- Re-run the relevant checks after fixing.
- Report only unresolved lint, format, or static-analysis issues as structured findings.
- If everything is clean or fixed, return an empty findings array.

Rules:
- Do not run tests or broader behavioral validation.
- Focus on lint, format, and static-analysis issues only.
- Do not report issues you already fixed.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			reassessHistory+notesSection,
		)
		if sctx.PreviousFindings != "" {
			prompt += `

Previous lint findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		result, err := sctx.RunAgentContext(ctx, agent.RunOpts{
			Prompt:     fixerPrompt(prompt),
			CWD:        sctx.WorkDir,
			JSONSchema: findingsSchema,
			OnChunk:    sctx.LogChunk,
			Purpose:    "lint",
		})
		if err != nil {
			return nil, fmt.Errorf("agent lint: %w", err)
		}

		var findings Findings
		if result.Output == nil {
			return nil, errors.New("lint analyzer returned no structured findings")
		}
		if err := unmarshalRequiredFindings(result.Output, &findings, true); err != nil {
			return nil, fmt.Errorf("validate lint analyzer findings: %w", err)
		}
		summary, err := extractCommitSummary(result)
		if err != nil {
			if errors.Is(err, errRejectedCommitSummary) {
				return nil, fmt.Errorf("validate lint summary: %w", err)
			}
			sctx.Log(fmt.Sprintf("warning: could not parse lint summary: %v", err))
		}
		committed, err := commitAgentFixesWithResult(sctx, s.Name(), summary, "fix lint issues")
		if err != nil {
			return nil, err
		}

		findings.AppliedNotes = reconcileHandoffOutcomes(sctx, lintNotes, findings.AppliedNotes)
		needsApproval := hasBlockingFindings(findings.Items)
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: needsApproval,
			AutoFixable:   false,
			Findings:      string(findingsJSON),
			FixSummary:    fixResultSummary(committed),
		}, nil
	}

	// In fix mode, ask agent to fix lint issues first
	var fixSummary string
	if sctx.Fixing {
		historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
		fixPrompt := fmt.Sprintf(
			`Fix the lint issues in this repository. Run the linter, identify all issues, and fix them.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- Do not run tests or broader behavioral validation.
- Re-run the relevant lint or format commands before finishing.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			historySection+notesSection,
		)
		if sctx.PreviousFindings != "" {
			fixPrompt += `

Previous lint findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			LogMessage:      "asking agent to fix lint issues...",
			Prompt:          fixPrompt,
			ErrorPrefix:     "agent fix lint",
			FallbackSummary: "fix lint issues",
		})
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	// Run configured lint command after the run-scoped dependency preparation.
	if err := ensurePrepared(sctx, s.Name()); err != nil {
		return nil, fmt.Errorf("prepare lint dependencies: %w", err)
	}
	sctx.Log(fmt.Sprintf("running linter: %s", lintCmd))
	output, exitCode, err := runStepShellCommand(sctx, lintCmd)
	if err != nil {
		return nil, fmt.Errorf("run lint command: %w", err)
	}

	projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepLint)

	if exitCode != 0 {
		findings := Findings{
			Items: []Finding{{
				Severity:    "warning",
				Description: fmt.Sprintf("linter found issues (exit code %d)", exitCode),
			}},
			Summary: projectedOutput,
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
			ExitCode:      exitCode,
			FixSummary:    fixSummary,
		}, nil
	}

	if len(lintNotes) > 0 {
		sctx.Log(fmt.Sprintf("lint passed; acting on %d lint note(s) from the review step", len(lintNotes)))
		return s.runReportFixer(sctx, baseSHA, lintNotes, notesSection, fixSummary)
	}

	sctx.Log("lint passed")
	return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
}

// runReportFixer spends one agent pass on the review step's lint report after
// the configured lint command has already passed.
//
// That combination is the whole reason the report exists: a configured
// `commands.lint` is a fixed set of checks, and the reviewer's notes are the
// deterministic cases OUTSIDE it - a type check the command does not run, a
// checker the repository has but does not wire into lint. Reporting them as
// findings would have parked the run and bought a cold re-review; fixing them
// here costs one pass whose output nobody re-reviews.
//
// It is one pass, not a loop: the step has no round budget of its own, and a
// note the agent cannot resolve comes back as an ordinary lint finding under
// the same gate semantics as every other lint result.
func (s *LintStep) runReportFixer(sctx *pipeline.StepContext, baseSHA string, lintNotes []types.HandoffNote, notesSection, fixSummary string) (*pipeline.StepOutcome, error) {
	historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	prompt := fmt.Sprintf(
		`The review step recorded lint, type-check, and formatting observations for this change instead of blocking the run. The configured lint command already passes, so these are the deterministic issues it does not cover. Confirm and fix them.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- configured lint command (already passing): %s

Task:
- For each note, run the relevant checker yourself to confirm it, apply the safe mechanical fix, and re-run that checker.
- Report only notes you could not resolve, or that turned out to be wrong, as structured findings.
- If you fixed everything, return an empty findings array.

Rules:
- Fixes must be safe, mechanical, and behavior-preserving. Do not change functional behavior or test assertions.
- Do not run tests or broader behavioral validation, and do not re-run the whole repository lint suite.
- Do not report issues you already fixed.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Config.Commands.Lint,
		historySection+notesSection,
	)

	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{
		Prompt:     fixerPrompt(prompt),
		CWD:        sctx.WorkDir,
		JSONSchema: findingsSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "lint",
	})
	if err != nil {
		return nil, fmt.Errorf("agent lint report: %w", err)
	}

	var findings Findings
	if result.Output == nil {
		return nil, errors.New("lint analyzer returned no structured findings")
	}
	if err := unmarshalRequiredFindings(result.Output, &findings, true); err != nil {
		return nil, fmt.Errorf("validate lint analyzer findings: %w", err)
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		if errors.Is(err, errRejectedCommitSummary) {
			return nil, fmt.Errorf("validate lint summary: %w", err)
		}
		sctx.Log(fmt.Sprintf("warning: could not parse lint summary: %v", err))
	}
	committed, err := commitAgentFixesWithResult(sctx, s.Name(), summary, "fix lint report notes")
	if err != nil {
		return nil, err
	}

	findings.AppliedNotes = reconcileHandoffOutcomes(sctx, lintNotes, findings.AppliedNotes)
	findingsJSON, _ := json.Marshal(findings)
	if fixSummary == "" {
		fixSummary = fixResultSummary(committed)
	}
	return &pipeline.StepOutcome{
		NeedsApproval: hasBlockingFindings(findings.Items),
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    fixSummary,
	}, nil
}

// lintOutcomeFromHousekeeping reports the lint findings the combined
// document+lint pass produced, with the same gate semantics as the lint
// step's own agent path: blocking (error/warning) findings park for a
// decision, info findings pass through.
func lintOutcomeFromHousekeeping(sctx *pipeline.StepContext, stash pipeline.HousekeepingLintResult) (*pipeline.StepOutcome, error) {
	findings, err := types.ParseFindingsJSON(stash.FindingsJSON)
	if err != nil {
		return nil, fmt.Errorf("validate combined housekeeping lint result: %w", err)
	}
	sctx.Log(fmt.Sprintf("lint assessed in the combined document+lint housekeeping pass: %d unresolved items", len(findings.Items)))
	return &pipeline.StepOutcome{
		NeedsApproval: hasBlockingFindings(findings.Items),
		AutoFixable:   false,
		Findings:      stash.FindingsJSON,
		FixSummary:    stash.Summary,
	}, nil
}
