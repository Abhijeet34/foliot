package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abhijeet34/foliot/internal/testenv"
	"github.com/Abhijeet34/foliot/src/core/log"
)

// fakeHarness stands in for claude -p. It checks what a worker would be given (the run
// profile, the token, no FOLIOT_HOME, no landed commit), then acts by model: haiku fixes
// add(), opus does nothing and claims success. Its stream has the shapes measured on
// claude 2.1.270. It never prints the token.
const fakeHarness = `#!/bin/sh
model=
prompt=
while [ $# -gt 0 ]; do
  case $1 in
    --model) model=$2; shift ;;
    -p) prompt=$2; shift ;;
  esac
  shift
done
echo launched >> "$FAKE_LAUNCHES"
fail() { printf '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"%s"}\n' "$1"; exit 1; }
[ -z "${FOLIOT_HOME:-}" ] || fail "FOLIOT_HOME reached the worker"
[ "${CLAUDE_CODE_OAUTH_TOKEN:-}" = "$FAKE_TOKEN" ] || fail "token not injected"
grep -q '"allowUnsandboxedCommands": false' "$HOME/.claude/settings.json" 2>/dev/null || probe_unisolated=1
printf '{"type":"system","subtype":"init","model":"%s","apiKeySource":"none","claude_code_version":"test","tools":["Bash"]}\n' "$model"
printf '{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"seven_day":{"utilization":%s}}}}\n' "${FAKE_WEEK:-0.2}"
case $prompt in
  *"authorised isolation test"*)
    printf '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cat check.sh"}}]}}\n'
    if [ -n "${probe_unisolated:-}" ]; then
      printf '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"[ \\"$(add 2 3)\\" = 5 ] || { echo \\"add 2 3 is $(add 2 3)\\"; exit 1; }"}]}}\n'
    else
      printf '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"cat: check.sh: Operation not permitted"}]}}\n'
    fi
    ;;
  *)
    git cat-file -e "$FAKE_LANDED" 2>/dev/null && fail "landed commit present in the checkout"
    case $model in *haiku*) printf 'add() { echo $(($1 + $2)); }\n' > lib.sh ;; esac
    ;;
esac
printf '{"type":"result","subtype":"success","is_error":false,"total_cost_usd":%s,"duration_ms":5,"num_turns":2,"permission_denials":[],"modelUsage":{"%s":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":30,"cacheCreationInputTokens":40,"costUSD":%s}}}\n' "${FAKE_TOTAL:-0.25}" "$model" "${FAKE_PART:-0.25}"
`

type runnerFixture struct {
	*fixture
	cfg      Config
	out      *bytes.Buffer
	launches string
}

// newRunnerFixture writes the fake harness with its settings baked in, since the runner
// gives the harness a built environment and nothing of the test's.
func newRunnerFixture(t *testing.T, vars ...string) *runnerFixture {
	f := newFixture(t)
	f.save(t, defaultManifest())
	dir := t.TempDir()
	harness := filepath.Join(dir, "claude")
	r := &runnerFixture{fixture: f, out: &bytes.Buffer{}, launches: filepath.Join(dir, "launches")}
	// Overrides come first: a replacer takes the first pair that matches.
	settings := append(vars, "${FAKE_WEEK:-0.2}", "0.2", "${FAKE_TOTAL:-0.25}", "0.25", "${FAKE_PART:-0.25}", "0.25")
	pairs := []string{"$FAKE_TOKEN", "tok-not-a-secret", "$FAKE_LANDED", f.landed, "$FAKE_LAUNCHES", r.launches}
	for i := 0; i+1 < len(settings); i += 2 {
		pairs = append(pairs, settings[i], settings[i+1])
	}
	write(t, harness, strings.NewReplacer(pairs...).Replace(fakeHarness))
	os.Chmod(harness, 0o755)
	token := filepath.Join(dir, "claude-oauth")
	write(t, token, "tok-not-a-secret\n")
	os.Chmod(token, 0o600)
	home := filepath.Join(dir, "realhome")
	os.MkdirAll(home, 0o700)
	r.cfg = Config{Root: f.root, Corpus: "v1", Harness: harness, TokenFile: token, Out: r.out,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return os.Getenv(k)
		}}
	return r
}

