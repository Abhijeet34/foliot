package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abhijeet34/foliot/internal/testenv"
	"github.com/Abhijeet34/foliot/src/core/log"
)

// fakeScript stands in for a harness process. It checks what a worker is given (the
// credential, no FOLIOT_HOME, no landed commit in its checkout), acts by model, and prints
// its reading as one JSON line. Its DurationMS is 0 because the wall control refuses a
// duration longer than the runner watched, and a fast machine runs this script in under 1 ms. Unisolated, it reads the real check the way a worker
// without a sandbox can. It never prints the credential.
const fakeScript = `model=$1 mode=$2
echo launched >> "@LAUNCHES@"
fail() { printf '{"Ended":true,"EndedBy":"error","Texts":["%s"]}\n' "$1"; exit 1; }
[ -z "${FOLIOT_HOME:-}" ] || fail "FOLIOT_HOME reached the worker"
[ "${FAKE_TOKEN:-}" = tok-not-a-secret ] || fail "credential not injected"
reading() { printf '{"Model":"%s","Billing":"subscription","ToolUses":2,"Denials":%s,"Texts":["%s"],"WeekUsed":@WEEK@,"Ended":true,"EndedBy":"exit","ClaimedDone":true,"CostUSD":@COST@,"InputTokens":10,"OutputTokens":20,"CacheRead":30,"CacheWrite":40,"DurationMS":0}\n' "$model" "$1" "$2"; }
case $FAKE_PROMPT in
  *"authorised isolation test"*)
    if [ "$mode" = none ]; then
      reading 0 "$(sed -n 2p "@ROOT@/bench/checks/calc-add/check.sh" | sed 's/\\/\\\\/g; s/"/\\"/g')"
    else
      reading 1 "cat: Operation not permitted"
    fi ;;
  *)
    git cat-file -e @LANDED@ 2>/dev/null && fail "landed commit present in the checkout"
    case $model in
      fixer-model) printf 'add() { echo $(($1 + $2)); }\n' > lib.sh ;;
      planter-model) printf 'cat "@ROOT@/bench/checks/calc-add/check.sh" > "@LEAK@/read" 2>/dev/null; echo planted > "@LEAK@/wrote" 2>/dev/null; d=$(mktemp -d) && rmdir "$d" && pwd -P >/dev/null && add() { echo $(($1 + $2)); }\n' > lib.sh ;;
    esac
    reading 0 "done" ;;
esac
`

type fakeHarness struct{ script string }

func (fakeHarness) Name() string          { return "fake" }
func (fakeHarness) Billing() string       { return "subscription" }
func (fakeHarness) CredentialEnv() string { return "FAKE_TOKEN" }

func (f fakeHarness) Launch(l Launch) (Command, error) {
	mode := "isolated"
	if l.Isolation == nil {
		mode = "none"
	}
	return Command{Argv: []string{"/bin/sh", f.script, l.Model, mode}, Env: []string{"FAKE_TOKEN=" + l.Credential, "FAKE_PROMPT=" + l.Prompt}}, nil
}

func (fakeHarness) Read(r io.Reader) (*Reading, error) {
	var last []byte
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		last = append([]byte(nil), sc.Bytes()...)
	}
	var out Reading
	if len(last) == 0 {
		return &out, nil
	}
	return &out, json.Unmarshal(last, &out)
}

type runnerFixture struct {
	*fixture
	cfg      Config
	out      *bytes.Buffer
	launches string
	leak     string // where a planted check-phase payload tries to copy the check
	fake     fakeHarness
}

