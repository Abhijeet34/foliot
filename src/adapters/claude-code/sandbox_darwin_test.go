package claudecode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abhijeet34/foliot/internal/testenv"
	"github.com/Abhijeet34/foliot/src/core/bench"
)

// TestRunProfileGrantsWritesInsideARunUnderATmpRoot is the regression for a root beneath
// /tmp. Seatbelt, which is what the harness's sandbox is on macOS, matches the path the
// kernel resolves, so a write grant written from the unresolved /tmp/... spelling of the
// run directory (a symlink to /private/tmp/... on stock macOS) never matches the worker's
// own checkout and every write inside it fails with Operation not permitted, whatever the
// model does. This applies the rendered grant through sandbox-exec, so the assertion is the
// kernel's own verdict rather than the shape of a string.
func TestRunProfileGrantsWritesInsideARunUnderATmpRoot(t *testing.T) {
	testenv.Isolate(t)
	if exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/usr/bin/true").Run() != nil {
		t.Skip("sandbox-exec cannot apply a profile here")
	}
	root, err := os.MkdirTemp("/tmp", "foliot-tmp-root-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if bench.RealPath(root) == root {
		t.Skip("/tmp is not a symlink on this machine")
	}
	run := filepath.Join(root, "bench", "work", "t.arm.r1")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}

	fs := renderFilesystem(t, &bench.Isolation{Run: run, DenyRead: []string{root}, AllowRead: []string{run}})
	if len(fs.AllowWrite) == 0 {
		t.Fatal("no write grant rendered")
	}
	var p strings.Builder
	p.WriteString(`(version 1)(allow default)(deny file-write*)`)
	for _, w := range fs.AllowWrite {
		p.WriteString(`(allow file-write* (subpath "` + w + `"))`)
	}
	// The write the probe reported failing: a symlink and a git fetch inside the checkout.
	out, err := exec.Command("/usr/bin/sandbox-exec", "-p", p.String(), "sh", "-c",
		"ln -s ./check.sh "+filepath.Join(run, "probe-link")+" 2>&1").CombinedOutput()
	if err != nil {
		t.Fatalf("a worker could not write inside its own checkout under a /tmp root: %v: %s\ngrant %v, real run %s", err, out, fs.AllowWrite, bench.RealPath(run))
	}
}

// renderFilesystem launches with iso and reads back the filesystem block of the run profile.
func renderFilesystem(t *testing.T, iso *bench.Isolation) struct {
	DenyRead, AllowRead, AllowWrite, DenyWrite []string
} {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if _, err := (Adapter{Binary: "/bin/claude"}).Launch(bench.Launch{
		Home: home, Tmp: filepath.Join(home, "tmp"), Model: "claude-opus-5", CapUSD: 1, Prompt: "p", Credential: "tok", Isolation: iso,
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Sandbox struct {
			Filesystem struct {
				DenyRead, AllowRead, AllowWrite, DenyWrite []string
			}
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s.Sandbox.Filesystem
}
