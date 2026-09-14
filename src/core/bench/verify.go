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
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	setupTimeout = 10 * time.Minute
	checkTimeout = 10 * time.Minute
	// A request line at least this long that the landed diff adds verbatim names the solution.
	solutionLineMin = 24
)

// checkEnv points every proxy-honouring client at a closed port while the hidden check runs.
// simplified: detection by scan plus this denial; a kernel-level deny (unshare -n,
// sandbox-exec) is the upgrade where the host permits one.
var checkEnv = []string{
	"http_proxy=http://127.0.0.1:9", "https_proxy=http://127.0.0.1:9", "all_proxy=http://127.0.0.1:9",
	"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9",
	"NO_PROXY=", "no_proxy=", "NODE_USE_ENV_PROXY=1", "npm_config_offline=true",
}

// DefaultVisibleRuns is two copies: the fewest that can disagree, and each copy is a full suite
// of CPU. On 2026-09-14 a 3-copy g9 round went red in every copy or none (6 red of 36 runs), so
// more copies of one round add little; the base and landed rounds are the second sample.
const DefaultVisibleRuns = 2

// visibleTimeout is a variable so a test can plant a hanging suite without waiting half an hour.
var visibleTimeout = 30 * time.Minute

// loadInterval is how often the load average is read while visible copies run or drain, and
// drainTimeout bounds the wait for it to fall to the cpu count before a re-run alone; both are
// variables so a test need not wait for them.
var (
	loadInterval = 15 * time.Second
	drainTimeout = 15 * time.Minute
)

var examinedRe = regexp.MustCompile(`^examined=([0-9]+)$`)

// Options select what Verify examines.
type Options struct {
	Root   string   // the resolved <root>
	Corpus string   // the corpus name, e.g. v1
	Tasks  []string // restrict to these ids; empty means the whole corpus
	Jobs   int      // tasks verified at once
	// VisibleRuns is how many copies of the visible suite run at once at base_sha, and then
	// at landed_sha; at least 2, since one run cannot tell a flaky suite from a green one.
	VisibleRuns int
	Out         io.Writer
}

// Result is one task's reading. An exit code of notRun means the step did not run.
type Result struct {
	ID, Class                  string
	Base, Landed, Additions    int
	VisibleBase, VisibleLanded Runs
	// AloneBase and AloneLanded are the re-run of a sha whose red was read above the cpu count,
	// with no other task beside it; LoadSensitive marks a task that was red only under load.
	AloneBase, AloneLanded Runs
	LoadSensitive          bool
	pending                bool
	AdditionsSameAsBase    bool
	Examined               int
	Refusals               []string
}

// Runs is one sha's visible-suite reading: the exit code of each concurrent copy, empty when
// the suite did not run, and the highest one-minute load average read while the copies ran,
// negative when no reading succeeded, so a red copy can be told from an overloaded machine.
type Runs struct {
	Codes   []int
	LoadMax float64
}

// Red is how many copies exited non-zero; Red over len(Codes) is the observed flake rate when
// it is neither 0 nor len(Codes).
func (r Runs) Red() int {
	n := 0
	for _, rc := range r.Codes {
		if rc != 0 {
			n++
		}
	}
	return n
}

func (r Runs) String() string {
	if len(r.Codes) == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", r.Red(), len(r.Codes))
}

func (r Runs) load() string {
	switch {
	case len(r.Codes) == 0:
		return "-"
	case r.LoadMax < 0:
		return "unknown"
	}
	return loadString(r.LoadMax)
}

func loadString(load float64) string {
	if load < 0 {
		return "unknown"
	}
	return strconv.FormatFloat(load, 'f', 1, 64)
}

var loadRe = regexp.MustCompile(`load averages?: *([0-9]+[.,][0-9]+)`)

// overloaded is true only for a load reading above the cpu count; an unknown load excuses nothing.
func (r Runs) overloaded() bool { return r.LoadMax > float64(runtime.NumCPU()) }

func (r Runs) describe() string {
	return fmt.Sprintf("red in %d of %d concurrent runs, exit codes %v, load average up to %s on %d cpus", r.Red(), len(r.Codes), r.Codes, r.load(), runtime.NumCPU())
}

