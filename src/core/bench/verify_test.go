package bench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abhijeet34/foliot/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// fixture is a source repository with one merged pull request fixing add(), and a <root>
// whose corpus holds one task drawn from it.
type fixture struct {
	root, src    string
	base, landed string
	task         Task
	check        string
}

const goodCheck = `. ./lib.sh
[ "$(add 2 3)" = 5 ] || { echo "add 2 3 is $(add 2 3)"; exit 1; }
echo examined=1
`

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	testenv.Isolate(t)
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@invalid", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@invalid",
	} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	f := &fixture{root: filepath.Join(dir, "root"), src: filepath.Join(dir, "calc")}
	git(t, dir, "init", "--quiet", "--initial-branch=main", f.src)
	write(t, filepath.Join(f.src, "lib.sh"), "add() { echo $(($1 - $2)); }\n")
	// The visible suite reads the git index, as a real repository's suite may.
	write(t, filepath.Join(f.src, "test.sh"), ". ./lib.sh\n[ \"$(add 2 0)\" = 2 ]\ngit ls-files --error-unmatch lib.sh >/dev/null\necho '# tests 1'\n")
	git(t, f.src, "add", ".")
	git(t, f.src, "commit", "--quiet", "-m", "start")
	f.base = git(t, f.src, "rev-parse", "HEAD")
	write(t, filepath.Join(f.src, "lib.sh"), "add() { echo $(($1 + $2)); }\n")
	write(t, filepath.Join(f.src, "notes/added.txt"), "the fix\n")
	git(t, f.src, "add", ".")
	git(t, f.src, "commit", "--quiet", "-m", "fix: add subtracts (#7)")
	f.landed = git(t, f.src, "rev-parse", "HEAD")

	pr := "https://github.com/example/calc/pull/7"
	request := "add 2 3 prints -1; it should print the sum."
	sum := sha256.Sum256([]byte(request))
	f.task = Task{
		ID: "calc-add", Class: "defect", Request: request, RequestSHA256: hex.EncodeToString(sum[:]),
		Repository: f.src, BaseSHA: f.base, LandedSHA: f.landed, PullRequest: pr,
		VisibleCheck: "sh test.sh", Scope: Scope{Files: []string{"lib.sh"}},
		History: History{Source: "archive.md", Item: "calc-add", Text: "- [x] calc-add - add subtracts " + pr},
	}
	f.check = goodCheck
	return f
}

// save writes the corpus and commits it, so the corpus sha pins what is verified.
func (f *fixture) save(t *testing.T, manifest Manifest) {
	t.Helper()
	b := filepath.Join(f.root, "bench")
	task, _ := json.MarshalIndent(f.task, "", " ")
	man, _ := json.MarshalIndent(manifest, "", " ")
	write(t, filepath.Join(b, "corpus", "v1", "corpus.json"), string(man))
	write(t, filepath.Join(b, "corpus", "v1", f.task.ID, "task.json"), string(task))
	write(t, filepath.Join(b, "checks", f.task.ID, "check.sh"), f.check)
	write(t, filepath.Join(b, ".gitignore"), "repos/\nverify/\n")
	git(t, b, "init", "--quiet")
	git(t, b, "add", ".")
	git(t, b, "commit", "--quiet", "--allow-empty", "-m", "corpus")
}

func defaultManifest() Manifest {
	return Manifest{
		Classes:             map[string]int{"defect": 1, "feature": 0, "refactor": 0},
		Refusals:            []Marker{{What: "a held decision", Pattern: `(?i)held for a decision`}},
		VisibleExaminedFrom: `(?m)^(?:ℹ|#) tests ([0-9]+)$`,
	}
}

func (f *fixture) verify(t *testing.T) (Summary, Result, string) {
	t.Helper()
	var out bytes.Buffer
	s, err := Verify(context.Background(), Options{Root: f.root, Corpus: "v1", Jobs: 1, Out: &out})
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, out.String())
	}
	if len(s.Results) != 1 {
		t.Fatalf("want 1 result, got %d\n%s", len(s.Results), out.String())
	}
	return s, s.Results[0], out.String()
}

