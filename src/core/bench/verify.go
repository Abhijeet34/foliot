package bench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// visibleTimeout bounds one visible suite run; a variable so a test can plant a hang.
var visibleTimeout = 30 * time.Minute

// wallNow is the wall clock stripped of Go's monotonic reading, which on darwin stops while the
// machine sleeps; wallPoll is how often a deadline reads it. Variables so a test can plant a sleep.
var (
	wallNow  = func() time.Time { return time.Now().Round(0) }
	wallPoll = time.Second
)

// withWallTimeout is context.WithTimeout over elapsed wall-clock time, sleep included. Go's timers
// alone let a 30-minute deadline pass a suite that ran 61 minutes across a 58-minute hibernation
// (measured 2026-09-15), so the context is also cancelled once the wall clock reads the deadline.
func withWallTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	now := wallNow
	deadline := now().Add(timeout)
	go func() {
		tick := time.NewTicker(min(wallPoll, timeout))
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if !now().Before(deadline) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

var examinedRe = regexp.MustCompile(`^examined=([0-9]+)$`)

// Options select what Verify examines.
type Options struct {
	Root   string   // the resolved <root>
	Corpus string   // the corpus name, e.g. v1
	Tasks  []string // restrict to these ids; empty means the whole corpus
	Jobs   int      // tasks whose hidden phases run at once; visible suites run one at a time
	Out    io.Writer
}

// Result is one task's reading. An exit code of notRun means the step did not run.
type Result struct {
	ID, Class                  string
	Base, Landed, Additions    int
	VisibleBase, VisibleLanded VisibleRun
	AdditionsSameAsBase        bool
	Examined                   int
	Refusals                   []string
	TaskSHA                    string // taskSHA of what was verified
}

// VisibleRun is one run of a task's visible suite, and the classifying re-run a red gets.
// WallMS, CPUMS and LoadAtStart are columns and never a verdict input.
type VisibleRun struct {
	Exit, Examined int
	WallMS, CPUMS  int64
	LoadAtStart    *float64
	Rerun          *VisibleRun
}

const notRun = -1000

// Summary is the corpus reading Verify prints last.
type Summary struct {
	Examined, OK, Refused int
	Classes               map[string]int
	CorpusSHA             string
	Dirty, Partial        bool
	Problems              []string
	Results               []Result
}

// Success is true only over a non-zero count with nothing refused (K7).
func (s Summary) Success() bool {
	return s.Examined > 0 && s.Refused == 0 && len(s.Problems) == 0
}

// Verify checks every task of a corpus against the six criteria of a5 §2.16.
func Verify(ctx context.Context, o Options) (Summary, error) {
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
	s := Summary{Classes: map[string]int{}}
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

	// Pass 1: everything but the visible suites, o.Jobs tasks at once.
	results := make([]Result, len(ids))
	suites := make([]*visibleSuite, len(ids))
	sem := make(chan struct{}, max(1, o.Jobs))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], suites[i] = verifyTask(ctx, bench, corpusDir, m, id)
		}()
	}
	wg.Wait()
	// Pass 2: the visible suites, serially and after every hidden phase has ended, because an
	// arm's worker runs its suite alone; a future best-of-N selector runs its suites the same way.
	verifyVisible(ctx, m, results, suites)

	for i, r := range results {
		printResult(o.Out, r, filepath.Join(bench, "verify", ids[i]))
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

// visibleSuite is what pass 2 needs of a task pass 1 left unrefused.
type visibleSuite struct {
	task         *Task
	mirror, work string
}

// verifyTask is pass 1 for one task. It returns the task's visible suite to run, or nil when
// the task is already refused.
func verifyTask(ctx context.Context, bench, corpusDir string, m *Manifest, id string) (Result, *visibleSuite) {
	r := Result{ID: id, Base: notRun, Landed: notRun, Additions: notRun, VisibleBase: VisibleRun{Exit: notRun}, VisibleLanded: VisibleRun{Exit: notRun}}
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
	if len(r.Refusals) > 0 {
		return r, nil
	}
	if r.TaskSHA, err = taskSHA(filepath.Dir(bench), filepath.Base(corpusDir), id); err != nil {
		refuse("record: hashing the task's content: %v", err)
		return r, nil
	}
	return r, &visibleSuite{task: t, mirror: mirror, work: work}
}

// taskSHA is the hex sha256 over task.json's bytes, then each file under checks/<id>/ in
// sorted path order, each prefixed by its path relative to that directory and a NUL: what a
// bench.verified record certifies, so a corpus commit that touches one task re-verifies one.
func taskSHA(root, corpus, id string) (string, error) {
	bench := filepath.Join(root, "bench")
	record, err := os.ReadFile(filepath.Join(bench, "corpus", corpus, id, "task.json"))
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(record)
	checks := filepath.Join(bench, "checks", id)
	var files []string
	err = filepath.WalkDir(checks, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, filepath.ToSlash(rel(checks, p)))
		}
		return err
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(checks, filepath.FromSlash(f)))
		if err != nil {
			return "", err
		}
		h.Write([]byte(f + "\x00"))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// visibleKey is what makes two visible runs the same run.