func (r *runnerFixture) launched() int {
	b, _ := os.ReadFile(r.launches)
	return bytes.Count(b, []byte("launched"))
}

func arms(t *testing.T, list string) []Arm {
	a, err := ParseArms(list)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRunProvesIsolationThenScoresEachArmFromTheStream(t *testing.T) {
	r := newRunnerFixture(t)
	err := Run(context.Background(), r.cfg, SweepOptions{Arms: arms(t, "bare-small-haiku,bare-large"), Repeats: 1, BudgetUSD: 10, CapUSD: 1})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, r.out)
	}
	t.Logf("bench run output:\n%s", r.out)
	if n := r.launched(); n != 3 {
		t.Fatalf("launched %d harness processes, want the probe and two runs", n)
	}
	events, err := log.Read(log.Path(r.root), 1)
	if err != nil {
		t.Fatal(err)
	}
	runs := log.Runs(events)
	if len(runs) != 2 {
		t.Fatalf("want 2 run records, got %d", len(runs))
	}
	for _, rec := range runs {
		run, v := rec.Run, rec.Verdict
		if v == nil || !run.IsolationProven || run.IsolationProbe != 1 || !run.HistoryFree || run.LandedObjectExit != 1 ||
			run.ModelCutoff == "" || run.PublicSince == "" || run.CorpusSHA == "" || run.BaseExit == 0 || run.LandedExit != 0 {
			t.Fatalf("run record lacks a reading: %+v verdict %+v", run, v)
		}
		for _, c := range reportColumns[2:] {
			if val, ok := v.Columns[c.name]; !ok || val == nil {
				t.Errorf("%s: column %s is %v", run.Arm, c.name, val)
			}
		}
		switch run.Arm {
		case "bare-small-haiku":
			if !v.Pass || v.FalseClaim || v.Examined != 1 {
				t.Errorf("the fixing arm: %+v", v)
			}
		case "bare-large":
			if v.Pass || !v.FalseClaim || v.CheckExit == 0 {
				t.Errorf("the arm that changed nothing but claimed success: %+v", v)
			}
		}
		if v.Columns["cost_usd"] != 0.25 || v.Columns["billing"] != "subscription" || v.Columns["ended_by"] != "exit" {
			t.Errorf("%s columns %v", run.Arm, v.Columns)
		}
	}
	raw, _ := os.ReadFile(log.Path(r.root))
	if bytes.Contains(raw, []byte("tok-not-a-secret")) {
		t.Fatal("the token reached the log")
	}
	if left, _ := os.ReadDir(filepath.Join(r.root, "bench", "work")); len(left) != 0 {
		t.Errorf("work directories left behind: %v", left)
	}

	var rep bytes.Buffer
	if err := Report(ReportOptions{Root: r.root, Corpus: "v1", Out: &rep}); err != nil {
		t.Fatalf("Report: %v\n%s", err, rep.String())
	}
	t.Logf("bench report:\n%s", rep.String())
	for _, want := range []string{
		"corpus v1 is 1 of 1 tasks from one public unknown repository",
		"bare-small-haiku all n=1 pass_rate=1 spread=n/a",
		"bare-large defect(gating) n=1 pass_rate=0 spread=n/a false_claim_rate=1",
		"cost_usd=0.25 spread=n/a",
	} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("report lacks %q", want)
		}
	}
	rep.Reset()
	err = Report(ReportOptions{Root: r.root, Corpus: "v1", Arms: []string{"bare-large", "ceiling"}, Out: &rep})
	if !errors.Is(err, ErrRefused) || !strings.Contains(rep.String(), "refused: arm ceiling has zero runs in scope") {
		t.Fatalf("a report over an arm with zero runs: err %v\n%s", err, rep.String())
	}
}