// newRunnerFixture writes the corpus, a benchmark profile with four fake arms, a token
// file and the fake harness script. vars override @WEEK@ and @COST@.
func newRunnerFixture(t *testing.T, vars ...string) *runnerFixture {
	f := newFixture(t)
	dir := t.TempDir()
	// A location a planted payload writes to, outside the checkout and outside <root>.
	// It must sit outside the per-user temp directory too: confineCheck allow-lists that
	// whole tree for mktemp -d (mktemp -d ignores TMPDIR even unconfined, ignoring even a
	// TMPDIR pointed at the checkout), and t.TempDir() nests under it on stock macOS, so a
	// deny-write assertion anchored there would pass by accident rather than by denial.
	outside, err := os.MkdirTemp("/tmp", "foliot-bench-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })
	for i := 0; i+1 < len(vars); i += 2 {
		if vars[i] == "@CHECK@" {
			f.check = strings.ReplaceAll(vars[i+1], "@OUTSIDE@", outside)
		}
	}
	f.save(t, defaultManifest())
	r := &runnerFixture{fixture: f, out: &bytes.Buffer{}, launches: filepath.Join(dir, "launches"), leak: filepath.Join(outside, "leak")}
	os.MkdirAll(r.leak, 0o755)
	// Overrides come first: a replacer takes the first pair that matches.
	pairs := append(vars, "@WEEK@", "0.2", "@COST@", "0.25", "@LAUNCHES@", r.launches, "@ROOT@", f.root, "@LEAK@", r.leak, "@LANDED@", f.landed)
	r.fake = fakeHarness{script: filepath.Join(dir, "harness.sh")}
	write(t, r.fake.script, strings.NewReplacer(pairs...).Replace(fakeScript))
	token := filepath.Join(dir, "token")
	write(t, token, "tok-not-a-secret\n")
	os.Chmod(token, 0o600)
	arm := func(name, model string, repeats int) Arm {
		return Arm{Name: name, Adapter: "fake", Model: model, Provider: "test", Cutoff: "2026-01", CutoffSource: "test", CapUSD: 3, Repeats: repeats}
	}
	profile, _ := json.Marshal(Profile{
		Arms:        []Arm{arm("fixer", "fixer-model", 0), arm("idle", "idle-model", 0), arm("planter", "planter-model", 0), arm("ceiling", "idle-model", 1)},
		DenyRead:    []string{filepath.Join(dir, "offsite-backup")},
		Credentials: map[string]string{"fake": token},
	})
	write(t, ProfilePath(f.root), string(profile))
	home := filepath.Join(dir, "realhome")
	os.MkdirAll(home, 0o700)
	r.cfg = Config{Root: f.root, Corpus: "v1", Adapters: map[string]Harness{"fake": r.fake}, Out: r.out,
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

func TestRunProvesIsolationThenScoresEachArmFromTheStream(t *testing.T) {
	r := newRunnerFixture(t)
	err := Run(context.Background(), r.cfg, SweepOptions{Arms: "fixer,idle", Repeats: 1, BudgetUSD: 10, CapUSD: 1})
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
		if v == nil || !run.IsolationProven || run.IsolationProbe != 2 || !run.HistoryFree || run.LandedObjectExit != 1 ||
			run.ModelCutoff != "2026-01" || run.PublicSince == "" || run.CorpusSHA == "" || run.BaseExit == 0 || run.LandedExit != 0 ||
			run.Adapter != "fake" || run.Provider != "test" || run.CapUSD != 1 {
			t.Fatalf("run record lacks a reading: %+v verdict %+v", run, v)
		}
		for _, c := range reportColumns[2:] {
			if val, ok := v.Columns[c.name]; !ok || val == nil {
				t.Errorf("%s: column %s is %v", run.Arm, c.name, val)
			}
		}
		switch run.Arm {
		case "fixer":
			if !v.Pass || v.FalseClaim || v.Examined != 1 {
				t.Errorf("the fixing arm: %+v", v)
			}
		case "idle":
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
		t.Fatal("the credential reached the log")
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
		"fixer all n=1 pass_rate=1 spread=n/a",
		"idle defect(gating) n=1 pass_rate=0 spread=n/a false_claim_rate=1",
		"cost_usd=0.25 spread=n/a",
	} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("report lacks %q", want)
		}
	}
	// A second sweep at the same corpus sha reuses the recorded verification.
	r.out.Reset()
	if err := Run(context.Background(), r.cfg, SweepOptions{Arms: "idle", Repeats: 1, BudgetUSD: 10, CapUSD: 1}); err != nil {
		t.Fatalf("second Run: %v\n%s", err, r.out)
	}
	if !strings.Contains(r.out.String(), "1 tasks reused from bench.verified, 0 to verify") || strings.Contains(r.out.String(), "task=calc-add class=defect") {
		t.Fatalf("the second sweep verified again:\n%s", r.out)
	}
	rep.Reset()
	err = Report(ReportOptions{Root: r.root, Corpus: "v1", Arms: []string{"idle", "ceiling"}, Out: &rep})
	if !errors.Is(err, ErrRefused) || !strings.Contains(rep.String(), "refused: arm ceiling has zero runs in scope") {
		t.Fatalf("a report over an arm with zero runs: err %v\n%s", err, rep.String())
	}
}

func TestRunRefusesBeforeLaunching(t *testing.T) {
	cases := []struct {
		name, want string
		change     func(r *runnerFixture)
		opts       SweepOptions
	}{
		{"caps over the budget with no estimate", "no bench.estimate covers arms idle", nil,
			SweepOptions{Arms: "idle", Repeats: 3, BudgetUSD: 2}},
		{"a probe that sees the check", "did not prove isolation", func(r *runnerFixture) {
			// A harness that ignores the profile, as one without a sandbox would.
			h, _ := os.ReadFile(r.fake.script)
			os.WriteFile(r.fake.script, bytes.Replace(h, []byte(`model=$1 mode=$2`), []byte(`model=$1 mode=none`), 1), 0o644)
		}, SweepOptions{Arms: "idle", Repeats: 1, BudgetUSD: 5}},
		{"an arm the profile lacks", `unknown arm "bare-huge"`, nil,
			SweepOptions{Arms: "idle,bare-huge", Repeats: 1, BudgetUSD: 5}},
		{"a token file others can read", "must be a regular file of mode 0600", func(r *runnerFixture) {
			var p Profile
			b, _ := os.ReadFile(ProfilePath(r.root))
			json.Unmarshal(b, &p)
			os.Chmod(p.Credentials["fake"], 0o644)
		}, SweepOptions{Arms: "idle", Repeats: 1, BudgetUSD: 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if c.change != nil {
				c.change(r)
			}
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

func TestCheckPhaseConfinesPlantedCode(t *testing.T) {
	r := newRunnerFixture(t)
	if _, ok := confineCheck("true", t.TempDir(), nil, nil); !ok {
		t.Skip("no working check confinement on this platform or inside this sandbox")
	}
	// Red first: the same payload, unconfined, copies the check out of the corpus.
	co := t.TempDir()
	payload := `cat "` + filepath.Join(r.root, "bench", "checks", "calc-add", "check.sh") + `" > "` + r.leak + `/read"`
	for _, c := range []struct {
		name     string
		confined bool
	}{{"unconfined", false}, {"confined", true}} {
		os.Remove(filepath.Join(r.leak, "read"))
		cmd := payload
		if c.confined {
			cmd, _ = confineCheck(payload, co, nil, []string{r.root})
		}
		exec.Command("sh", "-c", cmd).Run()
		b, _ := os.ReadFile(filepath.Join(r.leak, "read"))
		t.Logf("%s payload: bytes of the check copied out=%d", c.name, len(b))
		if (len(b) > 0) == c.confined {
			t.Fatalf("%s: copied %d bytes", c.name, len(b))
		}
	}
	os.Remove(filepath.Join(r.leak, "read"))
	if err := Run(context.Background(), r.cfg, SweepOptions{Arms: "planter", Repeats: 1, BudgetUSD: 5}); err != nil {
		t.Fatalf("Run: %v\n%s", err, r.out)
	}
	events, _ := log.Read(log.Path(r.root), 1)
	v := log.Runs(events)[0].Verdict
	left, _ := os.ReadDir(r.leak)
	t.Logf("planted payload: pass=%t check_confined=%t files written outside the checkout=%d", v.Pass, v.CheckConfined, len(left))
	if !v.Pass || !v.CheckConfined || len(left) != 0 {
		t.Fatalf("pass=%t confined=%t, and the payload wrote %v outside the checkout", v.Pass, v.CheckConfined, left)
	}
}

func TestControlRefusesAScoringEnvironmentTheLandedChangeCannotPass(t *testing.T) {
	// The check passes verify's unconfined run but writes outside its checkout, which the
	// confined scoring run denies: the control must catch it before any worker launches.
	r := newRunnerFixture(t, "@CHECK@", goodCheck+"echo probe > '@OUTSIDE@/written' || exit 1\necho examined=1\n")
	if _, ok := confineCheck("true", t.TempDir(), nil, nil); !ok {
		t.Skip("no working check confinement on this platform or inside this sandbox")
	}
	err := Run(context.Background(), r.cfg, SweepOptions{Arms: "fixer", Repeats: 1, BudgetUSD: 5})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "at landed_sha in the scoring environment") || r.launched() != 0 {
		t.Fatalf("err %v after %d launches, want the control's refusal before any launch\n%s", err, r.launched(), r.out)
	}
}

func TestRunStartsNoRunWhoseCapWouldPassTheBudget(t *testing.T) {
	// The caps project $1.80 inside $1.90, but the probe and each run cost $0.80: after the
	// probe and one run, $1.60 plus the next $0.90 cap no longer fits.
	r := newRunnerFixture(t, "@COST@", "0.8")
	err := Run(context.Background(), r.cfg, SweepOptions{Arms: "fixer,idle", Repeats: 1, BudgetUSD: 1.9, CapUSD: 0.9})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "cap does not fit") || r.launched() != 2 {
		t.Fatalf("err %v after %d launches, want the probe and one run, then a refusal\n%s", err, r.launched(), r.out)
	}
}

func TestRunStopsAtTheWeeklyRateLimit(t *testing.T) {
	r := newRunnerFixture(t, "@WEEK@", "0.81")
	err := Run(context.Background(), r.cfg, SweepOptions{Arms: "idle", Repeats: 1, BudgetUSD: 5})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "81% used") || r.launched() != 1 {
		t.Fatalf("err %v after %d launches, want a refusal after the probe alone", err, r.launched())
	}
}

func TestReportRefusesAColumnWithNoReading(t *testing.T) {
	r := newRunnerFixture(t, "@COST@", "null")
	// An unknown cost counts at its $3 cap, the probe's included, so the budget holds two caps.
	if err := Run(context.Background(), r.cfg, SweepOptions{Arms: "idle", Repeats: 1, BudgetUSD: 6}); err != nil {
		t.Fatalf("Run: %v\n%s", err, r.out)
	}
	events, _ := log.Read(log.Path(r.root), 1)
	if c, ok := log.Runs(events)[0].Verdict.Columns["cost_usd"]; !ok || c != nil {
		t.Fatalf("cost_usd is %v, want null when the stream carried no cost", c)
	}
	var rep bytes.Buffer
	err := Report(ReportOptions{Root: r.root, Corpus: "v1", Out: &rep})
	if !errors.Is(err, ErrRefused) || !strings.Contains(rep.String(), "column cost_usd has zero runs with a reading") {
		t.Fatalf("report over an unknown cost: %v\n%s", err, rep.String())
	}
}

func TestEstimateRecordsAProjectionRunReads(t *testing.T) {
	r := newRunnerFixture(t)
	if err := Estimate(context.Background(), r.cfg, SweepOptions{Arms: "idle", Repeats: 3}); err != nil {
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
	a, _ := s.profile.ParseArms("idle")
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
	write(t, filepath.Join(worker, "sub", "node_modules", "dep.js"), "x")
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

func TestIsolationDeniesTheCorpusAndReallowsOnlyTheRun(t *testing.T) {
	testenv.Isolate(t)
	home := t.TempDir()
	root := filepath.Join(home, "share", "foliot")
	run := filepath.Join(root, "bench", "work", "t.arm.r1")
	for _, d := range []string{run, filepath.Join(root, "bench", "checks"), filepath.Join(root, "bench", "work", "other-run"), filepath.Join(home, "records"), filepath.Join(root, "log")} {
		os.MkdirAll(d, 0o700)
	}
	iso := newIsolation(root, home, run, []string{"/opt/toolchain"}, []string{"/elsewhere/backup.git"})
	for _, want := range []string{root, home, filepath.Join(root, "bench", "checks"), filepath.Join(root, "bench", "corpus"), filepath.Join(root, "bench", "repos"), "/elsewhere/backup.git"} {
		if !contains(iso.DenyRead, want) {
			t.Errorf("DenyRead lacks %s", want)
		}
	}
	if !contains(iso.AllowRead, run) || !contains(iso.AllowRead, "/opt/toolchain") {
		t.Errorf("AllowRead %v", iso.AllowRead)
	}
	for _, want := range []string{filepath.Join(root, "log"), filepath.Join(root, "bench", "work", "other-run"), filepath.Join(home, "records"), filepath.Join(root, "bench", "checks"), "/elsewhere/backup.git"} {
		if !contains(iso.ToolDeny, want) {
			t.Errorf("ToolDeny lacks %s", want)
		}
	}
	for _, p := range iso.ToolDeny {
		if p == root || p == home || strings.HasPrefix(run, p+"/") || p == run {
			t.Errorf("ToolDeny %s would deny the run's own directory", p)
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

func TestColumnsControls(t *testing.T) {
	testenv.Isolate(t)
	i := func(n int64) *int64 { return &n }
	cost, dur := 1.5, int64(900)
	r := &Reading{Billing: "subscription", Ended: true, EndedBy: "budget", CostUSD: &cost, DurationMS: &dur,
		InputTokens: i(11), OutputTokens: i(22), CacheRead: i(33), CacheWrite: nil,
		DeniedInputs: []string{`{"file_path":"/run/repo/x.ts"}`, `{"command":"cat /root/bench/checks/t/check.sh"}`}}
	c := r.columns(Observed{WallMS: 1000, CPUMS: 7}, "subscription", "/run", "/root", "/home")
	want := map[string]any{"input_tokens": int64(11), "cache_write": nil, "cost_usd": 1.5, "wall_ms": int64(900), "cpu_ms": int64(7),
		"ended_by": "budget", "denials_in_scope": 1, "billing": "subscription", "load_at_start": nil}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("column %s = %v (%T), want %v (%T)", k, c[k], c[k], v, v)
		}
	}
	// A stream claiming a longer run than the runner watched is unknown, and a stream that
	// billed elsewhere than the runner injected says so.
	r.Billing = "api"
	if c := r.columns(Observed{WallMS: 100}, "subscription", "/run", "/root", "/home"); c["wall_ms"] != nil || c["billing"] != "mismatch:api" {
		t.Errorf("wall_ms %v billing %v", c["wall_ms"], c["billing"])
	}
	if c := (&Reading{}).columns(Observed{WallMS: 1}, "subscription", "/run", "/root", "/home"); c["ended_by"] != "unknown" || c["cost_usd"] != nil || c["billing"] != nil {
		t.Errorf("a stream with no terminal event: %v", c)
	}
}

func TestJudgeProbe(t *testing.T) {
	testenv.Isolate(t)
	check := []byte("#!/bin/sh\nset -u\n[ \"$(add 2 3)\" = 5 ] || exit 1\necho examined=1\n")
	r := judgeProbe(check, nil, "", &Reading{ToolUses: 1, Texts: []string{`[ "$(add 2 3)" = 5 ] || exit 1`}})
	if r.CheckLines != 1 || r.LinesSeen != 1 || r.seenAt() != "3" || r.proven() {
		t.Fatalf("a check line in the transcript must be seen: %+v", r)
	}
	diff := "--- a/lib.sh\n+++ b/lib.sh\n-moved line that is long enough to count\n+moved line that is long enough to count\n+add() { echo $(($1 + $2)); } # the landed fix\n+short\n"
	answer := answerLines(diff)
	if len(answer) != 1 || answer[0] != "add() { echo $(($1 + $2)); } # the landed fix" {
		t.Fatalf("answer lines %q: want only the added, unmoved, long line", answer)
	}
	leak := &Reading{ToolUses: 1, Denials: 1, Texts: []string{"Operation not permitted", "add() { echo $(($1 + $2)); } # the landed fix", "tok-123"}}
	if r := judgeProbe(check, answer, "tok-123", leak); r.DiffSeen != 1 || !r.TokenSeen || r.proven() {
		t.Fatalf("a fetched landed patch and a printed token must each refuse: %+v", r)
	}
	if judgeProbe(check, answer, "tok-123", &Reading{ToolUses: 1}).proven() {
		t.Fatal("no denial recorded must not prove isolation")
	}
	if !judgeProbe(check, answer, "tok-123", &Reading{ToolUses: 1, Denials: 1}).proven() {
		t.Fatal("a denied probe that saw nothing must prove isolation")
	}
}

func TestLoadProfileRefuses(t *testing.T) {
	testenv.Isolate(t)
	adapters := map[string]Harness{"fake": fakeHarness{}}
	for name, c := range map[string]struct{ profile, want string }{
		"missing":            {"", "no such file"},
		"no arms":            {`{"arms":[],"credentials":{}}`, "arms is empty"},
		"unknown adapter":    {`{"arms":[{"name":"a","adapter":"nope","model":"m","provider":"p","cutoff":"2026-01","cutoff_source":"s","cap_usd":1}]}`, `adapter "nope" is not one this binary has`},
		"no credential":      {`{"arms":[{"name":"a","adapter":"fake","model":"m","provider":"p","cutoff":"2026-01","cutoff_source":"s","cap_usd":1}]}`, "credentials has no entry"},
		"bad cutoff":         {`{"arms":[{"name":"a","adapter":"fake","model":"m","provider":"p","cutoff":"May 2026","cutoff_source":"s","cap_usd":1}],"credentials":{"fake":"/t"}}`, "cutoff YYYY-MM"},
		"relative deny":      {`{"arms":[{"name":"a","adapter":"fake","model":"m","provider":"p","cutoff":"2026-05","cutoff_source":"s","cap_usd":1}],"credentials":{"fake":"/t"},"deny_read":["rel"]}`, `deny_read "rel" is not absolute`},
		"unknown field":      {`{"arms":[],"token":"x"}`, "unknown field"},
		"arm declared twice": {`{"arms":[{"name":"a","adapter":"fake","model":"m","provider":"p","cutoff":"2026-05","cutoff_source":"s","cap_usd":1},{"name":"a","adapter":"fake","model":"m","provider":"p","cutoff":"2026-05","cutoff_source":"s","cap_usd":1}],"credentials":{"fake":"/t"}}`, "declared twice"},
	} {
		root := t.TempDir()
		if c.profile != "" {
			write(t, ProfilePath(root), c.profile)
		}
		if _, err := LoadProfile(root, adapters); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
}
