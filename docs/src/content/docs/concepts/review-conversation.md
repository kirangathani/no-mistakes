---
title: The Review Conversation
description: How the reviewer asks questions while it works, how answers reach it, and what the pipeline persists.
---

The review step used to be a monologue. One agent turn read the diff, returned
every finding at the end, and any finding it could not decide became an
`ask-user` finding that parked the run until a human answered it. The human's
answer then arrived as a gate response - approve, fix, or skip - which is a
verdict on the whole round, not an answer to the question that was asked.

The review conversation replaces the monologue with a two-way channel that runs
*while* the review runs:

- the reviewer emits each substantiated question the moment it has one, instead
  of holding it to the end of the turn;
- it keeps reviewing other areas while a question is open;
- it re-reads answers at its own checkpoints and adjusts;
- when it has reviewed everything it can and every question is emitted, the
  turn ends and the step parks, so nothing idles and no timeout burns;
- an answer wakes the *same* reviewer session to finalize, so nothing is
  re-read from scratch;
- once code changes, a fresh cold reviewer reads the result.

The independence guarantee `internal/pipeline/steps/review.go` documents is
unchanged: a reviewer session never spans a code change, so a reviewer never
certifies its own prescription.

## The file protocol

Every run owns a review-conversation directory under the run's evidence
directory (`pipeline.StepContext.EvidenceDir`, resolved once by the executor and
always outside the worktree):

```
<evidence-root>/<run-id>/review/
    questions.ndjson    # append-only, written by the reviewer
    answers.ndjson      # append-only, written by the operator
```

Both files are newline-delimited JSON. Append-only is the whole durability
story: a crash mid-write loses at most the trailing line, a reader can `tail -f`
either file, and no writer ever needs a lock on something another process is
reading. `internal/reviewqa` owns the shape and is the only parser.

### questions.ndjson

The reviewer appends one line per event, using the file tools it already has. No
MCP server is involved: an MCP server per run would be a process, a handshake
and a per-adapter support matrix for a capability the agent already has.

```json
{"id":"q1","kind":"question","question":"Should the legacy /v1 route keep answering after this change?","options":["Keep answering","Remove it","Keep behind a flag"],"weight":"major","file":"internal/api/router.go","line":88,"area":"routing","asked_at":"2026-09-15T13:04:11Z"}
{"id":"q1","kind":"retract","reason":"answered by the migration note in docs/api.md","at":"2026-09-15T13:19:02Z"}
```

- `id` is the reviewer's own stable handle for the question. A later line with
  the same `id` supersedes the earlier one, so a re-ask is an edit, not a
  duplicate.
- `kind` is `question` or `retract`. A retracted question is closed: it never
  blocks the step and never needs an answer.
- `options` carries the multiple-choice alternatives. It is required for an
  emitted question, because a question reaches the captain in the same
  multiple-choice form he already receives - an open-ended question is a worse
  question, not a shorter one.
