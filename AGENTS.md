# Operating manual

This is the orchestrator's own manual, written for an agent driving it and for an agent working inside it.
The test it must pass is that an agent that has never seen the tool can operate it correctly from this document and `foliot <command> --help` alone: first run from an empty home, submit a request and answer its questions to dispatch, read status and resolve a decision, each reaching its terminal event with zero human messages and zero commands refused as malformed.
Usage errors exit 2.
Every rule here names its source: an invariant or contract assertion (`K1`..`K8`, `A1`..`A12`, `G1`..`G9`), an event type from the closed catalogue, a profile field, or a requirement `R<n>` of the product requirements; a measurement from the fleet that ran on this machine in 2026 is marked `hist:` and is history, never an instruction.
Two words are fixed: the **driver** is whoever runs `foliot` commands, the **human** is the person whose profile this home runs under and whose decisions some events wait on; how the human is addressed is a profile field asked at first run (`report.address`, R59), and "you" is used until it is answered.
Every other name in this document is a command, an event, a field, or a file.

## 1. What holds by construction

Each line is an invariant a named suite case asserts (p8 §2.6); a change to one is a change to the core and to this document in the same commit.
None of them is a rule you have to remember: each is a refusal you will meet if you try.

1. State is a fold of the log and nothing else is truth; `foliot replay --verify` yields byte-identical state twice (K1) and `seq` is gapless (K2).
2. No model in the loop: every `model.call` has `component=intake` or `component=bench` (K3); intake makes at most one bounded call per request.
3. Dispatch is an ordered chain of events sharing one `record_hash`: `intake.ready`, `router.decided`, `capacity.admitted`, `workspace.created`, `task.dispatched` (K4).
4. No steer without a cause: every `steer.sent` names the `seq` it answers (K5).
5. A verdict only from a recorded artefact, never from a worker's sentence: for `ship`, `defect` and `docs` a runner-captured check or a gate step with an examined count, or a forge reading with its control; for `scout` the report file at its declared path with its decisions inventoried. A required step unrun refuses the terminal state (K6); `examined=0` is never a pass (K7); a failed control yields `unknown`, never a value.
6. The nine verbs are the only path to commit, push, forge calls, checks, jobs, scratch removal, service records, claims and questions; the raw shape is prevented or detected per adapter and the conformance suite says which (A10).
7. One process per home, one writer, headless workers under per-run isolation: `home.lock` is held by the writer; a worker's stream is read and its screen does not exist; every worker runs under its own `HOME`, `XDG_*`, `TMPDIR` and `CODEX_HOME`; credentials reach a worker only through the adapter's injection (A8).
8. A refusal is handled with identical bytes: one re-dispatch, then route, then block; nothing rewords a request or its intent (R42).
9. The human is addressed on three triggers and the digest has a bound; everything else is status on demand (R61, R62).
10. The core has no harness name and no profile string; `foliot core-check` fails on either (K8).
11. Nothing moves under running work: the binary is static, `foliot update` is explicit and refuses while a task is live, and workers run with the auto-updater off (R75).
12. Small models are benchmark arms only; every such record is marked and no real task dispatches below the roster (R12).

## 2. Driving it

### First run

`<root>` is `$FOLIOT_HOME` and must be an absolute path; no command falls back to another location (a5 §2.20 names the variable; the absence of a default is **declared here**, `src/core/log/root.go`).
`foliot init --check --json` prints `{missing: [{field, question, default?, validation}], ok: [field]}`.
`foliot init --answer <field>=<value>` validates and writes one field.
`foliot init --check` exits 0 only when `missing` is empty, and nothing dispatches before it does (R58).
The setting questions for every registered project (`setting {domain, description, who_authorises, evidence_of_authorisation, posture}`) and `report.address` are part of `missing` (R23, R59).
A fixture answers file and a ten-line driver reach exit 0 from an empty home in the conformance suite; that driver is the worked example of this section.

### Submitting a request