func TestVerifyCertifiesARedThenGreenTask(t *testing.T) {
	f := newFixture(t)
	f.save(t, defaultManifest())
	s, r, out := f.verify(t)
	if !s.Success() || s.Examined != 1 || s.Classes["defect"] != 1 {
		t.Fatalf("want success over 1 defect, got %+v\n%s", s, out)
	}
	if r.Base == 0 || r.Landed != 0 || r.Additions == 0 || r.Examined != 1 ||
		r.VisibleBase.Exit != 0 || r.VisibleBase.Examined != 1 || r.VisibleLanded.Exit != 0 || r.VisibleLanded.Examined != 1 {
		t.Fatalf("want base red, landed green, additions red, visible green over 1 test at base and landed, examined 1; got %+v", r)
	}
	for _, want := range []string{"task=calc-add class=defect base=1 landed=0 additions=1 visible_at_base=0/1 visible_at_landed=0/1 examined=1 verdict=ok", "examined=1 ok=1 refused=0 defect=1 feature=0 refactor=0 scope=full corpus_sha="} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "bench", "verify", "calc-add", "landed")); !os.IsNotExist(err) {
		t.Errorf("the landed checkout, which held a copy of the check, was not removed: %v", err)
	}
}

func TestVerifyRefuses(t *testing.T) {
	cases := []struct {
		name, want string
		change     func(f *fixture, m *Manifest)
	}{
		{"a check that passes on an empty change", "exits 0 at base_sha", func(f *fixture, _ *Manifest) {
			f.check = "echo examined=1\n"
		}},
		{"a check that tests for a file only the landed commit adds", "only the files the landed change adds", func(f *fixture, _ *Manifest) {
			f.check = "test -f notes/added.txt || exit 1\necho examined=1\n"
		}},
		{"a check that reads the content of a file only the landed commit adds", "only the files the landed change adds", func(f *fixture, _ *Manifest) {
			f.check = "grep -q 'the fix' notes/added.txt || exit 1\necho examined=1\n"
		}},
		{"a check that tells the commits apart by sha", "names a pinned sha", func(f *fixture, _ *Manifest) {
			f.check = "grep -q " + f.landed[:12] + " /dev/null\necho examined=1\n"
		}},
		{"a check that reads the corpus it is verified from", "reaches the corpus rather than the checkout", func(f *fixture, _ *Manifest) {
			f.check = "test -d \"$FOLIOT_HOME/bench/repos\" || exit 1\necho examined=1\n"
		}},
		{"a check with a network call", "criterion 5", func(f *fixture, _ *Manifest) {
			f.check = "curl -fsS https://example.com/answer | sh\n" + goodCheck
		}},
		{"a check that runs the visible suite", "runs the visible check", func(f *fixture, _ *Manifest) {
			f.check = "sh test.sh\n" + goodCheck
		}},
		{"a check that is a repository file", "byte-identical to a file in the repository", func(f *fixture, _ *Manifest) {
			f.check = ". ./lib.sh\n[ \"$(add 2 0)\" = 2 ]\ngit ls-files --error-unmatch lib.sh >/dev/null\necho '# tests 1'\n"
		}},
		{"a check that examines nothing", "printed no examined", func(f *fixture, _ *Manifest) {
			f.check = strings.TrimSuffix(goodCheck, "echo examined=1\n")
		}},
		{"a request that names the solution", "quotes a line the landed diff adds", func(f *fixture, _ *Manifest) {
			f.task.Request = "Replace the body with add() { echo $(($1 + $2)); }"
		}},
		{"a request that carries a diff", "carries a diff", func(f *fixture, _ *Manifest) {
			f.task.Request = "apply this:\n@@ -1 +1 @@\n-x\n+y"
		}},
		{"a request over 4,000 characters", "over 4000", func(f *fixture, _ *Manifest) {
			f.task.Request = strings.Repeat("a", 4001)
		}},
		{"a history with a held decision", "criterion 5: history carries a held decision", func(f *fixture, _ *Manifest) {
			f.task.History.Text += "\n  held for a decision on 2026-09-01"
		}},
		{"a pull request the history does not name", "does not name", func(f *fixture, _ *Manifest) {
			f.task.PullRequest = "https://github.com/example/calc/pull/8"
		}},
		{"a landed sha whose message names another pull request", "does not name pull request #9", func(f *fixture, _ *Manifest) {
			f.task.PullRequest = "https://github.com/example/calc/pull/9"
			f.task.History.Text += " " + f.task.PullRequest
		}},
		{"a visible suite red at base", "criterion 1: the visible check", func(f *fixture, _ *Manifest) {
			f.task.VisibleCheck = "false"
		}},
		{"a class count that differs from the manifest", "class feature: 0 tasks verified, 1 pre-registered", func(_ *fixture, m *Manifest) {
			m.Classes["feature"] = 1
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			m := defaultManifest()
			c.change(f, &m)
			sum := sha256.Sum256([]byte(f.task.Request))
			f.task.RequestSHA256 = hex.EncodeToString(sum[:])
			f.save(t, m)
			var out bytes.Buffer
			s, err := Verify(context.Background(), Options{Root: f.root, Corpus: "v1", Jobs: 1, Out: &out})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if s.Success() {
				t.Fatalf("verify certified it:\n%s", out.String())
			}
			if !strings.Contains(out.String(), c.want) {
				t.Fatalf("output lacks %q:\n%s", c.want, out.String())
			}
		})
	}
}

