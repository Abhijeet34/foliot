package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Abhijeet34/foliot/internal/testenv"
	"github.com/Abhijeet34/foliot/src/core/log"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

func TestPrintVersion(t *testing.T) {
	testenv.Isolate(t)
	var out bytes.Buffer
	if err := printVersion(&out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "foliot "+version+"\n"; got != want {
		t.Fatalf("printVersion wrote %q, want %q", got, want)
	}
}

func foliot(root string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	env := func(k string) string {
		if k == "FOLIOT_HOME" {
			return root
		}
		return ""
	}
	return run(args, env, &stdout, &stderr), stdout.String(), stderr.String()
}

func TestUsage(t *testing.T) {
	testenv.Isolate(t)
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{nil, 0, "foliot " + version + "\n"},
		{[]string{"--help"}, 0, usage},
		{[]string{"replay", "--help"}, 0, usage},
		{[]string{"frobnicate"}, 2, ""},
		{[]string{"replay"}, 2, ""},
		{[]string{"replay", "--verify", "extra"}, 2, ""},
		{[]string{"replay", "--verify=false"}, 2, ""},
		{[]string{"replay", "--nope"}, 2, ""},
	} {
		code, out, _ := foliot("/unused", c.args...)
		if code != c.code || out != c.out {
			t.Errorf("foliot %q exited %d printing %q; want %d printing %q", c.args, code, out, c.code, c.out)
		}
	}
}