// loadAverage reads the one-minute load average from uptime(1), which macOS and Linux both
// print; -1 when it cannot be read. A variable so a test can set the machine's load.
var loadAverage = func(ctx context.Context) float64 {
	cmd := exec.CommandContext(ctx, "uptime")
	cmd.Env = mergeEnv(os.Environ(), []string{"LC_ALL=C"})
	out, err := cmd.Output()
	m := loadRe.FindSubmatch(out)
	if err != nil || m == nil {
		return -1
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(string(m[1]), ",", "."), 64)
	if err != nil {
		return -1
	}
	return v
}

const notRun = -1000

// Summary is the corpus reading Verify prints last.
type Summary struct {
	Examined, OK, Refused   int
	Classes                 map[string]int
	CorpusSHA               string
	Dirty, Partial          bool
	Jobs, VisibleRuns, CPUs int
	Problems                []string
	Results                 []Result
}

// Success is true only over a non-zero count with nothing refused (K7).
func (s Summary) Success() bool {
	return s.Examined > 0 && s.Refused == 0 && len(s.Problems) == 0
}

// Verify checks every task of a corpus against the six criteria of a5 §2.16.
func Verify(ctx context.Context, o Options) (Summary, error) {
	if o.VisibleRuns < 2 {
		return Summary{}, fmt.Errorf("visible runs is %d; one run cannot tell a flaky suite from a green one, so it must be at least 2", o.VisibleRuns)
	}
	bench := filepath.Join(o.Root, "bench")
	corpusDir := filepath.Join(bench, "corpus", o.Corpus)
	m, err := LoadManifest(filepath.Join(corpusDir, "corpus.json"))
	if err != nil {
		return Summary{}, err
	}
	ids, err := taskIDs(corpusDir)
	if err != nil {
		return Summary{}, err
	}
	jobs := max(1, o.Jobs)
	s := Summary{Classes: map[string]int{}, Jobs: jobs, VisibleRuns: o.VisibleRuns, CPUs: runtime.NumCPU()}
	if len(o.Tasks) > 0 {
		known := map[string]bool{}
		for _, id := range ids {
			known[id] = true
		}
		for _, id := range o.Tasks {
			if !known[id] {
				return Summary{}, fmt.Errorf("task %q is not in corpus %s", id, o.Corpus)
			}
		}
		s.Partial = len(o.Tasks) < len(ids)
		ids = o.Tasks
	}
	s.CorpusSHA, s.Dirty = corpusVersion(bench)

	results := make([]Result, len(ids))
	alone := make([]func(*Result), len(ids))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	var outMu sync.Mutex
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], alone[i] = verifyTask(ctx, bench, corpusDir, m, id, o.VisibleRuns)
			outMu.Lock()
			printResult(o.Out, results[i], filepath.Join(bench, "verify", id))
			outMu.Unlock()
		}()
	}
	wg.Wait()
	// A red read while the load exceeded the cpu count says as much about the machine as about
	// the suite, so that sha is run again once every other task is done, one task at a time.
	for i, rerun := range alone {
		if rerun != nil {
			rerun(&results[i])
			printResult(o.Out, results[i], filepath.Join(bench, "verify", ids[i]))
		}
	}

	for _, r := range results {
		s.Examined++
		if len(r.Refusals) == 0 {
			s.OK++
			s.Classes[r.Class]++
		} else {
			s.Refused++
		}
	}
	if s.Dirty {
		s.Problems = append(s.Problems, "the corpus repository has uncommitted changes under corpus/ or checks/, so no sha pins what was verified")
	}
	if !s.Partial {
		for _, c := range Classes {
			if s.Classes[c] != m.Classes[c] {
				s.Problems = append(s.Problems, fmt.Sprintf("class %s: %d tasks verified, %d pre-registered", c, s.Classes[c], m.Classes[c]))
			}
		}
	}
	if s.Examined == 0 {
		s.Problems = append(s.Problems, "examined=0 is never a pass")
	}
	s.Results = results
	printSummary(o.Out, s)
	return s, nil
}