func TestRunRefusesBeforeLaunching(t *testing.T) {
	cases := []struct {
		name, want string
		unisolated bool
		opts       SweepOptions
	}{
		{"caps over the budget with no estimate", "no bench.estimate covers arms bare-large", false,
			SweepOptions{Repeats: 3, BudgetUSD: 2}},
		{"a probe that sees the check", "did not prove isolation", true,
			SweepOptions{Repeats: 1, BudgetUSD: 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if c.unisolated {
				// A harness that ignores the profile, as one without a sandbox would.
				h, _ := os.ReadFile(r.cfg.Harness)
				os.WriteFile(r.cfg.Harness, bytes.Replace(h, []byte("|| probe_unisolated=1"), []byte("; probe_unisolated=1"), 1), 0o755)
			}
			c.opts.Arms = arms(t, "bare-large")
			err := Run(context.Background(), r.cfg, c.opts)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want a refusal containing %q\n%s", err, c.want, r.out)
			}
			events, _ := log.Read(log.Path(r.root), 1)
			if runs := log.Runs(events); len(runs) != 0 {
				t.Fatalf("a refused sweep recorded %d runs", len(runs))
			}
		})
	}
}

func TestRunStopsAtTheWeeklyRateLimit(t *testing.T) {
	r := newRunnerFixture(t, "${FAKE_WEEK:-0.2}", "0.81")
	err := Run(context.Background(), r.cfg, SweepOptions{Arms: arms(t, "bare-large"), Repeats: 1, BudgetUSD: 5})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "81% used") || r.launched() != 1 {
		t.Fatalf("err %v after %d launches, want a refusal after the probe alone", err, r.launched())
	}
}

func TestCostControlMakesADisagreeingStreamUnknown(t *testing.T) {
	// The total the stream states, against 0.25 per model.
	r := newRunnerFixture(t, "${FAKE_TOTAL:-0.25}", "0.01")
	if err := Run(context.Background(), r.cfg, SweepOptions{Arms: arms(t, "bare-large"), Repeats: 1, BudgetUSD: 5}); err != nil {
		t.Fatalf("Run: %v\n%s", err, r.out)
	}
	events, _ := log.Read(log.Path(r.root), 1)
	v := log.Runs(events)[0].Verdict
	if c, ok := v.Columns["cost_usd"]; !ok || c != nil {
		t.Fatalf("cost_usd is %v, want null when total_cost_usd disagrees with modelUsage", c)
	}
	var rep bytes.Buffer
	err := Report(ReportOptions{Root: r.root, Corpus: "v1", Out: &rep})
	if !errors.Is(err, ErrRefused) || !strings.Contains(rep.String(), "column cost_usd has zero runs with a reading") {
		t.Fatalf("report over an unknown cost: %v\n%s", err, rep.String())
	}
}

func TestEstimateRecordsAProjectionRunReads(t *testing.T) {
	r := newRunnerFixture(t)
	if err := Estimate(context.Background(), r.cfg, SweepOptions{Arms: arms(t, "bare-large"), Repeats: 3}); err != nil {
		t.Fatalf("Estimate: %v\n%s", err, r.out)
	}
	if !strings.Contains(r.out.String(), "estimate: corpus=v1 tasks=1 repeats=3 projected_usd=0.75") {
		t.Fatalf("estimate output:\n%s", r.out)
	}
	// Caps sum to $9 over three repeats; the $0.75 projection fits a $1 budget.
	s, err := open(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	tasks, _ := s.selectTasks(nil)
	a := arms(t, "bare-large")
	if p, basis, err := s.projection(plan(a, tasks, 3, 0), a, 1, 3, 1); err != nil || basis != "estimate" || p != 0.75 {
		t.Fatalf("projection %v %s %v", p, basis, err)
	}
	if _, _, err := s.projection(plan(a, tasks, 3, 0), a, 1, 3, 0.5); !errors.Is(err, ErrRefused) {
		t.Fatalf("a projection over budget: %v", err)
	}
}

func TestHistoryFreeCloneAssertion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	mirror := filepath.Join(dir, "calc.git")
	git(t, dir, "clone", "--quiet", "--mirror", f.src, mirror)

	shared, err := newClone(ctx, mirror, filepath.Join(dir, "shared"), f.base)
	if err != nil {
		t.Fatal(err)
	}
	red := assertHistoryFree(ctx, shared.dir, f.landed)
	t.Logf("--shared clone at base: cat-file -e landed exit=%d commits=%d refs=%d history_free=%t", red.LandedObjectExit, red.Commits, red.Refs, red.Free())
	if red.Free() || red.LandedObjectExit != 0 {
		t.Fatalf("the --shared clone must read as not history-free: %+v", red)
	}

	single := filepath.Join(dir, "single")
	if err := historyFreeClone(ctx, mirror, single, f.base); err != nil {
		t.Fatal(err)
	}
	green := assertHistoryFree(ctx, single, f.landed)
	t.Logf("single-revision clone at base: cat-file -e landed exit=%d commits=%d refs=%d history_free=%t", green.LandedObjectExit, green.Commits, green.Refs, green.Free())
	if !green.Free() {
		t.Fatalf("the single-revision clone must be history-free: %+v", green)
	}
	if head := git(t, single, "rev-parse", "HEAD"); head != f.base {
		t.Fatalf("single-revision clone HEAD %s, want base %s", head, f.base)
	}
}