- `weight` is `major` (escalate) - see [Routing by weight](#routing-by-weight).
  Minor questions are never emitted at all.

### answers.ndjson

```json
{"id":"q1","answer":"Keep behind a flag","answered_by":"captain","answered_at":"2026-09-15T13:31:40Z"}
```

Written by `no-mistakes axi answer`, or by any operator process appending the
same line. The last line for an `id` wins, so a correction is another append.

An answer for an unknown or retracted `id` is recorded and ignored, never an
error: the writer may be racing a retraction it has not read yet.

## Routing by weight

Unchanged from today, by the captain's ruling of 2026-09-15: the reviewer
decides minor questions itself (pass or fail) and emits only the larger ones.
"Larger" is the reviewer's judgement, stated in the prompt as the existing
`ask-user` threshold - product behaviour, deliberate author intent, access
policy, or a remedy that would extend the change. A question the reviewer can
settle from the diff, the intent, the repository instructions or a recorded
decision is not a question; it is a finding or a pass.

## State machine

```
                        ┌──────────────────────────────────────┐
                        │ reviewing                            │
      turn starts ─────▶│ - emits questions as substantiated   │
                        │ - re-reads answers.ndjson at         │
                        │   its own checkpoints                │
                        └───────────────┬──────────────────────┘
                                        │ turn ends
                        ┌───────────────┴───────────────┐
               no open question                 open question(s)
                        │                               │
                        ▼                               ▼
            ┌───────────────────┐        ┌──────────────────────────────┐
            │ findings → gate   │        │ waiting-on-answers           │
            │ (today's review   │        │ step parked, no agent alive, │
            │  gate, unchanged) │        │ review_agent_timeout not     │
            └───────────────────┘        │ running, park accounted      │
                                         └──────────────┬───────────────┘
                                                        │ every open question answered
                                                        ▼
                                         ┌──────────────────────────────┐
                                         │ finalizing                   │
                                         │ SAME reviewer session        │
                                         │ resumed with the answers     │
                                         └──────────────┬───────────────┘
                                                        │
                                       ┌────────────────┴──────────────┐
                              no open question                  new question(s)
                                       │                               │
                                       ▼                               ▼
                           findings → gate            back to waiting-on-answers
                                       │
                           ┌───────────┴────────────┐
                    human approves            findings fixed
                           │                        │
                           ▼                        ▼
                    step completes       code changes → reviewer session
                                         dropped → next pass is COLD
```

`waiting-on-answers` is deliberately the pipeline's existing approval park, not
a new durable status. Each open question is carried as an `ask-user` finding
whose category is `review-question`, which means:

- `runs.awaiting_agent_since` is stamped and `runs.parked_ms` accrues, exactly
  as documented in `AGENTS.md` under **Parked / Awaiting-Agent Signal**;
- the TUI, the IPC event stream and `axi status` already surface the park;
- `review_agent_timeout` (30 m) cannot count the wait, because there is no
  agent turn in flight: the turn ended before the park, and the finalize turn
  is a fresh invocation with a fresh deadline from `reviewAgentContext`.

The reviewer therefore never idles. It idles only in the sense the captain
required - after it has reviewed everything it can and emitted every question -
and in that state no process is alive at all, so there is nothing to time out
and nothing to poll.

### Notification is a push, not a poll

While the reviewer is *working*, it re-reads `answers.ndjson` itself at its own
checkpoints. That is not idling and not a wait: it is a file read interleaved
with work it was doing anyway, and it is what lets an early answer redirect the
pass before the effort is spent.

While the reviewer is *parked*, nothing polls. `axi answer` appends the answer
and, once no question is open, sends one `respond --action answer` to the
daemon. The executor then resumes the reviewer's own session with the answers as
its next message. From the reviewer's side an answer arrives as a message it did
not ask for - a push - and it never learns that time passed.

A live `--input-format stream-json` stdin channel to a held-open subprocess was
considered and rejected: a park lasts tens of minutes to hours, a daemon restart
would kill the held process, and the reviewer has no idle window that channel
could serve that the resume does not. The resume path already exists for the
fixer and survives a daemon restart, because the session id is persisted in
`run_agent_sessions`.

## Session lifecycle

| Turn | Session | Why |
| --- | --- | --- |
| Initial review pass | fresh `reviewer` session, persisted | the finalize turn must be able to resume it |
| Finalize after answers | resumes `reviewer` | conversational within a round; nothing re-read |
| Re-review after any code change | cold | independence: a reviewer must not certify its own prescription |
| In-run fix round (`sctx.Fixing`) | cold, and the `reviewer` session is dropped first | same reason |

`RunSessions` is keyed by `(run, role)`, so an author push that supersedes the
run starts a new run and therefore a cold reviewer with no extra work. Within a
run, the review step drops the `reviewer` session identity the moment a fix
round begins (`Forget(SessionRoleReviewer)`), so no fix round can ever be
certified by the session that prescribed it.

An answer settles only the question it answers. The finalize prompt says so
explicitly: an answer of "that is intended" closes the question it names and
gives the reviewer no licence to soften a finding it did not ask about.

## What is persisted, and where the next cold reviewer reads it

A mid-turn answer is not a gate response, so it cannot ride the existing
`step_rounds` decision channel. The review step mirrors each answered question
into `review_questions` when it finalizes: repository, branch, run, question id,
question text, options, answer, who answered, and timestamps. Nothing deletes
those rows, for the same reason nothing deletes a branch decision - an answer a
human gave about this branch keeps standing.

Every later review turn on the same branch, in any run, receives them as a
**Settled questions (do not re-raise)** prompt section, rendered separately from
the acceptance criteria and from the branch-decision section. That separation is
the point: an acceptance criterion is something the change must satisfy, while a
settled question is something the reviewer must stop asking.

`internal/db.GetBranchReviewAnswers` is the reader; the section is bounded by the
same line/byte budget as the other decision channels
(`internal/pipeline/steps/round_history.go`).

## Round history across a supersede

With the coding agent applying the fixes, a worker push supersedes the parked run
and a new run starts. The superseded run's per-round fix summaries would be lost:
`stepRoundHistorySection` is scoped to one step result, and
`uncertifiedRoundHistoryPromptSection` covers only *pipeline-authored* commits a
previous run left uncertified.

The initial review of a run therefore also receives the most recent superseded
run's review rounds on the same branch, as a **Previous run's review rounds**
section. It is labelled as author-fixed and deliberately carries no fix-round
provenance clause: the code under review is the author's, reviewed under the
ordinary standard, not pipeline-authored code needing the adversarial framing.

## No round cap

There is none, and none may be added. The captain's ruling of 2026-09-15:
"there is no round cap we have introduced here". The pipeline enforces no limit
on review fix rounds - neither user-driven nor answer-driven - and there is no
`review.max_fix_rounds` setting. A worker-side convention about filing
follow-ups after a couple of rounds is a convention; it is not a pipeline limit
and must not become one.

## The PR body

The PR body records the conversation alongside the existing decision and
deferred lists: each question asked, its answer, and who answered it. A
retracted question is listed as withdrawn. An unanswered question cannot reach
the PR body, because the step cannot complete while one is open.

## What is unchanged

- A full review pass completes before any question blocks anything. The
  reviewer does not stop at its first question.
- Test runs after review, document and lint as today.
- Attestation semantics, the tests-kept gate and the checks-green gate are
  untouched.
- The in-run fixer path still exists and still works; it is simply no longer
  the default route for review findings.