type visibleKey struct{ repository, sha, setup, check string }

// verifyVisible is pass 2: each distinct visible run of the tasks pass 1 left unrefused, in
// corpus order, one at a time, its reading and logs given to every task holding its key.
func verifyVisible(ctx context.Context, m *Manifest, results []Result, suites []*visibleSuite) {
	type holder struct {
		i     int
		phase string
	}
	var keys []visibleKey
	holders := map[visibleKey][]holder{}
	for i, vs := range suites {
		if vs == nil {
			continue
		}
		for _, h := range []struct{ phase, sha string }{{"base", vs.task.BaseSHA}, {"landed", vs.task.LandedSHA}} {
			k := visibleKey{vs.task.Repository, h.sha, vs.task.Setup, vs.task.VisibleCheck}
			if _, ok := holders[k]; !ok {
				keys = append(keys, k)
			}
			holders[k] = append(holders[k], holder{i, h.phase})
		}
	}
	for _, k := range keys {
		hs := holders[k]
		first := suites[hs[0].i]
		o := runVisible(ctx, m, first.mirror, first.work, hs[0].phase, k)
		for _, h := range hs {
			work := suites[h.i].work
			if h != hs[0] {
				copyVisibleLogs(first.work, hs[0].phase, work, h.phase)
			}
			r := &results[h.i]
			if h.phase == "base" {
				r.VisibleBase = o.run
			} else {
				r.VisibleLanded = o.run
			}
			if why := o.refusal(k, h.phase, work); why != "" {
				r.Refusals = append(r.Refusals, why)
			}
		}
	}
}

// visibleOutcome is one key's reading and what, if anything, refuses it.
type visibleOutcome struct {
	run VisibleRun
	// failure is "" for a pass, or one of clone, setup, zero, red, flaky, killed.
	failure string
	detail  string // the clone error, or the setup log's suffix
	setup   int    // the setup exit code
}

func visibleLog(work, phase, suffix string) string {
	return filepath.Join(work, "visible-"+phase+suffix+".log")
}

var visibleLogSuffixes = []string{"", "-setup", "-rerun", "-rerun-setup"}

func copyVisibleLogs(fromWork, fromPhase, toWork, toPhase string) {
	for _, suffix := range visibleLogSuffixes {
		if b, err := os.ReadFile(visibleLog(fromWork, fromPhase, suffix)); err == nil {
			_ = os.WriteFile(visibleLog(toWork, toPhase, suffix), b, 0o644)
		}
	}
}

func (o visibleOutcome) refusal(k visibleKey, phase, work string) string {
	check := k.check
	suite, rerun := visibleLog(work, phase, ""), visibleLog(work, phase, "-rerun")
	switch o.failure {
	case "clone":
		return fmt.Sprintf("criterion 1: the visible checkout at %s_sha: %s", phase, o.detail)
	case "setup":
		return fmt.Sprintf("criterion 1: setup `%s` exited %d for the visible check at %s_sha (log %s)", k.setup, o.setup, phase, visibleLog(work, phase, o.detail))
	case "zero":
		return fmt.Sprintf("criterion 1: the visible check `%s` exits 0 at %s_sha over 0 tests (K7) (log %s)", check, phase, suite)
	case "killed":
		return fmt.Sprintf("criterion 1: the visible check `%s` at %s_sha was killed after %s (log %s)", check, phase, visibleTimeout, suite)
	case "red":
		return fmt.Sprintf("criterion 1: the visible check `%s` is red at %s_sha in two runs alone (exit %d, %d; logs %s %s)", check, phase, o.run.Exit, o.run.Rerun.Exit, suite, rerun)
	case "flaky":
		return fmt.Sprintf("criterion 1: the visible check `%s` is not deterministic at %s_sha: red then green in two runs alone (logs %s %s)", check, phase, suite, rerun)
	}
	return ""
}