func taskIDs(corpusDir string) ([]string, error) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(corpusDir, e.Name(), "task.json")); err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// corpusVersion reads the sha that pins corpus/ and checks/ (decided 2026-09-14: <root>/bench
// is a local git repository), and whether either has changes that sha does not hold.
func corpusVersion(bench string) (string, bool) {
	sha, err := gitOut(context.Background(), bench, "rev-parse", "HEAD")
	if err != nil {
		return "none", true
	}
	status, err := gitOut(context.Background(), bench, "status", "--porcelain", "--untracked-files=all", "--", "corpus", "checks")
	return sha, err != nil || status != ""
}

// verifyTask returns the task's reading and, when a visible red was read under overload, the
// re-run that decides it.
func verifyTask(ctx context.Context, bench, corpusDir string, m *Manifest, id string, copies int) (Result, func(*Result)) {
	r := Result{ID: id, Base: notRun, Landed: notRun, Additions: notRun}
	t, refusals := LoadTask(filepath.Join(corpusDir, id, "task.json"), m)
	checkDir := filepath.Join(bench, "checks", id)
	visible := ""
	if t != nil {
		visible = t.VisibleCheck
	}
	files, more := checkFiles(checkDir, visible, filepath.Dir(bench))
	r.Refusals = append(refusals, more...)
	if t == nil {
		return r, nil
	}
	r.Class = t.Class
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, sha := range []string{t.BaseSHA, t.LandedSHA} {
			if len(sha) >= 7 && bytes.Contains(b, []byte(sha[:7])) {
				r.Refusals = append(r.Refusals, fmt.Sprintf("criterion 3: %s names a pinned sha, so it can tell the checkouts apart without testing them", rel(checkDir, f)))
			}
		}
	}

	mirror, err := ensureMirror(ctx, filepath.Join(bench, "repos"), t.Repository, t.BaseSHA, t.LandedSHA)
	if err != nil {
		r.Refusals = append(r.Refusals, "criterion 2: "+err.Error())
		return r, nil
	}
	r.Refusals = append(r.Refusals, historyRefusals(ctx, mirror, t, files)...)
	added, err := addedPaths(ctx, mirror, t)
	if err != nil {
		r.Refusals = append(r.Refusals, "criterion 3: "+err.Error())
	}
	if len(r.Refusals) > 0 {
		return r, nil
	}

	work := filepath.Join(bench, "verify", id)
	if err := os.RemoveAll(work); err != nil {
		r.Refusals = append(r.Refusals, err.Error())
		return r, nil
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		r.Refusals = append(r.Refusals, err.Error())
		return r, nil
	}
	refuse := func(format string, a ...any) { r.Refusals = append(r.Refusals, fmt.Sprintf(format, a...)) }

	// Each phase is a fresh export of the pinned tree with no .git, so a check can read the
	// code it tests and nothing that tells it which commit it is in.
	run := func(phase, sha string, overlay []change) int {
		co, err := newCheckout(ctx, mirror, filepath.Join(work, phase), sha, overlay)
		if err != nil {
			refuse("criterion 3: %s checkout: %v", phase, err)
			return notRun
		}
		defer co.remove()
		if strings.TrimSpace(t.Setup) != "" {
			if rc := co.sh(ctx, t.Setup, nil, setupTimeout, filepath.Join(work, phase+"-setup.log")); rc != 0 {
				refuse("criterion 1: setup `%s` exited %d at %s (log %s)", t.Setup, rc, phase, filepath.Join(work, phase+"-setup.log"))
				return notRun
			}
		}
		if err := copyDir(checkDir, filepath.Join(co.dir, ".bench-check")); err != nil {
			refuse("criterion 3: copying the check into the %s checkout: %v", phase, err)
			return notRun
		}
		return co.sh(ctx, "sh .bench-check/check.sh", checkEnv, checkTimeout, filepath.Join(work, phase+"-check.log"))
	}

	r.Base = run("base", t.BaseSHA, nil)
	if r.Base == 0 {
		refuse("criterion 3: the hidden check exits 0 at base_sha, so it passes on an empty change")
	}
	r.Landed = run("landed", t.LandedSHA, nil)
	if r.Landed != 0 && r.Landed != notRun {
		refuse("criterion 3: the hidden check exits %d at landed_sha", r.Landed)
	}
	if r.Landed == 0 {
		r.Examined = lastExamined(filepath.Join(work, "landed-check.log"))
		if r.Examined <= 0 {
			refuse("criterion 3: the hidden check at landed_sha printed no examined=<n> with n > 0 as its last line (K7)")
		}
	}
	if len(added) == 0 {
		r.AdditionsSameAsBase = true
	} else if r.Additions = run("additions", t.BaseSHA, added); r.Additions == 0 {
		refuse("criterion 3: the hidden check exits 0 on base_sha plus only the files the landed change adds, so it reads new files rather than testing the changed behaviour")
	}
	// The visible suite is the slowest step, so it runs last, only for a task nothing refused.
	if len(r.Refusals) > 0 {
		return r, nil
	}
	type side struct {
		phase, sha   string
		under, alone func(*Result) *Runs
	}
	var overloaded []side
	for _, p := range []side{
		{"base", t.BaseSHA, func(r *Result) *Runs { return &r.VisibleBase }, func(r *Result) *Runs { return &r.AloneBase }},
		{"landed", t.LandedSHA, func(r *Result) *Runs { return &r.VisibleLanded }, func(r *Result) *Runs { return &r.AloneLanded }},
	} {
		runs, why := visibleRuns(ctx, mirror, work, t, p.phase, p.sha, copies)
		*p.under(&r) = runs
		switch {
		case why != "":
			refuse("%s", why)
		case runs.Red() > 0 && runs.overloaded():
			overloaded = append(overloaded, p)
		case runs.Red() > 0:
			refuse("%s", visibleRefusal(t, p.phase, runs, work))
		}
	}
	if len(overloaded) == 0 || len(r.Refusals) > 0 {
		return r, nil
	}
	r.pending = true
	return r, func(r *Result) {
		r.pending = false
		for _, p := range overloaded {
			if load, ok := drain(ctx); !ok {
				r.Refusals = append(r.Refusals, fmt.Sprintf("criterion 1: the load average did not fall to the cpu count within %s (last reading %s on %d cpus), so the red at %s_sha was not re-run alone and is inconclusive, not certified: %s beside other tasks", drainTimeout, loadString(load), runtime.NumCPU(), p.phase, p.under(r).describe()))
				continue
			}
			runs, why := visibleRuns(ctx, mirror, work, t, p.phase+"-alone", p.sha, copies)
			*p.alone(r) = runs
			under := *p.under(r)
			switch {
			case why != "":
				r.Refusals = append(r.Refusals, why)
			case runs.Red() == 0:
				r.LoadSensitive = true
			case runs.overloaded():
				r.Refusals = append(r.Refusals, fmt.Sprintf("criterion 1: the visible check `%s` is red again at %s_sha run alone and the load stayed above the cpu count, so the reading is inconclusive and not certified: alone %s, after %s beside other tasks", t.VisibleCheck, p.phase, runs.describe(), under.describe()))
			default:
				r.Refusals = append(r.Refusals, fmt.Sprintf("criterion 1: the visible check `%s` is red again at %s_sha run alone: %s, after %s beside other tasks (logs %s)", t.VisibleCheck, p.phase, runs.describe(), under.describe(), filepath.Join(work, "visible-"+p.phase+"-alone-<n>.log")))
			}
		}
	}
}

