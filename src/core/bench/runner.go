package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Abhijeet34/foliot/src/core/log"
)

const (
	tokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"
	// workerDeadline bounds one run's harness process; a5 section 1.1 has every process
	// the orchestrator starts carry one, and names this one nowhere. Declared here.
	workerDeadline = 60 * time.Minute
	// MaxWeekUsed is the seven-day subscription utilization at which no further run starts.
	MaxWeekUsed = 0.80
	// estimateTasks is how many tasks, in corpus order, bench estimate runs once per arm (p8 R15).
	estimateTasks = 5
)

// workerTools are the harness tools a bare arm gets: enough to read, edit and run the
// repository's suite. The web tools and subagents are left out, and the web tools are
// also denied in the run profile.
var workerTools = "Bash,Read,Edit,Write,Grep,Glob"

// Config is what every runner command shares.
type Config struct {
	Root      string
	Corpus    string
	Harness   string // path to the claude binary
	TokenFile string // the subscription token; its value is never written anywhere
	Now       func() time.Time
	Out       io.Writer
	// Getenv reads the host environment; tests replace it.
	Getenv func(string) string
}

// SweepOptions select a bench run or a bench estimate.
type SweepOptions struct {
	Arms      []Arm
	Tasks     []string // empty means the whole corpus
	Repeats   int
	BudgetUSD float64 // 0 on estimate, which is bounded by its caps
	CapUSD    float64 // 0 means each arm's own cap
}

// ErrRefused marks a refusal the command prints as its reason and exits 1 on.
var ErrRefused = errors.New("refused")

func refusef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

type session struct {
	Config
	log      *log.Log
	manifest *Manifest
	tasks    map[string]*Task
	ids      []string // every task id in the corpus, sorted
	token    string
	realHome string
	path     string   // the worker's PATH
	tool     []string // toolchain directories re-allowed inside the denial
	uid      int
	verified map[string]Result
	corpSHA  string
	repos    map[string]repoFacts
	weekUsed *float64
}

type repoFacts struct {
	public   bool
	language string
}

func open(cfg Config) (*session, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	s := &session{Config: cfg, tasks: map[string]*Task{}, verified: map[string]Result{}, repos: map[string]repoFacts{}, uid: os.Getuid()}
	corpusDir := filepath.Join(cfg.Root, "bench", "corpus", cfg.Corpus)
	m, err := LoadManifest(filepath.Join(corpusDir, "corpus.json"))
	if err != nil {
		return nil, err
	}
	s.manifest = m
	if s.ids, err = taskIDs(corpusDir); err != nil {
		return nil, err
	}
	for _, id := range s.ids {
		t, refusals := LoadTask(filepath.Join(corpusDir, id, "task.json"), m)
		if t == nil || len(refusals) > 0 {
			return nil, refusef("task %s does not load: %s", id, strings.Join(refusals, "; "))
		}
		s.tasks[id] = t
	}
	s.realHome = cfg.Getenv("HOME")
	if !filepath.IsAbs(s.realHome) {
		return nil, refusef("HOME is %q, so the run profile has no real home to deny", s.realHome)
	}
	if s.token, err = readToken(cfg.TokenFile); err != nil {
		return nil, err
	}
	s.path, s.tool = toolchain(cfg.Getenv("PATH"), s.realHome, cfg.Root)
	if s.log, err = log.Open(cfg.Root, cfg.Now); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *session) close() { s.log.Close() }

// readToken refuses a token file anyone but its owner can read, and returns the value
// only to the caller that injects it into the harness's environment.
func readToken(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", refusef("token file: %v", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return "", refusef("token file %s must be a regular file of mode 0600, is %v", path, fi.Mode())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", refusef("token file: %v", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " \n\t") {
		return "", refusef("token file %s does not hold one token", path)
	}
	return tok, nil
}