`foliot submit -F <request-file>` records `intake.received {request, source: human, sha256}` (**declared here**: the requirements name the scenario "submit a request" and the event, and no design document names the command; the flag shape follows the verbs' `-F` convention).
Intake then does four things you can read back from the log: it classifies (`intake.classified {kind, deliverable, repo, base, readings[], ruled_out[]}`), it names every field the request did not supply (`intake.unknown {field, default?}`), it asks before dispatch for every unknown with no default (`question.asked {phase: before}`), and when nothing is open it writes `intake.ready` with the record hash the whole dispatch chain carries.
An unknown with a recorded default proceeds on the default and says so in the record.
A question whose subject is destructive, irreversible, security-sensitive or a merge has no default and waits (R21).
Every question carries a recommendation, and a default with an `applies_at` instant where standing authority permits (R64).
The request travels as bytes with its `sha256`, unaltered, at the head and the tail of every worker prompt, with the setting and the authorisation block beside it (R19, R23); a worker never judges a stripped request.
Complexity is an intake field from `low`, `medium`, `high`, `xhigh`, set by the intake call with its reason and changeable by a question before dispatch (R27).

### Answering a question

`foliot answer <key> -F <answer-file>` records `question.answered {key, answer, by: human}` for a pre-dispatch question, or `decision.resolved {key, answer, by, under, file}` for a decision opened during a run (**declared here** for the `-F` form; `foliot answer <key> --unnecessary` is the design's own form and records `question.answered {unnecessary: true}`, which is how question precision is measured, R24).
The `by` value is written `human` here and not the architecture's `captain`, because the same architecture forbids that string anywhere in the core (K8's fixed list); the catalogue's enum is the one place the architecture contradicts its own check, and the fix belongs in the catalogue and the fold's tests in one commit.
A decision that a profile grant covers is answered by the orchestrator itself and recorded as `question.answered {by: orchestrator, under: <grant>}` (R55); you never answer a worker's own finding for it (G7).
`foliot feedback -F <file> --target <task-id>` records `feedback.received {text, targets[]}` and re-enters intake for each target (R26, **declared here** for the command shape).
`foliot cancel <task-id> --reason <text>` records `task.cancelled {reason}` (**declared here**).

### Reading state

`foliot status` prints one line per live task and nothing else, in this exact shape (R60):

```text
<id>  <state>  next: <step>  needs: none | decision <key> | credential <name>  spent: <usd>/<budget>
```

`<state>` is one of `intake`, `ready`, `dispatched`, `running`, `verifying`, `landed`, `ended`, derived from the task's last relevant event and never written directly.
`foliot status --json` prints the full derived state (`tasks`, `workers`, `jobs`, `workspaces`, `holds`, `ledger`, `providers`, `bench`).
`foliot digest` prints at most 30 task lines of at most 200 characters, then every open question at most 400 characters with its recommendation, default and `applies_at`; the size is measured after rendering and a cut is stated in the last line with its count (R61).
`foliot log tail` is the terminal you may want and the only one that exists (R1).
`foliot ledger` prints one line per failure key with its count, rung and whether a check is owed (R40).
`foliot report real-work --since <ts>` prints one row per ended task with `outcome`, `spent_usd`, `questions_after`, `false_claims` and `rescues` (R18); `foliot report lost-work --since <ts>` prints the no-lost-work reading (a5 §2.19).

### When it speaks to you unprompted

Exactly three triggers send a message (R62): an irreversible act awaiting authority, with the full `https://` URL when a pull request is involved; a blocker after the playbooks are exhausted, including a credential; a milestone that carries a decision, such as a pull request ready when the project waits on the human, or a scout report with open decisions.
Routine progress, retries and automatic fixes never produce a message; a kernel case asserts that no `report.sent {kind: trigger}` has a `worker.turn`, `job.ended` or `recovery.ran` event as its cause.
The attention it spends on you is measured beside the cost columns: human messages per landed change, decisions per landed change, `questions_after`, question precision, the digest's rendered size (R63).

### Cost

Every task carries a budget from the profile's per-kind defaults (declared first values `ship 20`, `defect 20`, `scout 15`, `docs 10` dollars, from the $15 median of 381 priced records, replaced by the class median after twenty tasks of that kind); the harness cap is the remaining budget at each launch; dispatch refuses at spend at or above budget; overshoot is bounded to one turn (R51; `hist:` a cap of `0.05` stopped at `0.0646`).
Tokens and cost per attempt are read from the harness's terminal event, never from prose, and the cache read-to-write ratio is reported (R65, A6).
The orchestrator's own model spend is the intake call alone (R66); headless workers hold no context to keep warm, so no keep-alive turn exists (R67).

### What ends an attempt

Exactly one `loop.ended {rule}` per attempt, under one of four rules (R34): `verdict`, the kind's terminal evidence exists; `cap`, the repair rounds reach the record's cap, declared default 2 after the first attempt; `repeat`, two consecutive rounds with the same failure signature; `budget`, the record's budget is spent.
A `cap`, `repeat` or `budget` exit is a failure row, never a pass.
This is where "stop after two failed attempts at the same thing" lives now: as the `cap` and `repeat` rules rather than as a sentence (`hist:` the fleet's own version of the rule, 2026-08-04).

### Drift, and what the orchestrator does about it

Five structural signals, each with a declared threshold and a response that is a grant, a steer or a verification, never a kill (R36): `drift.boundary` (a path outside `scope.files`: granted and recorded under `scope.grants`, or a question with a deny default when it matches `scope.protected`), `drift.command` (a denied verb or raw shape: the remedy text returns to the worker), `drift.budget` (half the budget with no artefact yet: one steer asking for evidence), `drift.stall` (no event and no live job for the interval: a controlled liveness probe, one steer, then interrupt and resume), `drift.claim` (a `done` with no terminal evidence: refused, one steer naming the missing evidence).
`hist:` on 2026-09-08 and 09 four of four workers reached a file their plan did not name and all four were right to, which is why the default outside `scope.protected` is grant-and-record; on 2026-09-11 three workers claimed done with no remote branch, which is why `drift.claim` exists.

### Recovery

Recoveries run unasked when they are reversible and lose nothing; the human's standing boundary decides the rest, and a playbook that asks opens a decision with no default (R68).

| Failure | Playbook | Asks first |
| --- | --- | --- |
| worker process dies mid-attempt | `worker-dead`: resume with the session handle when one exists, else a new attempt on the same workspace; the workspace is never deleted | no |
| worker stalled twice | `worker-stalled`: interrupt, resume with a steer naming the last event | no |
| runner job dies or hits its deadline | `job-dead`: save the output, rerun once | no |
| orchestrator killed | `reconcile`: fold the log, probe every worker and job without an exit event, resume the alive ones and record the dead | no |
| torn log tail; unreadable event | `log-repair`; `quarantine` | no |
| workspace deleted under a live task | `workspace-lost`: end the attempt failed and report exactly what survives, which is everything up to the last verb commit's `attempt/<task>/<n>` tag | no |
| stale CI run | `ci-rerun`: one rerun per attempt under the recorded grant, log saved first | no under the grant, else yes |
| orphaned state rows | `orphan-sweep`: rows whose task has ended; never a workspace, never a live task's row | no |
| credential revoked or missing | none: the task is `blocked` with a trigger to the human naming the credential | yes |
| branch delete, force push, a workspace with uncommitted work, a release tag, a publish | none unasked; a decision with no default | yes |

`foliot chaos --all` runs every playbook's case on the `fake` adapter and asserts the proof event and a no-lost-work reading, so "state survives a kill at any point" is a command's output, not a claim (R47, R71).

## 3. Task kinds and the steps they declare

Kinds are a closed set; each declares its intake fields, its critical steps and its terminal evidence as profile data, and the orchestrator refuses a terminal state with a required step unrun; a skip is `step.skipped {step, by, reason, grant}` (R20, R41).

| Kind | Required before dispatch | Critical steps, in order | Terminal evidence |
| --- | --- | --- | --- |
| `ship` | `repo, base, delivery: gate \| direct-pr \| local-only, acceptance` (a visible check command), `scope.files` or `scope.discover: true`, `budget_usd, purpose` | `workspace` created; `implement`; `acceptance` ran with `examined > 0`; `gate` per delivery; `pr` opened with expected base; `checks` green; `merge` under authority or the hold that gates it | `forge.merged {sha}`, or `forge.pr_opened` plus an open decision for a delivery that waits on the human, or `workspace.tagged` for `local-only` |
| `defect` | `ship` fields plus `reproduction`, a command that fails on `base` | `reproduce-red`; then every `ship` step; `reproduce-green` | as `ship`, plus both reproduction events |
| `scout` | `question, report_path, budget_usd, purpose` | `workspace` created; `report` exists at `report_path`; `decisions` inventoried (`decision.opened` for each or an explicit none) | the report file's sha in `task.ended.evidence` |
| `docs` | `ship` fields with `acceptance` a documentation check command | as `ship` | as `ship` |

The imported queue's `idea` items and its human-held decision items (its own kind name for them is the previous tool's) exist in state and are never dispatched; a human-held item is a hold.
A profile may add a step such as `design-review` for a repository without a core change (`kinds.<name>.steps[]`), and each kind declares its edge cases the same way (`kinds.<name>.edge_cases[]`, R20).
Every step named `check` declares `examined_from`, a regular expression over the captured stdout whose first group is the count, or the command prints `examined=<n>` as its last line; a check with neither is `examined: unknown` and refused (p8 §2.9).

## 4. The readings it trusts, and their controls

Every derived reading in state carries the question it answers and a control probe with a known answer; a reading whose control fails is `unknown`, never a value (R39).
This is the whole list; a reading not on it is not one the orchestrator compares both sides of, and is named residual risk rather than assumed away (R43).

| Reading | Question it answers | Control |
| --- | --- | --- |
| process liveness by `kill -0 <pgid>` | "does the process group exist now" and nothing about progress | the orchestrator's own pid must read alive in the same call; `EPERM` proves existence, `No such process` proves death, and the reading names which |
| progress | "did the worker emit an event or did a runner job write output since seq N" | the stream file's size must have changed since the last read when an event was counted |
| forge check state | "what does GitHub report for this head sha" | the profile's `forge.control_pr`, a merged pull request the human names, must read `merged` |
| CI verdict | "did the checks run and what did they examine" | `examined > 0`; a job GitHub reports `cancelled` is `red`, never a pass |
| capacity | "what can one more worker have" | `hostinfo` and `memory_pressure -Q` both read at exit 0 in the same probe, else `unknown` |
| stall | "has anything of this task's moved" | the live-job count is read from the runner's table, not from a process listing, so short-lived process workloads are not misread as stalls |

`hist:` every row is a fleet reading that answered a different question than the one asked of it: a live pid read as progress, a memory figure ten times off, a windows CI leg green over zero tests for its whole existence, a `cancelled` job read as a pass, a `pgrep` sample that could not see a workload of thousands of tenth-of-a-second processes.

## 5. If you are a worker

You were launched headless with a prompt that carries the request bytes, the setting, what you are authorised to do without asking, the kind's steps and edge cases, and only the profile prose rows whose trigger event has fired (R78).
Your environment holds exactly the credentials the adapter injected and no other; your git identity is `worker@invalid` with no signing key, so a raw commit is unsigned and identifiable and the gate refuses it (R5, R50).
Your scratch is your attempt's `TMPDIR`; the orchestrator removes it at attempt end.

The verbs are your only path to these effects; each call is a typed message over `$FOLIOT_SOCKET` bound to your attempt by a token in your environment and judged by policy before it runs; a denial returns its remedy text to you (R49).

| Verb | What the runner does | Policy it is judged by |
| --- | --- | --- |
| `foliot-verb commit -F <message-file>` | `git commit` in your worktree under the orchestrator's git configuration, signed with the profile's key and carrying no agent trailer | message shape, no `Co-authored-by` naming an agent (`commit.forbid_trailers`), no `--no-gpg-sign` |
| `foliot-verb push` | `git push` with the orchestrator's credential to the branch the record names | branch matches the record; never the default branch; never force |
| `foliot-verb pr open --base <branch> --title-file <f> --body-file <f>` | `gh pr create` on the record's repository, registered as `forge.pr_opened` with the expected base | repository equals the record's; base equals the record's |
| `foliot-verb check run [--name <n>] -- <cmd>` | runs the check with stdout, stderr, exit code and an examined count captured to `<attempt>/checks/<n>/`, and records `check.ran` | a check named in the kind's steps; output captured, never regenerated |
| `foliot-verb job run [--deadline <s>] -- <cmd>` | starts a background job in its own process group with a deadline (declared default 1800 s) | the command is not a denied shape; the deadline is stated |
| `foliot-verb scratch rm <absolute-path>` | removes a path only under your scratch root | inside the scratch root and not a glob |
| `foliot-verb service record <pid> --stop <cmd>` | records a service you started, so cleanup can stop it | pid alive at record time, control probed |
| `foliot-verb claim <done\|blocked\|needs-decision\|disagree\|paused> [--key <slug>] -F <file>` | records a claim; `done` is checked against the kind's terminal evidence before anything follows | a `done` with no evidence yields `drift.claim`, never a terminal state |
| `foliot-verb ask -F <file>` | records `question.asked {phase: after}` and parks your attempt | counted; the target for this count is zero |

What each claim means: `done` is checked, not believed; `blocked` names something only the orchestrator or the human can clear; `needs-decision` opens a decision above you and parks you; `disagree` is first-class and answered by a steer carrying its cause, never penalised, and a ruled-out hypothesis you overturn is a ledger entry against the record rather than against you (R25); `paused` names a job you started through the `job run` verb, which is the only durable background path, because a process started outside the runner does not survive your turn (R8).
A steer reaches you between turns and is acknowledged; anything longer than 64 KiB arrives as a file path in one line (R7).
A background process outside the runner, a raw `git commit`, `git push` or `gh` call, and an `rm` with a glob or a variable are each prevented or detected per adapter, and the conformance suite records which (A10); do not spend a turn discovering that.
Nothing here asks you to remember a rule about output: a step with no evidence yields no verdict (K6, K7), so the evidence is what you produce, and a sentence is not evidence.

## 6. Where a change belongs

Ask these in order and stop at the first yes (p8 §2.7).

1. Does it change what an event means, the envelope, the catalogue, the fold, a stop rule, a verb, a control, a kind's required fields, or one of the twelve invariants above? **Core**, under `src/core/<component>`, and this document changes in the same commit.
2. Does it describe how one specific harness launches, streams, steers, resumes, denies, bills, or reports cost? **That adapter**, under `src/adapters/<name>`; A1 to A12 do not change.
3. Does it describe how one validation provider starts a run, reports steps and findings, or takes a response? **That provider**, under `src/providers/<name>`; G1 to G9 do not change.
4. Is it a value someone else might set differently: a roster entry, a price, a weight, an interval, a grant, a scope glob, a kind's extra step, a budget, the address, the term map, a corpus's contents? **Profile data**, and `foliot profile lint` must show a component that reads it.
5. Is it a tool the worker uses, or a renderer that only reads `foliot status --json` or the log and changes nothing the orchestrator does? **Outside the orchestrator**.

`foliot profile lint` prints every profile field beside the component that reads it and fails on a field no component reads; it prints every rule the profile marks `kind: prose`, and `foliot profile lint --check <path>` exits 1 when that list and the table in the named document differ in either direction (R57).
The immovable column is the core and the movable column is the profile; a rule in neither is a gap (a5 §2.21).

Every package with tests has `func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }` and every test starts with `testenv.Isolate(t)`, from `internal/testenv` (R76, a5 §2.17); `go test -v ./...` prints one `FOLIOT_TEST_WITNESS` line per real root and fails a package whose tests changed one.

## 7. Judgement the driver keeps

These are the rules the profile marks `kind: prose` for the human's own profile, and they are the only sentences in this manual that ask something of a mind rather than describing a refusal.
Each names the row of the 2026 corpus audit it came from and the evidence it carried.

- Audit your own reasoning before acting on a conclusion: stop and look for the error in what you just concluded, and treat the fact that you concluded it as evidence you are already wrong (C4; every recorded instance found one).
- Report the honest ledger unprompted: what shipped versus what merely got done, what was verified independently versus asserted; volume of activity is not progress (C5, 2026-08-09).
- Do it properly, not partially: fix the shared cause, not the symptom; when a defect has siblings, fix the class and say what else was found (C2, 2026-08-04).
- A standing permission acquired by inference is the kind nobody remembers agreeing to; one question, one word back, and a delegation covers the object it named and never extends by analogy (C24, C3; the grants are profile data precisely so that this stays true).
- An ask-user finding is a correction within accepted intent or an expansion of the product or engineering contract; a correction, however difficult, runs under the grant, and an expansion, or anything destructive, irreversible or security-sensitive, is the human's (ask-user-authority's contract-expansion test; done-archive 8030 and 10522 record decisions made under it).
- A diagnosis, a report, or an implementation-ready recommendation is evidence, never authorisation to change code (A84, no incident on record).
- Report what changed for the project rather than how the machinery got there: the project outcome, the consequence, the next decision, in the human's own nouns; the internal terms and their replacements are the profile's term map, and the address is the profile's (A147, 2026-09-02).
- Every question to the human arrives with a recommended answer, because answering bandwidth binds, not asking (C33; measured at 91 open decisions with 27 percent carrying one); the schema refuses a `question.asked` or `decision.opened` without one (R64), so this line is the reason and not the check.

