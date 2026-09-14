// Command foliot is the orchestrator's one binary. With no arguments it prints its
// version; `foliot replay --verify` is the kernel's proof that state is a pure
// function of the log (a5 section 1.7, K1 and K2).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Abhijeet34/foliot/src/core/log"
)

// version is replaced at release with -ldflags "-X main.version=<v>".
var version = "0.0.0-dev"

const usage = `usage:
  foliot                   print the version
  foliot replay --verify   verify <root>/log/events.jsonl, fold it twice and
                           print the state when both folds are byte-identical

<root> is $FOLIOT_HOME, an absolute path. A usage error exits 2.
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
	case "replay":
		return replay(args[1:], getenv, stdout, stderr)
	}
	fmt.Fprintf(stderr, "foliot: unknown command %q\n%s", args[0], usage)
	return 2
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