// drain waits until the one-minute load average is at or under the cpu count, for at most
// drainTimeout, and returns the last reading and whether it got there.
func drain(ctx context.Context) (float64, bool) {
	deadline := time.Now().Add(drainTimeout)
	for {
		load := loadAverage(ctx)
		if load >= 0 && load <= float64(runtime.NumCPU()) {
			return load, true
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return load, false
		}
		select {
		case <-ctx.Done():
		case <-time.After(loadInterval):
		}
	}
}

// visibleRefusal names a red visible reading: red in every copy, or not deterministic.
func visibleRefusal(t *Task, phase string, runs Runs, work string) string {
	what := "is red"
	if red := runs.Red(); red < len(runs.Codes) {
		what = fmt.Sprintf("is not deterministic, flake rate %.2f,", float64(red)/float64(len(runs.Codes)))
	}
	return fmt.Sprintf("criterion 1: the visible check `%s` %s at %s_sha: %s (logs %s)", t.VisibleCheck, what, phase, runs.describe(), filepath.Join(work, "visible-"+phase+"-<n>.log"))
}

// historyRefusals checks criterion 2 (the landed change is the merged pull request, on the
// default branch), criterion 4 (no check file is a repository file) and criterion 6 (the
// request quotes no line the landed diff adds).
func historyRefusals(ctx context.Context, mirror string, t *Task, files []string) []string {
	var out []string
	if _, err := gitOut(ctx, mirror, "merge-base", "--is-ancestor", t.BaseSHA, t.LandedSHA); err != nil {
		out = append(out, "criterion 2: base_sha is not an ancestor of landed_sha")
	}
	if _, err := gitOut(ctx, mirror, "merge-base", "--is-ancestor", t.LandedSHA, "HEAD"); err != nil {
		out = append(out, "criterion 2: landed_sha is not on the repository's default branch")
	}
	if pr := prRe.FindStringSubmatch(t.PullRequest); pr != nil {
		msg, err := gitOut(ctx, mirror, "log", "-1", "--format=%B", t.LandedSHA)
		n := regexp.QuoteMeta(pr[1])
		if err != nil || !regexp.MustCompile(`\(#`+n+`\)|Merge pull request #`+n+`\b`).MatchString(msg) {
			out = append(out, fmt.Sprintf("criterion 2: landed_sha's message does not name pull request #%s", pr[1]))
		}
	}

	blobs := map[string]bool{}
	for _, sha := range []string{t.BaseSHA, t.LandedSHA} {
		tree, err := gitOut(ctx, mirror, "ls-tree", "-r", sha)
		if err != nil {
			out = append(out, "criterion 4: "+err.Error())
		}
		for _, line := range strings.Split(tree, "\n") {
			if f := strings.Fields(line); len(f) >= 3 {
				blobs[f[2]] = true
			}
		}
	}
	for _, f := range files {
		id, err := gitOut(ctx, mirror, "hash-object", "--no-filters", f)
		if err == nil && blobs[id] {
			out = append(out, fmt.Sprintf("criterion 4: %s is byte-identical to a file in the repository", filepath.Base(f)))
		}
	}

	diff, err := gitOut(ctx, mirror, "diff", "--no-color", "--no-ext-diff", "--unified=0", t.BaseSHA, t.LandedSHA)
	if err != nil {
		out = append(out, "criterion 6: "+err.Error())
	}
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		if l := strings.TrimSpace(line[1:]); len(l) >= solutionLineMin && strings.Contains(t.Request, l) {
			out = append(out, fmt.Sprintf("criterion 6: the request quotes a line the landed diff adds: %q", l))
			break
		}
	}
	return out
}