func TestVerifyClosesTheHostsProxyForAHiddenCheck(t *testing.T) {
	f := newFixture(t)
	f.check = `want="http:""//127.0.0.1:9"
[ "$HTTP_PROXY" = "$want" ] || { echo "HTTP_PROXY leaked: $HTTP_PROXY"; exit 1; }
[ "$HTTPS_PROXY" = "$want" ] || { echo "HTTPS_PROXY leaked: $HTTPS_PROXY"; exit 1; }
` + goodCheck
	f.save(t, defaultManifest())
	t.Setenv("HTTP_PROXY", "http://real-proxy.example:9999")
	t.Setenv("HTTPS_PROXY", "http://real-proxy.example:9999")
	s, r, out := f.verify(t)
	if !s.Success() || r.Landed != 0 {
		t.Fatalf("want the hidden check to see the closed proxy despite the host's own HTTP_PROXY, got %+v\n%s", r, out)
	}
}

func TestVerifyRefusesAnUncommittedCorpus(t *testing.T) {
	f := newFixture(t)
	f.save(t, defaultManifest())
	write(t, filepath.Join(f.root, "bench", "checks", "calc-add", "check.sh"), goodCheck+"# edited\n")
	s, _, out := f.verify(t)
	if s.Success() || !strings.Contains(out, "+uncommitted") {
		t.Fatalf("want an uncommitted corpus refused:\n%s", out)
	}
}