// runVisible runs one key's suite and, on a red that is not a kill, runs it once more in a
// fresh clone to name the refusal deterministic or flaky; the second run never excuses it.
func runVisible(ctx context.Context, m *Manifest, mirror, work, phase string, k visibleKey) visibleOutcome {
	once := func(suffix string) (VisibleRun, *visibleOutcome) {
		v := VisibleRun{Exit: notRun}
		co, err := newClone(ctx, mirror, filepath.Join(work, "visible-"+phase+suffix), k.sha)
		if err != nil {
			return v, &visibleOutcome{run: v, failure: "clone", detail: err.Error()}
		}
		defer co.remove()
		if strings.TrimSpace(k.setup) != "" {
			setupSuffix := suffix + "-setup"
			if rc := co.sh(ctx, k.setup, nil, setupTimeout, visibleLog(work, phase, setupSuffix)); rc != 0 {
				return v, &visibleOutcome{run: v, failure: "setup", detail: setupSuffix, setup: rc}
			}
		}
		log := visibleLog(work, phase, suffix)
		rc, obs := co.run(ctx, k.check, nil, visibleTimeout, log)
		return VisibleRun{Exit: rc, Examined: countFrom(m.visibleRe, log), WallMS: obs.WallMS, CPUMS: obs.CPUMS, LoadAtStart: obs.LoadAtStart}, nil
	}
	first, failed := once("")
	if failed != nil {
		return *failed
	}
	o := visibleOutcome{run: first}
	switch {
	case first.Exit == 0 && first.Examined <= 0:
		o.failure = "zero"
	case first.Exit == 124:
		o.failure = "killed"
	case first.Exit != 0:
		second, failed := once("-rerun")
		if failed != nil {
			rerun := failed.run
			failed.run = first
			failed.run.Rerun = &rerun
			return *failed
		}
		o.run.Rerun = &second
		o.failure = "flaky"
		if second.Exit != 0 {
			o.failure = "red"
		}
	}
	return o
}

// countFrom reads the last count re's first group finds in a log, or 0.
func countFrom(re *regexp.Regexp, log string) int {
	b, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	all := re.FindAllSubmatch(b, -1)
	if len(all) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(string(all[len(all)-1][1]))
	return n
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
	// One buffer each: both processes write their stderr from their own copying goroutine.
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
	rc, _ := c.run(ctx, command, env, timeout, log)
	return rc
}

// run is sh that also returns the command's wall time, its CPU time and the load average
// read immediately before it starts.
func (c *checkout) run(ctx context.Context, command string, env []string, timeout time.Duration, log string) (int, Observed) {
	var obs Observed
	ctx, cancel := withWallTimeout(ctx, timeout)
	defer cancel()
	f, err := os.Create(log)
	if err != nil {
		return notRun, obs
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = c.dir
	cmd.Env = mergeEnv(environ(), append([]string{"CI=true"}, env...))
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	obs.LoadAtStart = readLoadAverage()
	start := time.Now()
	err = cmd.Run()
	obs.WallMS = time.Since(start).Milliseconds()
	// Reap anything the command left running in its group.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			obs.CPUMS = (ru.Utime.Nano() + ru.Stime.Nano()) / int64(time.Millisecond)
		}
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, obs
	case ctx.Err() != nil:
		fmt.Fprintf(f, "\nkilled after %s\n", timeout)
		return 124, obs
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode(), obs
	default:
		fmt.Fprintf(f, "\n%v\n", err)
		return 125, obs
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

// visibleCode is a visible run's <exit>/<examined>, or -/- when it did not run.
func visibleCode(v VisibleRun) string {
	if v.Exit == notRun {
		return "-/-"
	}
	return fmt.Sprintf("%d/%d", v.Exit, v.Examined)
}

func printResult(w io.Writer, r Result, logs string) {
	verdict := "ok"
	if len(r.Refusals) > 0 {
		verdict = "REFUSED"
	}
	additions := code(r.Additions)
	if r.AdditionsSameAsBase {
		additions = "same-as-base"
	}
	fmt.Fprintf(w, "task=%s class=%s base=%s landed=%s additions=%s visible_at_base=%s visible_at_landed=%s examined=%d verdict=%s logs=%s\n",
		r.ID, r.Class, code(r.Base), code(r.Landed), additions, visibleCode(r.VisibleBase), visibleCode(r.VisibleLanded), r.Examined, verdict, logs)
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
	fmt.Fprintf(w, "examined=%d ok=%d refused=%d %s scope=%s corpus_sha=%s%s\n",
		s.Examined, s.OK, s.Refused, strings.Join(classes, " "), scope, s.CorpusSHA, dirty)
	for _, p := range s.Problems {
		fmt.Fprintf(w, "refused: %s\n", p)
	}
}