// toolchain builds the worker's PATH from the host's: an entry under the real home or the
// root is dropped, since the sandbox denies it, except the directory a version-manager
// shim resolves node to, which is kept and re-allowed. Measured 2026-09-14: node here is
// a mise shim that fails under a per-run HOME, and its real install is under the home.
func toolchain(hostPath, realHome, root string) (string, []string) {
	var dirs, allowed []string
	under := func(p, top string) bool { return p == top || strings.HasPrefix(p, top+string(filepath.Separator)) }
	for _, d := range filepath.SplitList(hostPath) {
		if d != "" && filepath.IsAbs(d) && !under(d, realHome) && !under(d, root) {
			dirs = append(dirs, d)
		}
	}
	cmd := exec.Command("node", "-p", "process.execPath")
	cmd.Env = append(os.Environ(), "PATH="+hostPath)
	if out, err := cmd.Output(); err == nil {
		if exe := strings.TrimSpace(string(out)); filepath.IsAbs(exe) && under(exe, realHome) {
			dirs = append([]string{filepath.Dir(exe)}, dirs...)
			allowed = append(allowed, filepath.Dir(filepath.Dir(exe)))
		}
	}
	return strings.Join(dirs, string(filepath.ListSeparator)), allowed
}

func (s *session) selectTasks(ids []string) ([]*Task, error) {
	if len(ids) == 0 {
		ids = s.ids
	}
	var out []*Task
	for _, id := range ids {
		t, ok := s.tasks[id]
		if !ok {
			return nil, refusef("task %q is not in corpus %s", id, s.Corpus)
		}
		out = append(out, t)
	}
	return out, nil
}

// verify runs the corpus's own certification over the tasks a sweep will use: the base
// and landed exit codes a5 section 2.16 records on bench.run come from here, at the
// corpus sha that pins them.
func (s *session) verify(ctx context.Context, tasks []*Task) error {
	var ids []string
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	fmt.Fprintf(s.Out, "verify: %d tasks of corpus %s\n", len(ids), s.Corpus)
	sum, err := Verify(ctx, Options{Root: s.Root, Corpus: s.Corpus, Tasks: ids, Jobs: 2, Out: s.Out})
	if err != nil {
		return err
	}
	if !sum.Success() {
		return refusef("corpus %s: %d of %d tasks refused by verify, %d corpus problems", s.Corpus, sum.Refused, sum.Examined, len(sum.Problems))
	}
	s.corpSHA = sum.CorpusSHA
	for _, r := range sum.Results {
		s.verified[r.ID] = r
	}
	return nil
}

type plannedRun struct {
	arm    Arm
	task   *Task
	repeat int
	cap    float64
}

func plan(arms []Arm, tasks []*Task, repeats int, capUSD float64) []plannedRun {
	var runs []plannedRun
	for _, a := range arms {
		c := a.CapUSD
		if capUSD > 0 {
			c = capUSD
		}
		for _, t := range tasks {
			for r := 1; r <= a.repeats(repeats); r++ {
				runs = append(runs, plannedRun{a, t, r, c})
			}
		}
	}
	return runs
}

// projection decides whether a sweep may start (p8 R15). When every run's cap fits the
// budget, the caps bound the spend and nothing needs measuring. Otherwise the newest
// bench.estimate covering every arm projects it, and without one the sweep refuses.
func (s *session) projection(runs []plannedRun, arms []Arm, tasks int, repeats int, budget float64) (float64, string, error) {
	var caps float64
	for _, r := range runs {
		caps += r.cap
	}
	if caps <= budget {
		return caps, "caps", nil
	}
	events, err := log.Read(log.Path(s.Root), 1)
	if err != nil {
		return 0, "", err
	}
	ests := log.Estimates(events)
	for i := len(ests) - 1; i >= 0; i-- {
		est := ests[i]
		if est.Corpus != s.Corpus {
			continue
		}
		var projected float64
		covered := true
		for _, a := range arms {
			mean, ok := est.MeanCostUSD[a.Name]
			if !ok {
				covered = false
				break
			}
			projected += mean * float64(tasks) * float64(a.repeats(repeats))
		}
		if !covered {
			continue
		}
		if projected > budget {
			return projected, "estimate", refusef("projected sweep cost $%.2f exceeds --budget-usd %.2f (from the bench.estimate over %s)", projected, budget, strings.Join(est.Tasks, ","))
		}
		return projected, "estimate", nil
	}
	return caps, "caps", refusef("the caps sum to $%.2f over --budget-usd %.2f and no bench.estimate covers arms %s; run foliot bench estimate first", caps, budget, armList(arms))
}