func TestVerifyRefusesZeroTasks(t *testing.T) {
	f := newFixture(t)
	f.save(t, Manifest{Classes: map[string]int{}, Refusals: defaultManifest().Refusals, VisibleExaminedFrom: defaultManifest().VisibleExaminedFrom})
	if err := os.RemoveAll(filepath.Join(f.root, "bench", "corpus", "v1", "calc-add")); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Join(f.root, "bench"), "commit", "--quiet", "-am", "empty")
	var out bytes.Buffer
	s, err := Verify(context.Background(), Options{Root: f.root, Corpus: "v1", Jobs: 1, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if s.Success() || !strings.Contains(out.String(), "examined=0 is never a pass") {
		t.Fatalf("want examined=0 refused:\n%s", out.String())
	}
}

func TestLoadManifestRefusesNoHistoryMarkers(t *testing.T) {
	testenv.Isolate(t)
	markers := `"history_refusals": [{"what": "a hold", "pattern": "hold"}]`
	for name, c := range map[string]struct{ manifest, want string }{
		"no history markers":                       {`{"classes": {"defect": 1}, "history_refusals": [], "visible_examined_from": "tests ([0-9]+)"}`, "examine nothing"},
		"no visible count expression":              {`{"classes": {"defect": 1}, ` + markers + `}`, "visible_examined_from"},
		"a count expression that does not compile": {`{"classes": {"defect": 1}, ` + markers + `, "visible_examined_from": "tests ([0-9]+"}`, "does not compile"},
		"a count expression with no group":         {`{"classes": {"defect": 1}, ` + markers + `, "visible_examined_from": "tests [0-9]+"}`, "no group"},
	} {
		path := filepath.Join(t.TempDir(), "corpus.json")
		write(t, path, c.manifest)
		if _, err := LoadManifest(path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want a refusal containing %q, got %v", name, c.want, err)
		}
	}
}

// addMulTask commits a third change, mul(), on top of the fixture's landed commit and adds
// a feature task drawn from it: its base_sha is the first task's landed_sha. It returns the
// manifest counting both tasks; call save after it.
func (f *fixture) addMulTask(t *testing.T) (Task, string, Manifest) {
	t.Helper()
	write(t, filepath.Join(f.src, "lib.sh"), "add() { echo $(($1 + $2)); }\nmul() { echo $(($1 * $2)); }\n")
	git(t, f.src, "add", ".")
	git(t, f.src, "commit", "--quiet", "-m", "feat: mul (#8)")
	pr := "https://github.com/example/calc/pull/8"
	request := "calc needs mul, which prints the product of its two arguments."
	sum := sha256.Sum256([]byte(request))
	task := f.task
	task.ID, task.Class, task.Request, task.RequestSHA256 = "calc-mul", "feature", request, hex.EncodeToString(sum[:])
	task.BaseSHA, task.LandedSHA, task.PullRequest = f.landed, git(t, f.src, "rev-parse", "HEAD"), pr
	task.History = History{Source: "archive.md", Item: "calc-mul", Text: "- [x] calc-mul - add mul " + pr}
	m := defaultManifest()
	m.Classes["feature"] = 1
	return task, ". ./lib.sh\n[ \"$(mul 2 3)\" = 6 ] || exit 1\necho examined=1\n", m
}

// saveTask writes one more task and its check into the corpus and commits it.
func (f *fixture) saveTask(t *testing.T, task Task, check string) {
	t.Helper()
	b := filepath.Join(f.root, "bench")
	raw, _ := json.MarshalIndent(task, "", " ")
	write(t, filepath.Join(b, "corpus", "v1", task.ID, "task.json"), string(raw))
	write(t, filepath.Join(b, "checks", task.ID, "check.sh"), check)
	git(t, b, "add", ".")
	git(t, b, "commit", "--quiet", "-m", "corpus: "+task.ID)
}

func verifyAll(t *testing.T, root string, jobs int) (Summary, string) {
	t.Helper()
	var out bytes.Buffer
	s, err := Verify(context.Background(), Options{Root: root, Corpus: "v1", Jobs: jobs, Out: &out})
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, out.String())
	}
	return s, out.String()
}

// runs counts the lines a visible check appended to a file named for the commit it ran at.
func runs(t *testing.T, dir, sha string) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, sha))
	return bytes.Count(b, []byte("run\n"))
}

// counting prefixes a visible check with a line that records one run at the checkout's commit.
func counting(dir, check string) string {
	return `echo run >> "` + dir + `/$(git rev-parse HEAD)"; ` + check
}

func TestVerifyRunsVisibleSuitesOneAtATime(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "running")
	f.task.VisibleCheck = `test ! -e "` + marker + `" || { echo "another visible suite is running"; exit 1; }; touch "` + marker + `"; sleep 2; rm "` + marker + `"; sh test.sh`
	mul, check, m := f.addMulTask(t)
	mul.VisibleCheck = f.task.VisibleCheck
	f.save(t, m)
	f.saveTask(t, mul, check)
	s, out := verifyAll(t, f.root, 2)
	if !s.Success() || s.OK != 2 {
		t.Fatalf("want both tasks certified with two jobs, got %+v\n%s", s, out)
	}
}

func TestVerifyRefusesAVisibleSuiteGreenOverZeroTests(t *testing.T) {
	f := newFixture(t)
	f.task.VisibleCheck = "echo '# tests 0'"
	f.save(t, defaultManifest())
	s, r, out := f.verify(t)
	if s.Success() || !strings.Contains(out, "exits 0 at base_sha over 0 tests (K7)") {
		t.Fatalf("want a green over zero tests refused:\n%s", out)
	}
	if r.VisibleBase.Exit != 0 || r.VisibleBase.Examined != 0 {
		t.Fatalf("want the reading kept, got %+v", r.VisibleBase)
	}
}

