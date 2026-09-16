package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	claudecode "github.com/Abhijeet34/foliot/src/adapters/claude-code"
	"github.com/Abhijeet34/foliot/src/core/bench"
	"github.com/Abhijeet34/foliot/src/core/log"
)

var benchUsage = map[string]string{
	"run": `usage: foliot bench run --corpus <name> --arms <list> --repeats <n> --budget-usd <usd>
                        [--task <id>]... [--cap-usd <usd>] [--no-resume]

Verifies the selected tasks, runs the isolation probe with the first arm and refuses to
start unless it proves isolation, then runs every arm on every task --repeats times (the
ceiling arm once), each in a history-free checkout under a sandboxed per-run HOME. Refuses
before any run when the caps sum over --budget-usd and no bench.estimate projects the
sweep within it. Every run writes bench.run before launch and bench.verdict after the
hidden check; runs stop at the budget or at 80 percent of the weekly subscription window.

A run of the same corpus sha, task, arm, model and repeat that already carries a
bench.verdict is not planned again, so a sweep interrupted at run N restarts at run N+1 and
--budget-usd bounds this invocation alone; the plan line prints how many runs were reused.
--no-resume plans every run again.
`,
	"estimate": `usage: foliot bench estimate --corpus <name> --arms <list> --repeats <n> [--cap-usd <usd>]

Runs the first five tasks of the corpus once per arm, then records bench.estimate with
each arm's measured mean cost and the projected cost of a --repeats sweep.
`,
	"report": `usage: foliot bench report --corpus <name> [--arms <list>] [--task <id>]...

Prints the corpus's provenance, then one row per arm over every task in scope and one per
class, the defect class marked gating, with every column, its run count and its spread.
Refuses, exit 1, any arm, class or column in scope with zero runs.
`,
	"probe": `usage: foliot bench probe --corpus <name> --arm <arm> [--task <id>] [--cap-usd <usd>]
                          [--without-isolation]

Asks the arm to print the task's hidden check by every known route and records bench.probe.
Exit 0 only when isolation is proven. --without-isolation writes no run profile, which is the
red half of the proof and always exits 1.
`,
}

const benchTail = `
<root> is $FOLIOT_HOME. The arms, each arm's adapter, model, cutoff and cap, the extra paths
a worker must not read, and each adapter's credential file (mode 0600) are read from
<root>/profile/bench.json. The claude-code adapter runs $FOLIOT_CLAUDE, default claude on PATH.
Exit: 0 done; 1 a refusal; 2 usage.
`

func benchCommand(name string, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	help := benchUsage[name] + benchTail
	fs := flag.NewFlagSet("foliot bench "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	corpus := fs.String("corpus", "", "")
	arms := fs.String("arms", "", "")
	arm := fs.String("arm", "", "")
	repeats := fs.Int("repeats", 0, "")
	budget := fs.Float64("budget-usd", 0, "")
	capUSD := fs.Float64("cap-usd", 0, "")
	without := fs.Bool("without-isolation", false, "")
	noResume := fs.Bool("no-resume", false, "")
	var tasks repeated
	fs.Var(&tasks, "task", "")
	usageError := func(msg string) int {
		fmt.Fprintf(stderr, "foliot bench %s: %s\n%s", name, msg, help)
		return 2
	}
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, help)
		return 0
	} else if err != nil {
		return usageError(err.Error())
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	allowed := map[string][]string{
		"run":      {"corpus", "arms", "repeats", "budget-usd", "task", "cap-usd", "no-resume"},
		"estimate": {"corpus", "arms", "repeats", "cap-usd"},
		"report":   {"corpus", "arms", "task"},
		"probe":    {"corpus", "arm", "task", "cap-usd", "without-isolation"},
	}[name]
	for f := range set {
		if !slices.Contains(allowed, f) {
			return usageError("--" + f + " does not apply to bench " + name)
		}
	}
	switch {
	case fs.NArg() > 0:
		return usageError(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	case *corpus == "" || strings.ContainsAny(*corpus, `/\`) || strings.HasPrefix(*corpus, "."):
		return usageError("--corpus <name> is required and is a plain name")
	case (name == "run" || name == "estimate") && (*arms == "" || *repeats < 1):
		return usageError("--arms <list> and --repeats <n> of at least 1 are required")
	case name == "run" && *budget <= 0:
		return usageError("--budget-usd <usd> above 0 is required")
	case name == "probe" && *arm == "":
		return usageError("--arm <arm> is required")
	case name == "probe" && len(tasks) > 1:
		return usageError("--task is given at most once")
	case *capUSD < 0:
		return usageError("--cap-usd must not be negative")
	}
	root, err := log.Root(getenv)
	if err != nil {
		return usageError(err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if name == "report" {
		var names []string
		if *arms != "" {
			names = strings.Split(*arms, ",")
		}
		return exitFor(name, bench.Report(bench.ReportOptions{Root: root, Corpus: *corpus, Arms: names, Tasks: tasks, Out: stdout}), stderr)
	}
	binary := getenv("FOLIOT_CLAUDE")
	if binary == "" {
		binary, _ = exec.LookPath("claude") // a missing binary fails at launch, naming it
		if binary == "" {
			binary = "claude"
		}
	}
	adapters := map[string]bench.Harness{"claude-code": claudecode.Adapter{Binary: binary}}
	cfg := bench.Config{Root: root, Corpus: *corpus, Adapters: adapters, Out: stdout, Getenv: getenv}
	o := bench.SweepOptions{Arms: *arms, Tasks: tasks, Repeats: *repeats, BudgetUSD: *budget, CapUSD: *capUSD, NoResume: *noResume}
	switch name {
	case "run":
		err = bench.Run(ctx, cfg, o)
	case "estimate":
		err = bench.Estimate(ctx, cfg, o)
	case "probe":
		task := ""
		if len(tasks) == 1 {
			task = tasks[0]
		}
		err = bench.Probe(ctx, cfg, *arm, task, !*without, *capUSD)
	}
	return exitFor(name, err, stderr)
}

func exitFor(name string, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "foliot bench %s: %v\n", name, err)
	return 1
}
