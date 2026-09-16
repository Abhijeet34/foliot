# foliot

`foliot` is a planned orchestrator for coding agents.
It is meant to take a software task from a person, ask up front what the request leaves open, hand the work to an existing coding agent running headless in its own git worktree, and decide "done" only from evidence the agent did not produce itself.

**Status: pre-release.**
There is no usable binary and no release yet.
`go build ./cmd/foliot` today produces a program that prints its version, runs `foliot replay --verify` over an existing log, certifies the benchmark corpus with `foliot bench corpus verify`, and runs the bare benchmark arms with `foliot bench run`, `estimate`, `report` and `probe`.
Everything below describes the design, not working software.

## What it is designed to do

- **One log as the source of truth.** Every event is appended to one log, and the current state is computed from that log alone.
- **Headless workers.** Each task attempt runs one coding agent in its own worktree with its own `HOME`, and `foliot` reads the agent's structured event stream instead of its screen.
- **Verdicts from recorded evidence.** A task passes on a check that was captured with a count of what it examined, never on an agent's own claim, and a check that examined nothing is not a pass.
- **No model in the loop.** The only model call is at intake, where it drafts the questions a request leaves open before anything is dispatched.
- **Quiet by default.** It speaks up for an irreversible action awaiting approval, a blocker, or a milestone that needs a decision; everything else is a one-line status on demand.

[`AGENTS.md`](AGENTS.md) is the operating manual written for that design.
Every `foliot` command it names is planned and does not exist yet, except `foliot replay --verify` and the `foliot bench` commands.

## Verifying the benchmark corpus

The corpus lives outside this repository, under `$FOLIOT_HOME/bench/`: one `corpus/<name>/<task>/task.json` per task and one hidden check at `checks/<task>/check.sh`.
Keeping the checks out of the tree is what lets a worker be benchmarked against a test it has never seen.

```sh
FOLIOT_HOME=<root> foliot bench corpus verify --corpus v1
```

It works in two passes.
The first, `--jobs` tasks at once, exports a fresh tree at `base_sha`, where the hidden check must fail, and at `landed_sha`, where it must pass and print `examined=<n>` last.
It also runs the check on the base tree plus only the files the landed commit adds, so a check that reads a new file instead of testing the changed behaviour is refused.
The second runs each task's visible suite exactly as recorded, in a fresh clone at `base_sha` and at `landed_sha`, one suite at a time and only after every hidden check has finished, because a benchmark arm's worker runs its suite alone.
A commit two tasks share is run once, and its test count is read from the output with the corpus's `visible_examined_from` expression, so a green over zero tests is refused.
A red suite runs once more in a fresh clone, only to name it red in both runs or flaky; either way the task is refused, and a suite killed at its 30-minute deadline is refused without a second run.
Every hidden check and visible suite runs with node's `Date` and file timestamps moved together to the task's `clock_at`, each run starting at that instant, and a run whose `node -p Date.now()` does not read within 120 seconds of it is refused rather than run on the wall clock.
It ends with `examined=<n>` and a count per class, and exits 0 only when every task passed, the class counts match `corpus.json`, and the corpus is committed.

A green over zero tasks is not a pass.

## Running the benchmark

```sh
FOLIOT_HOME=<root> foliot bench estimate --corpus v1 --arms bare-large,bare-small --repeats 3
FOLIOT_HOME=<root> foliot bench run --corpus v1 --arms bare-large,bare-small --repeats 3 --budget-usd 360
FOLIOT_HOME=<root> foliot bench report --corpus v1
```

The arms, their models, training cutoffs and per-run caps, each adapter's credential file, and any extra paths a worker must not read live in `$FOLIOT_HOME/profile/bench.json`, not in the code.
Each run drives `claude -p` headless in a checkout that holds `base_sha` and no other commit, under its own `HOME` with a sandbox profile written into it.
A per-run `HOME` alone does not hide the hidden checks: it changes what the harness loads, not what its shell can read.

Before any run, `bench run` certifies every task in scope, reusing a task's certification from `bench.verified` when the corpus is committed, the record carries both visible suite readings, and the task's `task.json` and hidden check still hash to the record's `task_sha`, then runs the hidden check at `landed_sha` in the scoring environment for each task, refusing if that control does not pass: a check environment that cannot pass the landed change would score every arm red, which is a failed control and never a reading.
Only then does it ask the first arm to print a hidden check, fetch the landed patch and find the landed commit by every route it can, and refuse to start unless none of it reached the transcript and a denial was recorded.
`foliot bench probe --without-isolation` is the same probe without the profile, and it prints the check.

Every column comes from the harness's own stream: tokens, cost, wall time, how the run ended and how it was billed.
The hidden check then runs on `base_sha` plus the worker's changes, in a tree the worker never touched.
`bench run` refuses before the first run when the per-run caps sum over `--budget-usd` and no `bench estimate` projects the sweep inside it; the probe's own cost counts against the budget too, each run starts only when its cap still fits within what remains, and the sweep stops when the subscription's weekly window reads 80 percent used.
`bench report` prints one row per arm and per class with each column's spread, and refuses any row that rests on zero runs.

An isolation claim is only as good as the probe that failed to break it.

## Building

`foliot` is written in Go 1.27 with no dependencies outside the standard library.

```sh
go vet ./...
go test ./...
go build -trimpath ./cmd/foliot
```

CI runs the suite on `ubuntu-24.04`, so on a macOS workstation run the Linux lane before opening a pull request; it needs a container runtime and takes a few minutes.

```sh
sh scripts/linux-lane.sh
```

## Contributing, security and support

[`CONTRIBUTING.md`](CONTRIBUTING.md) says how to propose a change and what every pull request needs, and [`AGENTS.md`](AGENTS.md) holds this project's own rules.
Report a vulnerability privately, never in an issue: [`SECURITY.md`](SECURITY.md) gives the route and the response times.
[`SUPPORT.md`](SUPPORT.md) says where a bug report, a feature request or a question goes.
Everyone taking part is held to the [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

## License

Apache License 2.0, in [`LICENSE`](LICENSE), with the attribution in [`NOTICE`](NOTICE).
Third-party components are listed in [`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md); there are none yet.