type change struct {
	path    string
	deleted bool
	content []byte
}

// addedPaths is the landed change's additions: the files it adds, with their landed content,
// and the files it deletes. Over base content they make a tree that holds everything a check
// could read from a new file and none of the edits to existing code.
func addedPaths(ctx context.Context, mirror string, t *Task) ([]change, error) {
	out, err := gitOut(ctx, mirror, "diff", "--name-status", "--no-renames", "-z", t.BaseSHA, t.LandedSHA)
	if err != nil {
		return nil, err
	}
	var changes []change
	f := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		switch f[i] {
		case "A":
			blob, err := exec.CommandContext(ctx, "git", "--git-dir", mirror, "cat-file", "blob", t.LandedSHA+":"+f[i+1]).Output()
			if err != nil {
				return nil, fmt.Errorf("reading %s at landed_sha: %w", f[i+1], err)
			}
			changes = append(changes, change{path: f[i+1], content: blob})
		case "D":
			changes = append(changes, change{path: f[i+1], deleted: true})
		}
	}
	return changes, nil
}

var mirrorLocks sync.Map

// ensureMirror returns <root>/bench/repos/<name>.git holding both shas, cloning or fetching once.
func ensureMirror(ctx context.Context, repos, url string, shas ...string) (string, error) {
	name := strings.TrimSuffix(filepath.Base(strings.TrimRight(url, "/")), ".git")
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("repository %q names no repository", url)
	}
	path := filepath.Join(repos, name+".git")
	mu, _ := mirrorLocks.LoadOrStore(path, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(repos, 0o755); err != nil {
			return "", err
		}
		if _, err := gitOut(ctx, repos, "clone", "--quiet", "--mirror", url, path); err != nil {
			return "", fmt.Errorf("cloning %s: %w", url, err)
		}
	}
	have := func() bool {
		for _, sha := range shas {
			if _, err := gitOut(ctx, path, "cat-file", "-e", sha+"^{commit}"); err != nil {
				return false
			}
		}
		return true
	}
	if !have() {
		if _, err := gitOut(ctx, path, "fetch", "--quiet", "--prune", "origin"); err != nil {
			return "", fmt.Errorf("fetching %s: %w", url, err)
		}
		if !have() {
			return "", fmt.Errorf("%s does not hold base_sha and landed_sha", url)
		}
	}
	return path, nil
}