// record writes the recorded log in testdata: a slice of a benchmark sweep with
// one line corrupted and quarantined, so replay runs over the remaining events.
func record(t *testing.T, root string) {
	t.Helper()
	at := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { at = at.Add(1500 * time.Millisecond); return at }
	l, err := log.Open(root, now)
	if err != nil {
		t.Fatal(err)
	}
	appendAll := func(l *log.Log, es ...log.Entry) {
		for _, e := range es {
			if _, err := l.Append(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	runOf := func(task, class, arm, model string, repeat int) log.Entry {
		return log.Entry{Type: "bench.run", Actor: "bench", Data: log.BenchRun{
			Corpus: "v1", Task: task, Class: class, Arm: arm, Adapter: "claude-code", Gate: "none", Repeat: repeat,
			Model: model, Provider: "anthropic", Billing: "subscription", BaseSHA: "5a9830e0", BaseExit: 1, LandedExit: 0, Benchmark: true,
		}}
	}
	verdictOf := func(task, arm string, repeat int, pass bool, cost float64) log.Entry {
		return log.Entry{Type: "bench.verdict", Actor: "bench", Evidence: []log.Evidence{{Kind: "file", Ref: "bench/checks/" + task + "/check.sh"}},
			Data: log.BenchVerdict{Corpus: "v1", Task: task, Arm: arm, Repeat: repeat, Pass: pass, Columns: map[string]any{"cost_usd": cost}}}
	}
	appendAll(l,
		runOf("defect-a", "defect", "bare-large", "claude-opus-5", 1),
		verdictOf("defect-a", "bare-large", 1, true, 1.42),
		runOf("defect-a", "defect", "bare-small", "claude-sonnet-5", 1),
		verdictOf("defect-a", "bare-small", 1, false, 0.31),
		runOf("feature-b", "feature", "bare-small", "claude-sonnet-5", 1),
		verdictOf("feature-b", "bare-small", 1, true, 0.58),
	)
	l.Close()
	data, _ := os.ReadFile(log.Path(root))
	lines := bytes.SplitAfter(data, []byte("\n"))
	copy(lines[4][2:14], "corrupted!!!") // seq 5, the bare-small verdict; line 1 is home.opened
	os.WriteFile(log.Path(root), bytes.Join(lines, nil), 0o600)
	if l, err = log.Open(root, now); err != nil {
		t.Fatal(err)
	}
	appendAll(l,
		runOf("defect-a", "defect", "bare-small", "claude-sonnet-5", 2),
		verdictOf("defect-a", "bare-small", 2, true, 0.27),
	)
	l.Close()
}

func TestRecordedLogIsWhatTheWriterWrites(t *testing.T) {
	root := testenv.Isolate(t)
	record(t, root)
	got, _ := os.ReadFile(log.Path(root))
	// home.opened and home.closed carry this process's pid, which changes every run, so
	// the fixture holds a fixed one and the writer's bytes are compared against it.
	got = regexp.MustCompile(`"pid":[0-9]+`).ReplaceAll(got, []byte(`"pid":0`))
	fixture := filepath.Join("testdata", "home", "log", "events.jsonl")
	if os.Getenv("FOLIOT_UPDATE_TESTDATA") != "" {
		os.WriteFile(fixture, got, 0o600)
	}
	want, err := os.ReadFile(fixture)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the writer produced\n%s\nthe recorded log is (err %v)\n%s", got, err, want)
	}
}

func TestReplayVerifyOnTheRecordedLog(t *testing.T) {
	testenv.Isolate(t)
	home, _ := filepath.Abs(filepath.Join("testdata", "home"))
	code, out, errOut := foliot(home, "replay", "--verify")
	golden := filepath.Join("testdata", "replay-verify.out")
	if os.Getenv("FOLIOT_UPDATE_TESTDATA") != "" {
		os.WriteFile(golden, []byte(out), 0o600)
	}
	want, _ := os.ReadFile(golden)
	if code != 0 || out != string(want) || errOut != "" {
		t.Fatalf("exit %d, stderr %q, stdout\n%s\nwant\n%s", code, errOut, out, want)
	}
}

func TestReplayVerifyRefusesAnUnverifiableLog(t *testing.T) {
	root := testenv.Isolate(t)
	record(t, root)
	data, _ := os.ReadFile(log.Path(root))
	lines := bytes.SplitAfter(data, []byte("\n"))
	os.WriteFile(log.Path(root), bytes.Join(append(lines[:1:1], lines[2:]...), nil), 0o600)
	if code, out, errOut := foliot(root, "replay", "--verify"); code != 1 || out != "" ||
		errOut != "foliot replay --verify: log seq 2: expected seq 2, found 3 after seq 1 and 0 unreadable lines\n" {
		t.Fatalf("a deleted line: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut := foliot("", "replay", "--verify"); code != 1 || !strings.Contains(errOut, "FOLIOT_HOME must be set") {
		t.Fatalf("no FOLIOT_HOME: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := foliot(t.TempDir(), "replay", "--verify"); code != 1 || !strings.Contains(errOut, "no such file") {
		t.Fatalf("a root with no log: exit %d, stderr %q", code, errOut)
	}
}

func TestCorpusVerifyExitCodes(t *testing.T) {
	testenv.Isolate(t)
	empty := t.TempDir()
	for _, c := range []struct {
		name, root string
		args       []string
		code       int
		want       string
	}{
		{"an unknown bench command is a usage error", "/unused", []string{"bench", "frobnicate"}, 2, "unknown command"},
		{"run without arms", "/unused", []string{"bench", "run", "--corpus", "v1", "--repeats", "1", "--budget-usd", "1"}, 2, "--arms <list> and --repeats"},
		{"run without a budget", "/unused", []string{"bench", "run", "--corpus", "v1", "--arms", "bare-large", "--repeats", "1"}, 2, "--budget-usd <usd> above 0"},
		{"estimate with a run-only flag", "/unused", []string{"bench", "estimate", "--corpus", "v1", "--arms", "ceiling", "--repeats", "1", "--budget-usd", "1"}, 2, "--budget-usd does not apply to bench estimate"},
		{"probe without an arm", "/unused", []string{"bench", "probe", "--corpus", "v1"}, 2, "--arm <arm> is required"},
		{"report without a corpus", "/unused", []string{"bench", "report"}, 2, "--corpus <name> is required"},
		{"report over a missing corpus", empty, []string{"bench", "report", "--corpus", "v1"}, 1, "corpus.json"},
		{"run with no benchmark profile", empty, []string{"bench", "run", "--corpus", "v1", "--arms", "bare-large", "--repeats", "1", "--budget-usd", "1"}, 1, "profile/bench.json"},
		{"run --help", "", []string{"bench", "run", "--help"}, 0, "usage: foliot bench run"},
		{"verify without --corpus", "/unused", []string{"bench", "corpus", "verify"}, 2, "--corpus <name> is required"},
		{"verify with a path for a corpus", "/unused", []string{"bench", "corpus", "verify", "--corpus", "../v1"}, 2, "plain name"},
		{"verify with a stray argument", "/unused", []string{"bench", "corpus", "verify", "--corpus", "v1", "extra"}, 2, "unexpected argument"},
		{"verify with --jobs 0", "/unused", []string{"bench", "corpus", "verify", "--corpus", "v1", "--jobs", "0"}, 2, "--jobs must be at least 1"},
		{"verify without FOLIOT_HOME", "", []string{"bench", "corpus", "verify", "--corpus", "v1"}, 2, "FOLIOT_HOME"},
		{"verify over a missing corpus", empty, []string{"bench", "corpus", "verify", "--corpus", "v1"}, 1, "corpus.json"},
		{"verify --help", "", []string{"bench", "corpus", "verify", "--help"}, 0, "usage: foliot bench corpus verify"},
	} {
		code, out, errOut := foliot(c.root, c.args...)
		if code != c.code || !strings.Contains(out+errOut, c.want) {
			t.Errorf("%s: foliot %q exited %d\nstdout: %s\nstderr: %s\nwant exit %d containing %q", c.name, c.args, code, out, errOut, c.code, c.want)
		}
	}
}
