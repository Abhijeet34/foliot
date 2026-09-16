// Command foliot is the orchestrator's one binary. With no arguments it prints its
// version; `foliot replay --verify` is the kernel's proof that state is a pure
// function of the log (a5 section 1.7, K1 and K2), and `foliot bench corpus verify`
// certifies the benchmark corpus (a5 section 2.16, p8 R10).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Abhijeet34/foliot/src/core/bench"
	"github.com/Abhijeet34/foliot/src/core/log"
)

// version is replaced at release with -ldflags "-X main.version=<v>".
var version = "0.0.0-dev"

const usage = `usage:
  foliot                   print the version
  foliot replay --verify   verify <root>/log/events.jsonl, fold it twice and
                           print the state when both folds are byte-identical
  foliot bench corpus verify --corpus <name> [--task <id>]... [--jobs <n>]
                           certify a benchmark corpus against its six criteria
  foliot bench run --corpus <name> --arms <list> --repeats <n> --budget-usd <usd>
                           prove isolation, then run every arm over the corpus
  foliot bench estimate --corpus <name> --arms <list> --repeats <n>
                           run the first five tasks once per arm and project the sweep
  foliot bench report --corpus <name> [--arms <list>] [--task <id>]...
                           one row per arm and class with every column and its spread
  foliot bench probe --corpus <name> --arm <arm> [--task <id>] [--without-isolation]
                           ask an arm to print a hidden check and record what it saw

The families above are what is built. Every other family AGENTS.md section 8 names
is planned and not built; running one exits 2 saying so rather than calling it an
unknown command.

<root> is $FOLIOT_HOME, an absolute path. A usage error exits 2.
`

const verifyUsage = `usage: foliot bench corpus verify --corpus <name> [--task <id>]... [--jobs <n>]

Checks every task of <root>/bench/corpus/<name> against the six corpus criteria:
the hidden check <root>/bench/checks/<task>/check.sh must exit non-zero in a fresh
checkout at base_sha and at base_sha plus only the files the landed change adds, and
exit 0 at landed_sha with examined=<n> as its last line. Then the visible check must
pass at base_sha and at landed_sha over a non-zero test count, read from its output by
the corpus's visible_examined_from; these suites run one at a time, after every hidden
check, once per distinct commit and clock, and a red one runs once more only to name it
red or flaky. Every check and suite runs with node's clock pinned to the task's clock_at,
and a run whose node does not read that instant is refused. --jobs is how many tasks'
hidden checks run at once (default 2).
Prints one line per task, then examined=<n> with per-class counts.
<root> is $FOLIOT_HOME, an absolute path.

Exit: 0 every examined task passed over a non-zero count; 1 a refusal; 2 usage.
`

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "foliot %s\n", version)
	return err
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		if printVersion(stdout) != nil {
			return 1
		}
		return 0
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	switch c, named := lookup(args[0]); {
	case named && c.built():
		return c.run(args[1:], getenv, stdout, stderr)
	case named:
		return planned(c.name, stderr)
	}
	return unknown(args, stderr)
}

func replay(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	verify := fs.Bool("verify", false, "")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	} else if err != nil || fs.NArg() > 0 || !*verify {
		fmt.Fprintf(stderr, "foliot replay: takes exactly --verify\n%s", usage)
		return 2
	}
	root, err := log.Root(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "foliot replay:", err)
		return 1
	}
	path := log.Path(root)
	if err := log.Verify(path); err != nil {
		fmt.Fprintln(stderr, "foliot replay --verify:", err)
		return 1
	}
	// Each fold reads the file afresh, so a nondeterministic reader fails too.
	var states [2][]byte
	var events int
	for i := range states {
		evs, err := log.Read(path, 1)
		if err != nil {
			fmt.Fprintln(stderr, "foliot replay --verify:", err)
			return 1
		}
		events = len(evs)
		if states[i], err = json.MarshalIndent(log.Fold(evs), "", "  "); err != nil {
			fmt.Fprintln(stderr, "foliot replay --verify:", err)
			return 1
		}
	}
	if !bytes.Equal(states[0], states[1]) {
		fmt.Fprintf(stderr, "foliot replay --verify: two folds differ\n%s\n%s\n", states[0], states[1])
		return 1
	}
	fmt.Fprintf(stdout, "%s\nreplay --verify: events=%d folds=2 identical=true state_sha256=%x\n", states[0], events, sha256.Sum256(states[0]))
	return 0
}

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func corpusVerify(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("foliot bench corpus verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	corpus := fs.String("corpus", "", "")
	jobs := fs.Int("jobs", 2, "")
	var tasks repeated
	fs.Var(&tasks, "task", "")
	usageError := func(msg string) int {
		fmt.Fprintf(stderr, "foliot bench corpus verify: %s\n%s", msg, verifyUsage)
		return 2
	}
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, verifyUsage)
		return 0
	} else if err != nil {
		return usageError(err.Error())
	}
	switch {
	case fs.NArg() > 0:
		return usageError(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	case *corpus == "" || strings.ContainsAny(*corpus, `/\`) || strings.HasPrefix(*corpus, "."):
		return usageError("--corpus <name> is required and is a plain name")
	case *jobs < 1:
		return usageError("--jobs must be at least 1")
	}
	root, err := log.Root(getenv)
	if err != nil {
		return usageError(err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := bench.Verify(ctx, bench.Options{Root: root, Corpus: *corpus, Tasks: tasks, Jobs: *jobs, Out: stdout})
	if err != nil {
		fmt.Fprintln(stderr, "foliot bench corpus verify:", err)
		return 1
	}
	if !s.Success() {
		fmt.Fprintf(stderr, "foliot bench corpus verify: corpus %s refused: %d of %d tasks refused, %d corpus problems\n", *corpus, s.Refused, s.Examined, len(s.Problems))
		return 1
	}
	return 0
}