// visibleRuns sets up one clone at sha, copies it n-1 times, and runs the visible suite in all n
// copies at once, so every run shares the machine with n-1 copies of itself: a suite that is red
// beside itself is not deterministic, and benchmark arms run beside each other. It returns the
// reading, or why the copies could not be made. The clone is a git clone, not an export, because
// a repository's own suite may read its index (git ls-files); setup runs once and the tree is
// copied, since a copy of an installed tree costs seconds where each install costs a registry
// round trip (hist: cp -R of a 129 MB treadle tree, 4.99 s).
func visibleRuns(ctx context.Context, mirror, work string, t *Task, phase, sha string, n int) (Runs, string) {
	dir := func(i int) string { return filepath.Join(work, fmt.Sprintf("visible-%s-%d", phase, i+1)) }
	cos := make([]*checkout, n)
	defer func() {
		for _, co := range cos {
			if co != nil {
				co.remove()
			}
		}
	}()
	co, err := newClone(ctx, mirror, dir(0), sha)
	if err != nil {
		return Runs{}, fmt.Sprintf("criterion 1: visible checkout at %s_sha: %v", phase, err)
	}
	cos[0] = co
	if strings.TrimSpace(t.Setup) != "" {
		log := filepath.Join(work, fmt.Sprintf("visible-%s-setup.log", phase))
		if rc := co.sh(ctx, t.Setup, nil, setupTimeout, log); rc != 0 {
			return Runs{}, fmt.Sprintf("criterion 1: setup `%s` exited %d for the visible check at %s_sha (log %s)", t.Setup, rc, phase, log)
		}
	}
	for i := 1; i < n; i++ {
		// cp -R keeps symlinks as symlinks (node_modules/.bin), which copyDir does not.
		if out, err := exec.CommandContext(ctx, "cp", "-R", dir(0), dir(i)).CombinedOutput(); err != nil {
			return Runs{}, fmt.Sprintf("criterion 1: copying the visible checkout at %s_sha: %v: %s", phase, err, bytes.TrimSpace(out))
		}
		cos[i] = &checkout{dir: dir(i)}
	}

	runs := Runs{Codes: make([]int, n), LoadMax: loadAverage(ctx)}
	stop, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(loadInterval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				runs.LoadMax = max(runs.LoadMax, loadAverage(ctx))
			}
		}
	}()
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			runs.Codes[i] = cos[i].sh(ctx, t.VisibleCheck, nil, visibleTimeout, filepath.Join(work, fmt.Sprintf("visible-%s-%d.log", phase, i+1)))
		})
	}
	wg.Wait()
	close(stop)
	<-sampled
	return runs, ""
}

type checkout struct{ dir string }

func newClone(ctx context.Context, mirror, dir, sha string) (*checkout, error) {
	if _, err := gitOut(ctx, filepath.Dir(dir), "clone", "--quiet", "--shared", "--no-checkout", mirror, dir); err != nil {
		return nil, err
	}
	if _, err := gitOut(ctx, dir, "checkout", "--quiet", "--detach", sha); err != nil {
		return nil, err
	}
	return &checkout{dir: dir}, nil
}