## 8. Command index

Every `foliot` command the design corpus names, one line each; a command marked **declared here** is named in this document and owed by the architecture.

| Command | What it does | Source |
| --- | --- | --- |
| `foliot init --check --json`, `foliot init --answer <field>=<value>` | first run; nothing dispatches until `--check` exits 0 | R58 |
| `foliot submit -F <file>` | records a request and starts intake | **declared here** |
| `foliot answer <key> -F <file>`, `foliot answer <key> --unnecessary` | answers a question or a decision; marks one unnecessary | R24; `-F` **declared here** |
| `foliot feedback -F <file> --target <task-id>` | feedback that re-enters intake | R26; shape **declared here** |
| `foliot cancel <task-id> --reason <text>` | records `task.cancelled` | **declared here** |
| `foliot status`, `foliot status --json` | one line per live task; the derived state | R60 |
| `foliot digest` | the bounded digest with every open question | R61 |
| `foliot log tail` | the live tail of the event log | R1 |
| `foliot ledger` | failure keys with count, rung, owed | R40 |
| `foliot report real-work --since <ts>`, `foliot report lost-work --since <ts>` | real work landed; the no-lost-work reading | R18; a5 §2.19 |
| `foliot capacity --probe` | the machine's admission line | R69 |
| `foliot profile lint [--check <path>]` | every profile field beside its reader; the prose list | R57 |
| `foliot core-check` | no harness name and no profile string in the core | K8 |
| `foliot replay --verify` | fold the log twice and compare | K1 |
| `foliot conformance --kernel`, `--adapter <name>`, `--all-adapters`, `--gate <name>` | the contract suites with counts | R2, R54 |
| `foliot chaos --all` | the recovery cases on the `fake` adapter | R47 |
| `foliot bench corpus verify --corpus v1`, `foliot bench estimate`, `foliot bench run`, `foliot bench report` | the benchmark instrument | R9 to R17 |
| `foliot import --from tasks-axi <backlog.md> --verify` | import the previous queue and its holds | a5 §2.19 |
| `foliot update` | explicit update; refuses while a task is live | R75 |
