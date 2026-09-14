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
	write(t, filepath.Join(f.src, "test.sh"), ". ./lib.sh\n[ \"$(add 2 0)\" = 2 ]\ngit ls-files --error-unmatch lib.sh >/dev/null\n")
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
		Classes:  map[string]int{"defect": 1, "feature": 0, "refactor": 0},
		Refusals: []Marker{{What: "a held decision", Pattern: `(?i)held for a decision`}},
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
	if r.Base == 0 || r.Landed != 0 || r.Additions == 0 || r.Visible != 0 || r.Examined != 1 {
		t.Fatalf("want base red, landed green, additions red, visible green, examined 1; got %+v", r)
	}
	for _, want := range []string{"task=calc-add class=defect base=1 landed=0 additions=1 visible_at_base=0 examined=1 verdict=ok", "examined=1 ok=1 refused=0 defect=1 feature=0 refactor=0 scope=full corpus_sha="} {
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
			f.check = ". ./lib.sh\n[ \"$(add 2 0)\" = 2 ]\ngit ls-files --error-unmatch lib.sh >/dev/null\n"
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
	f.save(t, Manifest{Classes: map[string]int{}, Refusals: defaultManifest().Refusals})
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
	path := filepath.Join(t.TempDir(), "corpus.json")
	write(t, path, `{"classes": {"defect": 1}, "history_refusals": []}`)
	if _, err := LoadManifest(path); err == nil || !strings.Contains(err.Error(), "examine nothing") {
		t.Fatalf("want an empty marker list refused, got %v", err)
	}
}