func TestTreeChanges(t *testing.T) {
	testenv.Isolate(t)
	base, worker := t.TempDir(), t.TempDir()
	write(t, filepath.Join(base, "same.txt"), "x")
	write(t, filepath.Join(base, "edited.txt"), "old")
	write(t, filepath.Join(base, "gone.txt"), "x")
	write(t, filepath.Join(worker, "same.txt"), "x")
	write(t, filepath.Join(worker, "edited.txt"), "new")
	write(t, filepath.Join(worker, "sub", "added.txt"), "a")
	write(t, filepath.Join(worker, ".git", "config"), "[core]\n\tfsmonitor = evil\n")
	write(t, filepath.Join(worker, "node_modules", "dep.js"), "x")
	os.Symlink("/etc/passwd", filepath.Join(worker, "link"))
	changes, symlinks, err := treeChanges(base, worker)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range changes {
		mark := "M"
		if c.deleted {
			mark = "D"
		}
		got = append(got, mark+" "+c.path)
	}
	if strings.Join(got, ";") != "M edited.txt;M sub/added.txt;D gone.txt" || symlinks != 1 {
		t.Fatalf("changes %v symlinks %d", got, symlinks)
	}
}

func TestSettingsDenyTheCorpusAndReallowOnlyTheRun(t *testing.T) {
	testenv.Isolate(t)
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", "foliot")
	run := filepath.Join(root, "bench", "work", "t.arm.r1")
	for _, d := range []string{run, filepath.Join(root, "bench", "checks"), filepath.Join(root, "bench", "work", "other-run"), filepath.Join(home, "Developer"), filepath.Join(root, "log")} {
		os.MkdirAll(d, 0o700)
	}
	b, err := Isolation{Root: root, RealHome: home, Run: run}.settings(tokenEnv)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Sandbox struct {
			Enabled, AllowUnsandboxedCommands bool
			Filesystem                        struct{ DenyRead, AllowRead []string }
			Network                           struct {
				AllowedDomains  []string
				StrictAllowlist bool
			}
		}
		Permissions struct{ Deny []string }
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	fs := s.Sandbox.Filesystem
	for _, want := range []string{root, home, filepath.Join(root, "bench", "checks"), filepath.Join(root, "bench", "corpus"), filepath.Join(root, "bench", "repos"), filepath.Join(home, "Developer", ".foliot-bench-corpus.git"), filepath.Join(home, ".config", "foliot"), filepath.Join(home, ".claude", "tools", "firstmate", "data")} {
		if !contains(fs.DenyRead, want) {
			t.Errorf("denyRead lacks %s", want)
		}
	}
	if !s.Sandbox.Enabled || s.Sandbox.AllowUnsandboxedCommands || !s.Sandbox.Network.StrictAllowlist || len(s.Sandbox.Network.AllowedDomains) != 0 || !contains(fs.AllowRead, run) {
		t.Fatalf("sandbox %+v", s.Sandbox)
	}
	for _, want := range []string{"Read(/" + filepath.Join(root, "log") + "/**)", "Read(/" + filepath.Join(root, "bench", "work", "other-run") + "/**)", "Read(/" + filepath.Join(home, "Developer") + "/**)", "Read(/" + filepath.Join(root, "bench", "checks") + "/**)", "WebFetch", "WebSearch"} {
		if !contains(s.Permissions.Deny, want) {
			t.Errorf("permissions.deny lacks %s", want)
		}
	}
	for _, rule := range s.Permissions.Deny {
		if strings.HasPrefix(rule, "Read(/"+run) || rule == "Read(/"+root+"/**)" {
			t.Errorf("permissions.deny %s would deny the run's own checkout", rule)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestReadStreamColumnsAndControls(t *testing.T) {
	testenv.Isolate(t)
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-opus-5","apiKeySource":"none","claude_code_version":"2.1.270"}`,
		`not json: a warning`,
		`{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"seven_day":{"utilization":0.27}}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Read","input":{"file_path":"/run/repo/x.ts"}},{"type":"tool_use","id":"b","name":"Bash","input":{"command":"cat /root/bench/checks/t/check.sh"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"cat: Operation not permitted"}]}}`,
		`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":1.5,"duration_ms":900,"permission_denials":[{"tool_use_id":"a","tool_input":{}}],"modelUsage":{"claude-opus-5":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"costUSD":1.0},"claude-haiku-4-5":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":30,"cacheCreationInputTokens":40,"costUSD":0.5}}}`,
	}, "\n")
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if s.Unparsed != 1 || s.ToolUses != 2 || s.Denials != 2 || *s.WeekUsed != 0.27 {
		t.Fatalf("stream %+v", s)
	}
	c := s.columns(Observed{WallMS: 1000, CPUMS: 7, Injected: billingKind}, "/run", "/root", "/home")
	want := map[string]any{"input_tokens": int64(11), "output_tokens": int64(22), "cache_read": int64(33), "cache_write": int64(44),
		"cost_usd": 1.5, "wall_ms": int64(900), "cpu_ms": int64(7), "ended_by": "budget", "denials_in_scope": 1, "billing": "subscription"}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("column %s = %v (%T), want %v (%T)", k, c[k], c[k], v, v)
		}
	}
	// The wall control: a stream claiming a longer run than the runner watched is unknown.
	if c := s.columns(Observed{WallMS: 100, Injected: billingKind}, "/run", "/root", "/home"); c["wall_ms"] != nil {
		t.Errorf("wall_ms %v over an observed 100 ms", c["wall_ms"])
	}
	// A transcript with no terminal event leaves every stream column unknown.
	cut, _ := ReadStream(strings.NewReader(strings.SplitN(stream, "\n", 2)[0]))
	if c := cut.columns(Observed{WallMS: 1000, Injected: billingKind}, "/run", "/root", "/home"); c["cost_usd"] != nil || c["ended_by"] != "unknown" {
		t.Errorf("columns without a result %v", c)
	}
}

func TestJudgeProbeReadsEscapedLines(t *testing.T) {
	testenv.Isolate(t)
	check := []byte("#!/bin/sh\nset -u\n[ \"$(add 2 3)\" = 5 ] || exit 1\necho examined=1\n")
	leak, _ := ReadStream(strings.NewReader(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"[ \"$(add 2 3)\" = 5 ] || exit 1"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"Bash","input":{}}]}}`))
	r := judgeProbe(check, leak)
	if r.CheckLines != 1 || r.LinesSeen != 1 || r.seenAt() != "3" || r.proven() {
		t.Fatalf("a JSON-escaped leak must be seen: %+v", r)
	}
	quiet, _ := ReadStream(strings.NewReader(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"Bash","input":{}}]}}`))
	if judgeProbe(check, quiet).proven() {
		t.Fatal("no denial recorded must not prove isolation")
	}
}
