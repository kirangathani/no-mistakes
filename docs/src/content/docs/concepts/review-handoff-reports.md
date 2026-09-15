---
title: Review Handoff Reports
description: Why the reviewer stops parking a run on wording and lint, and how the doc and lint reports reach the steps that own those remedies.
---

A review round is expensive because of what follows it, not because of the turn
itself. A finding the reviewer reports either parks the run for a human or
starts a fix round, and any code change then buys a **fresh cold re-review** -
the independence guarantee `internal/pipeline/steps/review.go` documents, which
is deliberate and unchanged. A measured run spent two of its rounds, and three
further informational notes, on stale README/SETUP/ARCHITECTURE prose, one
overstated code comment, and a documentation list: ten to twenty minutes of
cold re-review per round, for wording.

The document and lint steps run *after* review, their own changes are never
re-reviewed, and between them they already own exactly those remedies. So the
reviewer hands them the work instead of reporting it:

- **it is not a finding** - it never parks the run and never starts a fix round;
- **it is a note in one of two reports** - the doc report or the lint report;
- the document step and the lint step **consume** their report, act on every
  note or say why not, and list which notes they applied.

## What leaves the findings list

A finding whose remedy is:

- **documentation** - a README, anything under `docs/`, a changelog, any prose
  file; a doc list that is missing an entry;
- **the wording of a comment** - a code comment, a workflow comment, a test
  NAME, or a log/summary string that merely describes what the code does;

goes to the **doc report**.

A finding the project's configured `commands.lint`, formatter, or type checker
would catch deterministically - a declared type the checker rejects, a lint
rule, formatting, an unused import - goes to the **lint report**.

## What is still a finding

The boundary is about the *remedy*, not the topic. These stay findings and park
or fix exactly as before:

- defects, including a computation that returns a wrong value without erroring;
- security and privacy findings;
- departures from the pinned intent;
- **simplifications** - the dedicated Simplification pass is unchanged. An
  applied simplification is a code change and does cost a re-review; that price
  was weighed and accepted (captain's ruling, 2026-09-15);
- **a type hint or comment that is WRONG in a way the tool accepts and that
  would mislead a caller.** A comment saying a function returns nil on error
  when it returns a zero value, or a parameter documented as optional that
  panics when empty, is a defect reported as a finding. Only *wording* -
  clumsy, stale, overstated, imprecise prose whose claim is still true - is a
  doc note.

## The reports

Both reports ride the review step's structured output, in the same durable
findings payload the risk assessment and the Test step's scenarios already use
(`types.Findings.DocReport` / `LintReport`, persisted in
`step_results.findings`). A report is a full list per round: the last round's
report wins, and there are no retraction or supersede lines to reconcile.

Each note carries an id, `file` and `line`, what is wrong, and what right looks
like:

```json
{"id":"doc1","file":"README.md","line":42,"problem":"README still says the flag defaults to false","right_looks_like":"state the new default (true) in the flag table"}
```

They surface:

- in `no-mistakes axi status`, under the review step, as `doc_report` and
  `lint_report`;
- in the review step log, and therefore in `no-mistakes axi logs --step review`;
- in the PR body's Pipeline section, beside the review conversation, together
  with which notes each step applied.

A consuming step declares `applied_notes` in its output schema **only when it
actually received notes**. That is not cosmetic: the codex adapter rewrites
every declared property as required, so a step given no report - and any agent
whose output predates the field - would be rejected for a field it has nothing
to say about. The field is never in `required` either; an agent that ignores it
still parses, and its notes are recorded as unaddressed.

## The document step consumes the doc report

The document step's scope widens from documentation to documentation **and code
and workflow comments**: the same one-owner-per-fact placement policy applies,
and a comment whose wording the reviewer flagged is now this step's to fix. The
notes are appended to its prompt, and it must act on each one or say why not.
Its structured output lists each note id with whether it was applied.

## The lint step consumes the lint report

- **Configured lint command passes, report non-empty**: the step still runs the
  fixer agent once, with the report. That is the whole point of the report -
  the deterministic-but-unconfigured cases, such as a type check the configured
  lint command does not include, get fixed rather than parked.
- **Configured lint command fails**: the report is appended to the fixer
  prompt, so one fix round addresses both.
- **No lint command configured**: the report is appended to the agent lint
  pass, including the combined document+lint housekeeping pass.

No re-review is triggered by document or lint changes. That is unchanged from
today, and it is what makes this cheaper: the work moves to the steps whose
output nobody re-reviews.

## Unchanged

The [review conversation](/no-mistakes/concepts/review-conversation/)
(questions, answers, `axi answer`), the pipeline attestation, the tests-kept
and checks-green gates, test-after-review ordering, the live-evidence gate, and
the absence of any review round cap.
