# foliot

`foliot` is a planned orchestrator for coding agents.
It is meant to take a software task from a person, ask up front what the request leaves open, hand the work to an existing coding agent running headless in its own git worktree, and decide "done" only from evidence the agent did not produce itself.

**Status: pre-release.**
There is no usable binary and no release yet.
`go build ./cmd/foliot` today produces a program that prints its version and nothing else.
Everything below describes the design, not working software.

## What it is designed to do

- **One log as the source of truth.** Every event is appended to one log, and the current state is computed from that log alone.
- **Headless workers.** Each task attempt runs one coding agent in its own worktree with its own `HOME`, and `foliot` reads the agent's structured event stream instead of its screen.
- **Verdicts from recorded evidence.** A task passes on a check that was captured with a count of what it examined, never on an agent's own claim, and a check that examined nothing is not a pass.
- **No model in the loop.** The only model call is at intake, where it drafts the questions a request leaves open before anything is dispatched.
- **Quiet by default.** It speaks up for an irreversible action awaiting approval, a blocker, or a milestone that needs a decision; everything else is a one-line status on demand.

[`AGENTS.md`](AGENTS.md) is the operating manual written for that design.
Every `foliot` command it names is planned and does not exist yet.

## Building

`foliot` is written in Go 1.27 with no dependencies outside the standard library.

```sh
go vet ./...
go test ./...
go build -trimpath ./cmd/foliot
```

## License

Apache License 2.0, in [`LICENSE`](LICENSE).
Third-party components are listed in [`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md); there are none yet.