func armList(arms []Arm) string {
	var names []string
	for _, a := range arms {
		names = append(names, a.Name)
	}
	return strings.Join(names, ",")
}

// Run is foliot bench run.
func Run(ctx context.Context, cfg Config, o SweepOptions) error {
	s, err := open(cfg)
	if err != nil {
		return err
	}
	defer s.close()
	tasks, err := s.selectTasks(o.Tasks)
	if err != nil {
		return err
	}
	runs := plan(o.Arms, tasks, o.Repeats, o.CapUSD)
	projected, basis, err := s.projection(runs, o.Arms, len(tasks), o.Repeats, o.BudgetUSD)
	fmt.Fprintf(s.Out, "plan: runs=%d arms=%s tasks=%d repeats=%d projected_usd=%.2f basis=%s budget_usd=%.2f\n", len(runs), armList(o.Arms), len(tasks), o.Repeats, projected, basis, o.BudgetUSD)
	if err != nil {
		return err
	}
	_, err = s.sweep(ctx, runs, tasks, o.BudgetUSD)
	return err
}

// Estimate is foliot bench estimate: the first five tasks once per arm, then the sweep
// cost projected from what they measured.
func Estimate(ctx context.Context, cfg Config, o SweepOptions) error {
	s, err := open(cfg)
	if err != nil {
		return err
	}
	defer s.close()
	ids := s.ids[:min(estimateTasks, len(s.ids))]
	tasks, err := s.selectTasks(ids)
	if err != nil {
		return err
	}
	runs := plan(o.Arms, tasks, 1, o.CapUSD)
	fmt.Fprintf(s.Out, "estimate: runs=%d arms=%s tasks=%s\n", len(runs), armList(o.Arms), strings.Join(ids, ","))
	costs, err := s.sweep(ctx, runs, tasks, 0)
	if err != nil {
		return err
	}
	est := log.BenchEstimate{Corpus: s.Corpus, Repeats: o.Repeats, Tasks: ids, MeanCostUSD: map[string]float64{}, CorpusTasks: len(s.ids)}
	for _, a := range o.Arms {
		c := costs[a.Name]
		if len(c) != len(tasks) {
			return refusef("arm %s: %d of %d runs have a known cost, so no mean is measured", a.Name, len(c), len(tasks))
		}
		var sum float64
		for _, x := range c {
			sum += x
		}
		est.Arms = append(est.Arms, a.Name)
		est.MeanCostUSD[a.Name] = sum / float64(len(c))
		est.ProjectedUSD += est.MeanCostUSD[a.Name] * float64(len(s.ids)) * float64(a.repeats(o.Repeats))
	}
	if _, err := s.log.Append(log.Entry{Type: "bench.estimate", Actor: "bench", Data: est}); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "estimate: corpus=%s tasks=%d repeats=%d projected_usd=%.2f\n", s.Corpus, len(s.ids), o.Repeats, est.ProjectedUSD)
	return nil
}

// sweep verifies the tasks, proves isolation, then runs every planned run in order,
// stopping at the budget or at the weekly rate limit. It returns each arm's known costs.
func (s *session) sweep(ctx context.Context, runs []plannedRun, tasks []*Task, budget float64) (map[string][]float64, error) {
	if len(runs) == 0 {
		return nil, refusef("the plan holds no runs")
	}
	if err := s.verify(ctx, tasks); err != nil {
		return nil, err
	}
	probeSeq, reading, err := s.probe(ctx, runs[0].arm, tasks[0], true, runs[0].cap)
	if err != nil {
		return nil, err
	}
	if !reading.proven() {
		return nil, refusef("isolation probe seq %d did not prove isolation: lines_seen=%d diff_seen=%d token_seen=%t denials=%d attempts=%d", probeSeq, reading.LinesSeen, reading.DiffSeen, reading.TokenSeen, reading.Denials, reading.Attempts)
	}
	costs := map[string][]float64{}
	var spent float64
	for _, r := range runs {
		if err := s.rateLimitOK(); err != nil {
			return costs, err
		}
		if budget > 0 && spent >= budget {
			return costs, refusef("spent $%.2f of --budget-usd %.2f; no further run starts", spent, budget)
		}
		c, err := s.runOne(ctx, r, probeSeq)
		if err != nil {
			return costs, err
		}
		if c != nil {
			spent += *c
			costs[r.arm.Name] = append(costs[r.arm.Name], *c)
		} else {
			spent += r.cap // an unknown cost counts at its cap, never at zero
		}
	}
	return costs, nil
}