func newCheckout(ctx context.Context, mirror, dir, sha string, overlay []change) (*checkout, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	archive := exec.CommandContext(ctx, "git", "--git-dir", mirror, "archive", "--format=tar", sha)
	untar := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", dir)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return nil, err
	}
	untar.Stdin = pipe
	var archiveErr, untarErr bytes.Buffer
	archive.Stderr, untar.Stderr = &archiveErr, &untarErr
	if err := untar.Start(); err != nil {
		return nil, err
	}
	if err := archive.Run(); err != nil {
		_ = untar.Wait()
		return nil, fmt.Errorf("git archive %s: %v: %s", sha, err, archiveErr.String())
	}
	if err := untar.Wait(); err != nil {
		return nil, fmt.Errorf("tar: %v: %s", err, untarErr.String())
	}
	for _, c := range overlay {
		p := filepath.Join(dir, filepath.FromSlash(c.path))
		if c.deleted {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, c.content, 0o644); err != nil {
			return nil, err
		}
	}
	return &checkout{dir: dir}, nil
}

// sh runs a command in its own process group under a deadline, output to log, and returns its
// exit code; a command that cannot start or is killed returns a non-zero code.
func (c *checkout) sh(ctx context.Context, command string, env []string, timeout time.Duration, log string) int {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	f, err := os.Create(log)
	if err != nil {
		return notRun
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = c.dir
	cmd.Env = mergeEnv(environ(), append([]string{"CI=true"}, env...))
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	// Reap anything the command left running in its group.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case ctx.Err() != nil:
		fmt.Fprintf(f, "\nkilled after %s\n", timeout)
		return 124
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode()
	default:
		fmt.Fprintf(f, "\n%v\n", err)
		return 125
	}
}

// mergeEnv drops every base entry whose key an override sets, so the override is the only value
// a child process can read for that key, regardless of a platform's duplicate-key resolution.
func mergeEnv(base, overrides []string) []string {
	set := make(map[string]bool, len(overrides))
	for _, kv := range overrides {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			set[kv[:i]] = true
		}
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i >= 0 && set[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, overrides...)
}

// environ is the process environment without FOLIOT_HOME, so no step learns where the corpus is.
func environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FOLIOT_HOME=") {
			out = append(out, kv)
		}
	}
	return out
}

func (c *checkout) remove() { _ = os.RemoveAll(c.dir) }

func lastExamined(log string) int {
	b, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if m := examinedRe.FindStringSubmatch(strings.TrimSpace(lines[len(lines)-1])); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel(from, p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o755)
	})
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func code(rc int) string {
	if rc == notRun {
		return "-"
	}
	return strconv.Itoa(rc)
}

func printResult(w io.Writer, r Result, logs string) {
	verdict := "ok"
	switch {
	case len(r.Refusals) > 0:
		verdict = "REFUSED"
	case r.pending:
		verdict = "rerun-alone"
	}
	additions := code(r.Additions)
	if r.AdditionsSameAsBase {
		additions = "same-as-base"
	}
	fmt.Fprintf(w, "task=%s class=%s base=%s landed=%s additions=%s visible_red_at_base=%s load_at_base=%s visible_red_at_landed=%s load_at_landed=%s alone_red_at_base=%s alone_load_at_base=%s alone_red_at_landed=%s alone_load_at_landed=%s load_sensitive=%t examined=%d verdict=%s logs=%s\n",
		r.ID, r.Class, code(r.Base), code(r.Landed), additions, r.VisibleBase, r.VisibleBase.load(), r.VisibleLanded, r.VisibleLanded.load(),
		r.AloneBase, r.AloneBase.load(), r.AloneLanded, r.AloneLanded.load(), r.LoadSensitive, r.Examined, verdict, logs)
	for _, why := range r.Refusals {
		fmt.Fprintf(w, "  refused: %s\n", why)
	}
}

func printSummary(w io.Writer, s Summary) {
	var classes []string
	for _, c := range Classes {
		classes = append(classes, fmt.Sprintf("%s=%d", c, s.Classes[c]))
	}
	scope := "full"
	if s.Partial {
		scope = "partial"
	}
	dirty := ""
	if s.Dirty {
		dirty = "+uncommitted"
	}
	fmt.Fprintf(w, "examined=%d ok=%d refused=%d %s scope=%s corpus_sha=%s%s jobs=%d visible_runs=%d cpus=%d\n",
		s.Examined, s.OK, s.Refused, strings.Join(classes, " "), scope, s.CorpusSHA, dirty, s.Jobs, s.VisibleRuns, s.CPUs)
	for _, p := range s.Problems {
		fmt.Fprintf(w, "refused: %s\n", p)
	}
}
