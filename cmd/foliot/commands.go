package main

import (
	"fmt"
	"io"
	"strings"
)

// The command index: every command family AGENTS.md section 8 names, in that
// order, with the function that runs it or nil when it is planned and not built.
// This slice is the one place builtness is recorded - `run` dispatches from it
// and the manual's Built column is checked against it, so a family cannot be
// documented as working before the code that works exists.
var commands = []command{
	{name: "init"},
	{name: "submit"},
	{name: "answer"},
	{name: "feedback"},
	{name: "cancel"},
	{name: "status"},
	{name: "digest"},
	{name: "log"},
	{name: "ledger"},
	{name: "report"},
	{name: "capacity"},
	{name: "profile"},
	{name: "core-check"},
	{name: "replay", run: replay},
	{name: "conformance"},
	{name: "chaos"},
	{name: "bench", run: benchFamily},
	{name: "import"},
	{name: "update"},
}

type command struct {
	name string
	run  func(args []string, getenv func(string) string, stdout, stderr io.Writer) int
}

func (c command) built() bool { return c.run != nil }

func lookup(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func builtNames() []string {
	var names []string
	for _, c := range commands {
		if c.built() {
			names = append(names, "foliot "+c.name)
		}
	}
	return names
}

// planned refuses a documented but unbuilt command by name, so a driver reading
// AGENTS.md learns the command is not here yet rather than that it misspelled one.
func planned(name string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "foliot %s: planned, not built (AGENTS.md section 8)\nbuilt today: %s (%d of %d command families)\n",
		name, strings.Join(builtNames(), ", "), len(builtNames()), len(commands))
	return 2
}

func benchFamily(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) >= 2 && args[0] == "corpus" && args[1] == "verify" {
		return corpusVerify(args[2:], getenv, stdout, stderr)
	}
	if len(args) >= 1 {
		switch args[0] {
		case "run", "estimate", "report", "probe":
			return benchCommand(args[0], args[1:], getenv, stdout, stderr)
		}
	}
	return unknown(append([]string{"bench"}, args...), stderr)
}

func unknown(args []string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "foliot: unknown command %q\n%s", strings.Join(args, " "), usage)
	return 2
}