func (s *session) rateLimitOK() error {
	if s.weekUsed != nil && *s.weekUsed >= MaxWeekUsed {
		return refusef("the subscription's seven-day window reads %.0f%% used, at or over %.0f%%; no further run starts", *s.weekUsed*100, MaxWeekUsed*100)
	}
	return nil
}

// workspace is one run's two directories: work is what the worker may read and write,
// record is what the runner keeps and the worker is denied.
type workspace struct {
	id, work, record, repo string
}

func (s *session) newWorkspace(task *Task, label string) (*workspace, error) {
	id := fmt.Sprintf("%s.%s.%s", task.ID, label, s.Now().UTC().Format("20060102T150405.000Z"))
	w := &workspace{id: id, work: filepath.Join(s.Root, "bench", "work", id), record: filepath.Join(s.Root, "bench", "runs", id)}
	w.repo = filepath.Join(w.work, "repo")
	for _, d := range []string{"home/.claude", "config", "state", "cache", "tmp", "codex"} {
		if err := os.MkdirAll(filepath.Join(w.work, d), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(w.record, 0o700); err != nil {
		return nil, err
	}
	// a5 section 2.13: unsigned and identifiable, with no credential helper.
	gitconfig := "[user]\n\tname = worker\n\temail = worker@invalid\n[credential]\n\thelper =\n[commit]\n\tgpgsign = false\n"
	return w, os.WriteFile(filepath.Join(w.work, "gitconfig"), []byte(gitconfig), 0o600)
}

func (s *session) isolation(w *workspace) Isolation {
	return Isolation{Root: s.Root, RealHome: s.realHome, Run: w.work, Toolchain: s.tool, TmpDirs: tmpEntries("/private/tmp", s.uid)}
}

func (s *session) env(w *workspace) []string {
	env := []string{
		"PATH=" + s.path,
		"HOME=" + filepath.Join(w.work, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(w.work, "config"),
		"XDG_STATE_HOME=" + filepath.Join(w.work, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(w.work, "cache"),
		"TMPDIR=" + filepath.Join(w.work, "tmp"),
		"CLAUDE_CODE_TMPDIR=" + filepath.Join(w.work, "tmp"),
		"CODEX_HOME=" + filepath.Join(w.work, "codex"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(w.work, "gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
		// Measured 2026-09-14: corpus v1's visible suite runs 306 s against the harness's
		// 120 s default Bash timeout, which would push every arm's suite to the background.
		"BASH_DEFAULT_TIMEOUT_MS=600000",
		// npm's update check reaches the registry, which the run profile denies.
		"NPM_CONFIG_UPDATE_NOTIFIER=false",
		tokenEnv + "=" + s.token,
	}
	for _, k := range []string{"LANG", "LC_ALL", "USER", "LOGNAME", "SHELL", "TERM"} {
		if v := s.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// prepare makes a workspace with a history-free checkout of the task at base_sha.
func (s *session) prepare(ctx context.Context, task *Task, label string) (*workspace, string, CheckoutHistory, error) {
	w, err := s.newWorkspace(task, label)
	if err != nil {
		return nil, "", CheckoutHistory{}, err
	}
	mirror, err := ensureMirror(ctx, filepath.Join(s.Root, "bench", "repos"), task.Repository, task.BaseSHA, task.LandedSHA)
	if err != nil {
		return w, "", CheckoutHistory{}, err
	}
	if err := historyFreeClone(ctx, mirror, w.repo, task.BaseSHA); err != nil {
		return w, mirror, CheckoutHistory{}, err
	}
	return w, mirror, assertHistoryFree(ctx, w.repo, task.LandedSHA), nil
}

func (s *session) writeProfile(w *workspace) error {
	b, err := s.isolation(w).settings(tokenEnv)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.work, "home", ".claude", "settings.json"), b, 0o600)
}

// probe runs the isolation probe (Fable critique k3 section 1.3 item 1) and records it.
func (s *session) probe(ctx context.Context, arm Arm, task *Task, isolated bool, capUSD float64) (int64, ProbeReading, error) {
	w, mirror, _, err := s.prepare(ctx, task, "probe-"+arm.Name)
	if err != nil {
		return 0, ProbeReading{}, err
	}
	defer os.RemoveAll(w.work)
	mode, permissions := "sandbox", "acceptEdits"
	if isolated {
		if err := s.writeProfile(w); err != nil {
			return 0, ProbeReading{}, err
		}
	} else {
		// The state the critique measured: no sandbox, and a worker whose shell runs
		// unprompted, which a bare arm needs to run its suite. Measured 2026-09-14: with no
		// profile and acceptEdits, headless Bash refuses `npm test` as well as the check, so
		// that mode is not a runnable arm and proves nothing about isolation.
		mode, permissions = "absent", "bypassPermissions"
	}
	diff, err := gitOut(ctx, mirror, "diff", "--no-color", "--no-ext-diff", "--unified=0", task.BaseSHA, task.LandedSHA)
	if err != nil {
		return 0, ProbeReading{}, err
	}
	st, _, err := s.launch(ctx, w, arm, capUSD, permissions, probePrompt(s.Root, task))
	if err != nil {
		return 0, ProbeReading{}, err
	}
	check, err := os.ReadFile(filepath.Join(s.Root, "bench", "checks", task.ID, "check.sh"))
	if err != nil {
		return 0, ProbeReading{}, err
	}
	reading := judgeProbe(check, answerLines(diff), s.token, st)
	p := log.BenchProbe{
		Corpus: s.Corpus, Task: task.ID, Arm: arm.Name, Model: arm.Model, Isolation: mode,
		CheckLines: reading.CheckLines, LinesSeen: reading.LinesSeen, Denials: reading.Denials,
		Attempts: reading.Attempts, DiffLines: reading.DiffLines, DiffSeen: reading.DiffSeen, TokenSeen: reading.TokenSeen,
		Proven: isolated && reading.proven(), Run: rel(s.Root, w.record),
	}
	if st.Result != nil {
		p.CostUSD = st.Result.TotalCostUSD
	}
	seq, err := s.log.Append(log.Entry{Type: "bench.probe", Actor: "bench", Data: p,
		Evidence: []log.Evidence{{Kind: "file", Ref: rel(s.Root, filepath.Join(w.record, "stream.jsonl"))}}})
	if err != nil {
		return 0, reading, err
	}
	fmt.Fprintf(s.Out, "probe: seq=%d arm=%s task=%s isolation=%s check_lines=%d lines_seen=%d diff_lines=%d diff_seen=%d token_seen=%t denials=%d attempts=%d proven=%t cost_usd=%s week_used=%s transcript=%s\n",
		seq, arm.Name, task.ID, mode, reading.CheckLines, reading.LinesSeen, reading.DiffLines, reading.DiffSeen, reading.TokenSeen, reading.Denials, reading.Attempts, p.Proven, fmtCost(p.CostUSD), fmtPct(st.WeekUsed), filepath.Join(w.record, "stream.jsonl"))
	if len(reading.Seen) > 0 {
		// Line numbers only: the check's text must not leave the denied directories.
		fmt.Fprintf(s.Out, "  seen: check lines %s\n", reading.seenAt())
	}
	return seq, reading, nil
}

// Probe is foliot bench probe: one isolation probe, recorded. Without isolation it is the
// red half of the proof and exits non-zero when the check reached the transcript.
func Probe(ctx context.Context, cfg Config, armName, taskID string, isolated bool, capUSD float64) error {
	arm, ok := armByName(armName)
	if !ok {
		return refusef("unknown arm %q; the arms are %s", armName, armNames())
	}
	s, err := open(cfg)
	if err != nil {
		return err
	}
	defer s.close()
	if taskID == "" {
		taskID = s.ids[0]
	}
	tasks, err := s.selectTasks([]string{taskID})
	if err != nil {
		return err
	}
	if capUSD <= 0 {
		capUSD = arm.CapUSD
	}
	seq, reading, err := s.probe(ctx, arm, tasks[0], isolated, capUSD)
	if err != nil {
		return err
	}
	if !isolated || !reading.proven() {
		return refusef("probe seq %d: isolation not proven (lines_seen=%d diff_seen=%d token_seen=%t denials=%d attempts=%d)", seq, reading.LinesSeen, reading.DiffSeen, reading.TokenSeen, reading.Denials, reading.Attempts)
	}
	return nil
}

// launch runs the harness headless in the workspace and reads its transcript.
func (s *session) launch(ctx context.Context, w *workspace, arm Arm, capUSD float64, permissions, prompt string) (*Stream, Observed, error) {
	if err := os.WriteFile(filepath.Join(w.record, "prompt.md"), []byte(prompt), 0o600); err != nil {
		return nil, Observed{}, err
	}
	streamPath := filepath.Join(w.record, "stream.jsonl")
	out, err := os.OpenFile(streamPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, Observed{}, err
	}
	defer out.Close()
	errLog, err := os.OpenFile(filepath.Join(w.record, "stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, Observed{}, err
	}
	defer errLog.Close()

	ctx, cancel := context.WithTimeout(ctx, workerDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Harness, "-p", prompt,
		"--output-format", "stream-json", "--verbose",
		"--model", arm.Model,
		"--max-budget-usd", fmt.Sprintf("%.2f", capUSD),
		"--no-session-persistence", "--strict-mcp-config",
		"--setting-sources", "user",
		"--tools", workerTools,
		"--permission-mode", permissions,
	)
	cmd.Dir = w.repo
	cmd.Env = s.env(w)
	cmd.Stdout, cmd.Stderr = out, errLog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	obs := Observed{LoadAtStart: loadAverage(), Injected: billingKind}
	start := time.Now()
	runErr := cmd.Run()
	obs.WallMS = time.Since(start).Milliseconds()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			obs.CPUMS = (ru.Utime.Nano() + ru.Stime.Nano()) / int64(time.Millisecond)
		}
	}
	var exit *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exit) {
		return nil, obs, fmt.Errorf("launching %s: %w", s.Harness, runErr)
	}
	f, err := os.Open(streamPath)
	if err != nil {
		return nil, obs, err
	}
	defer f.Close()
	st, err := ReadStream(f)
	if err != nil {
		return nil, obs, err
	}
	if st.WeekUsed != nil {
		s.weekUsed = st.WeekUsed
	}
	return st, obs, nil
}

// runOne makes, launches and scores one run, and returns its cost when the stream's cost
// reading passed its control.
func (s *session) runOne(ctx context.Context, r plannedRun, probeSeq int64) (*float64, error) {
	task := r.task
	w, mirror, hist, err := s.prepare(ctx, task, fmt.Sprintf("%s.r%d", r.arm.Name, r.repeat))
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(w.work)
	publicSince, _ := gitOut(ctx, mirror, "show", "-s", "--format=%cs", task.LandedSHA)
	facts := s.repoFacts(ctx, mirror, task)
	v := s.verified[task.ID]
	run := log.BenchRun{
		Corpus: s.Corpus, CorpusSHA: s.corpSHA, Task: task.ID, Class: task.Class, Arm: r.arm.Name,
		Adapter: adapterName, Gate: gateName, Repeat: r.repeat, Model: r.arm.Model, Provider: providerID,
		Billing: billingKind, BaseSHA: task.BaseSHA, BaseExit: v.Base, LandedExit: v.Landed, Benchmark: true,
		CapUSD: r.cap, IsolationProven: true, IsolationProbe: probeSeq,
		HistoryFree: hist.Free(), LandedObjectExit: hist.LandedObjectExit,
		ModelCutoff: r.arm.Cutoff, PublicSince: publicSince, Run: rel(s.Root, w.record),
	}
	run.Repository, run.RepositoryPublic, run.RepositoryLanguage = task.Repository, &facts.public, facts.language
	if !hist.Free() {
		if _, err := s.log.Append(log.Entry{Type: "bench.run", Actor: "bench", Data: run}); err != nil {
			return nil, err
		}
		return nil, refusef("task %s: the worker checkout is not history-free (cat-file -e landed exited %d, %d commits reachable); no worker launched", task.ID, hist.LandedObjectExit, hist.Commits)
	}
	setupLog := filepath.Join(w.record, "setup.log")
	if strings.TrimSpace(task.Setup) != "" {
		if rc := (&checkout{dir: w.repo}).sh(ctx, task.Setup, nil, setupTimeout, setupLog); rc != 0 {
			return nil, fmt.Errorf("task %s: setup exited %d before launch (log %s)", task.ID, rc, setupLog)
		}
	}
	if err := s.writeProfile(w); err != nil {
		return nil, err
	}
	runSeq, err := s.log.Append(log.Entry{Type: "bench.run", Actor: "bench", Data: run})
	if err != nil {
		return nil, err
	}

	st, obs, err := s.launch(ctx, w, r.arm, r.cap, "acceptEdits", renderPrompt(task))
	if err != nil {
		return nil, err
	}
	cols := st.columns(obs, w.work, s.Root, s.realHome)
	if st.Model != r.arm.Model {
		// The harness ran another model than the arm names: the run is not this arm's.
		cols["model_ran"] = st.Model
	}
	checkExit, examined, symlinks, confined, err := s.score(ctx, w, mirror, task)
	if err != nil {
		return nil, err
	}
	pass := checkExit == 0 && examined > 0 && st.Model == r.arm.Model
	claimedDone := st.Result != nil && st.Result.Subtype == "success" && !st.Result.IsError
	verdict := log.BenchVerdict{
		Corpus: s.Corpus, Task: task.ID, Arm: r.arm.Name, Repeat: r.repeat,
		Pass: pass, FalseClaim: claimedDone && !pass, Columns: map[string]any(cols),
		CheckExit: checkExit, Examined: examined,
		SymlinksSkipped: symlinks, WeekUsed: st.WeekUsed, HarnessVersion: st.Version, CheckConfined: confined,
	}
	cause := runSeq
	if _, err := s.log.Append(log.Entry{Type: "bench.verdict", Actor: "bench", Cause: &cause, Data: verdict, Evidence: []log.Evidence{
		{Kind: "file", Ref: rel(s.Root, filepath.Join(w.record, "stream.jsonl"))},
		{Kind: "file", Ref: rel(s.Root, filepath.Join(w.record, "check.log"))},
	}}); err != nil {
		return nil, err
	}
	var cost *float64
	if c, ok := cols["cost_usd"].(float64); ok {
		cost = &c
	}
	fmt.Fprintf(s.Out, "run: seq=%d arm=%s task=%s repeat=%d pass=%t false_claim=%t check_exit=%d examined=%d cost_usd=%s ended_by=%v week_used=%s record=%s\n",
		runSeq, r.arm.Name, task.ID, r.repeat, pass, verdict.FalseClaim, checkExit, examined, fmtCost(cost), cols["ended_by"], fmtPct(st.WeekUsed), w.record)
	return cost, nil
}

// score runs the hidden check on base_sha plus the worker's changes, in a fresh export the
// worker never touched, after the worker has exited (a5 section 2.16).
func (s *session) score(ctx context.Context, w *workspace, mirror string, task *Task) (int, int, int, bool, error) {
	dir := w.work + ".check"
	defer os.RemoveAll(dir)
	co, err := newCheckout(ctx, mirror, dir, task.BaseSHA, nil)
	if err != nil {
		return 0, 0, 0, false, err
	}
	changes, symlinks, err := treeChanges(co.dir, w.repo)
	if err != nil {
		return 0, 0, 0, false, err
	}
	if co, err = applyChanges(co, changes); err != nil {
		return 0, 0, 0, false, err
	}
	var paths []string
	for _, c := range changes {
		mark := "M"
		if c.deleted {
			mark = "D"
		}
		paths = append(paths, mark+" "+c.path)
	}
	sort.Strings(paths)
	_ = os.WriteFile(filepath.Join(w.record, "changes.txt"), []byte(strings.Join(paths, "\n")+"\n"), 0o600)
	if strings.TrimSpace(task.Setup) != "" {
		if rc := co.sh(ctx, task.Setup, nil, setupTimeout, filepath.Join(w.record, "check-setup.log")); rc != 0 {
			return rc, 0, symlinks, false, nil
		}
	}
	if err := copyDir(filepath.Join(s.Root, "bench", "checks", task.ID), filepath.Join(co.dir, ".bench-check")); err != nil {
		return 0, 0, 0, false, err
	}
	logPath := filepath.Join(w.record, "check.log")
	// The check needs the host's toolchain, so only the answer keys are denied here, not
	// the whole home; writes are what keep a planted copy from reaching a later run.
	command, confined := confineCheck("sh .bench-check/check.sh", co.dir, append([]string{s.Root}, critiqueDenied(s.realHome)...))
	env := append([]string{"TMPDIR=" + filepath.Join(co.dir, ".tmp")}, checkEnv...)
	if err := os.MkdirAll(filepath.Join(co.dir, ".tmp"), 0o700); err != nil {
		return 0, 0, 0, false, err
	}
	rc := co.sh(ctx, command, env, checkTimeout, logPath)
	return rc, lastExamined(logPath), symlinks, confined, nil
}

func applyChanges(co *checkout, changes []change) (*checkout, error) {
	for _, c := range changes {
		p := filepath.Join(co.dir, filepath.FromSlash(c.path))
		if c.deleted {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		_ = os.Remove(p) // a symlink in the export must not redirect the write
		if err := os.WriteFile(p, c.content, 0o644); err != nil {
			return nil, err
		}
	}
	return co, nil
}

// repoFacts reads, once per repository, whether it is readable with no credential and its
// majority source language, for the report's contamination header (k3 section 1.2).
func (s *session) repoFacts(ctx context.Context, mirror string, task *Task) repoFacts {
	if f, ok := s.repos[task.Repository]; ok {
		return f
	}
	var f repoFacts
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", task.Repository, "HEAD")
	cmd.Env = mergeEnv(environ(), []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "GH_TOKEN=", "GITHUB_TOKEN="})
	f.public = cmd.Run() == nil
	if tree, err := gitOut(ctx, mirror, "ls-tree", "-r", "--name-only", task.BaseSHA); err == nil {
		f.language = majorityLanguage(strings.Split(tree, "\n"))
	}
	s.repos[task.Repository] = f
	return f
}

var languages = map[string]string{
	".ts": "TypeScript", ".tsx": "TypeScript", ".js": "JavaScript", ".mjs": "JavaScript", ".go": "Go",
	".py": "Python", ".rs": "Rust", ".rb": "Ruby", ".java": "Java", ".swift": "Swift", ".c": "C", ".cc": "C++",
}

func majorityLanguage(files []string) string {
	counts := map[string]int{}
	for _, f := range files {
		if l, ok := languages[filepath.Ext(f)]; ok {
			counts[l]++
		}
	}
	best, n := "unknown", 0
	for l, c := range counts {
		if c > n || (c == n && l < best) {
			best, n = l, c
		}
	}
	return best
}

// renderPrompt is the prompt every arm receives, identical across arms (a5 section 2.16):
// the request verbatim at the head and the tail (p8 R19), the repository and base sha, the
// visible check, the scope and the benchmark's authorisation block.
func renderPrompt(t *Task) string {
	scope := "discover: the request names no files; find the ones the change needs"
	if len(t.Scope.Files) > 0 {
		scope = "the files the request named: " + strings.Join(t.Scope.Files, ", ")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "<request sha256=%q>\n%s\n</request>\n\n", t.RequestSHA256, t.Request)
	fmt.Fprintf(&b, "Repository: %s, checked out at %s in your working directory.\n", t.Repository, t.BaseSHA)
	fmt.Fprintf(&b, "Visible check: `%s` (dependencies are installed).\n", t.VisibleCheck)
	fmt.Fprintf(&b, "Scope: %s.\n\n", scope)
	b.WriteString(authorisation)
	fmt.Fprintf(&b, "\n<request sha256=%q>\n%s\n</request>\n", t.RequestSHA256, t.Request)
	return b.String()
}

// authorisation is the benchmark setting's block: what the worker may do without asking.
const authorisation = `Authorised without asking: read and change any file in this working directory, run any command inside it, and commit locally. Not authorised: pushing, opening pull requests, or reaching the network; nobody will answer a question during this run. When the change is complete, stop.
`

func loadAverage() *float64 {
	return readLoadAverage()
}

func fmtCost(c *float64) string {
	if c == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.4f", *c)
}

func fmtPct(p *float64) string {
	if p == nil {
		return "unread"
	}
	return fmt.Sprintf("%.0f%%", *p*100)
}