func TestVerifyRefusesAVisibleSuiteRedAtLanded(t *testing.T) {
	f := newFixture(t)
	f.task.VisibleCheck = `. ./lib.sh; [ "$(add 2 1)" = 1 ] || { echo "add 2 1 is $(add 2 1)"; exit 1; }; echo '# tests 1'`
	f.save(t, defaultManifest())
	s, r, out := f.verify(t)
	want := "criterion 1: the visible check `" + f.task.VisibleCheck + "` is red at landed_sha in two runs alone (exit 1, 1; logs "
	if s.Success() || !strings.Contains(out, want) || strings.Contains(out, "at base_sha") {
		t.Fatalf("want a refusal at landed_sha only, containing %q:\n%s", want, out)
	}
	if r.VisibleBase.Exit != 0 || r.VisibleLanded.Exit != 1 || r.VisibleLanded.Rerun == nil || r.VisibleLanded.Rerun.Exit != 1 {
		t.Fatalf("want green at base and both landed runs kept red, got base %+v landed %+v", r.VisibleBase, r.VisibleLanded)
	}
	for _, name := range []string{"visible-landed.log", "visible-landed-rerun.log", "visible-base.log"} {
		if _, err := os.Stat(filepath.Join(f.root, "bench", "verify", "calc-add", name)); err != nil {
			t.Errorf("log %s: %v", name, err)
		}
	}
}

func TestVerifyNamesAFlakyVisibleSuite(t *testing.T) {
	f := newFixture(t)
	counter := t.TempDir()
	// Red on the first run at the commit that holds notes/added.txt, green on every other.
	f.task.VisibleCheck = counting(counter, `if [ -e notes/added.txt ] && [ "$(grep -c run "`+counter+`/$(git rev-parse HEAD)")" = 1 ]; then exit 1; fi; sh test.sh`)
	f.save(t, defaultManifest())
	s, r, out := f.verify(t)
	want := "is not deterministic at landed_sha: red then green in two runs alone (logs "
	if s.Success() || !strings.Contains(out, want) {
		t.Fatalf("want the flake named, containing %q:\n%s", want, out)
	}
	if n := runs(t, counter, f.landed); n != 2 {
		t.Fatalf("the suite ran %d times at landed_sha, want 2", n)
	}
	if n := runs(t, counter, f.base); n != 1 {
		t.Fatalf("the suite ran %d times at base_sha, want 1", n)
	}
	if r.VisibleLanded.Exit != 1 || r.VisibleLanded.Rerun == nil || r.VisibleLanded.Rerun.Exit != 0 {
		t.Fatalf("want both landed readings kept, got %+v", r.VisibleLanded)
	}
}

func TestVerifyDedupesVisibleRoundsBySha(t *testing.T) {
	f := newFixture(t)
	counter := t.TempDir()
	f.task.VisibleCheck = counting(counter, "sh test.sh")
	mul, check, m := f.addMulTask(t)
	mul.VisibleCheck = f.task.VisibleCheck
	f.save(t, m)
	f.saveTask(t, mul, check)
	s, out := verifyAll(t, f.root, 2)
	if !s.Success() {
		t.Fatalf("want both tasks certified:\n%s", out)
	}
	entries, _ := os.ReadDir(counter)
	total := 0
	for _, e := range entries {
		total += runs(t, counter, e.Name())
	}
	if total != 3 || runs(t, counter, f.landed) != 1 {
		t.Fatalf("want 3 visible runs for 3 distinct shas, one at the shared sha; got %d (%d at the shared sha)", total, runs(t, counter, f.landed))
	}
	for _, id := range []string{"calc-add", "calc-mul"} {
		for _, name := range []string{"visible-base.log", "visible-landed.log"} {
			if _, err := os.Stat(filepath.Join(f.root, "bench", "verify", id, name)); err != nil {
				t.Errorf("%s: %v", id, err)
			}
		}
	}
}

func TestVerifyRefusesAHangingVisibleSuiteWithoutRerun(t *testing.T) {
	f := newFixture(t)
	counter := t.TempDir()
	f.task.VisibleCheck = counting(counter, "sleep 10")
	f.save(t, defaultManifest())
	old := visibleTimeout
	visibleTimeout = 2 * time.Second
	t.Cleanup(func() { visibleTimeout = old })
	s, r, out := f.verify(t)
	if s.Success() || !strings.Contains(out, "at base_sha was killed after 2s (log ") {
		t.Fatalf("want the hang refused:\n%s", out)
	}
	if r.VisibleBase.Exit != 124 || r.VisibleBase.Rerun != nil || runs(t, counter, f.base) != 1 {
		t.Fatalf("want one run killed with 124 and no re-run, got %+v after %d runs", r.VisibleBase, runs(t, counter, f.base))
	}
}
