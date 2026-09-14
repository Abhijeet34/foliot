// Package testenv holds the test runner's two rules (a5 section 2.17, p8 R76):
// every test runs with HOME, XDG_*, TMPDIR and CODEX_HOME pointed at its own
// temporary directory, and a TestMain witness fails the run when a real root's
// entries changed while the package's tests ran.
package testenv

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// prefix marks the directories this package creates under the real TMPDIR, so a
// package running concurrently under `go test ./...` is not read as a leak.
const prefix = "foliot-test-"

// vars maps each redirected variable to its subdirectory name, after a5 section
// 2.13's per-run layout.
var vars = []struct{ name, dir string }{
	{"HOME", "home"},
	{"XDG_CONFIG_HOME", "config"},
	{"XDG_STATE_HOME", "state"},
	{"XDG_CACHE_HOME", "cache"},
	{"TMPDIR", "tmp"},
	{"CODEX_HOME", "codex"},
}

// realRoots reads where each variable points on the machine, with the defaults
// the XDG base directory specification and Codex apply when one is unset.
func realRoots() (map[string]string, error) {
	home := os.Getenv("HOME")
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("HOME is %q, so the witness has no real root to read", home)
	}
	roots := map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"TMPDIR":          os.TempDir(),
		"CODEX_HOME":      filepath.Join(home, ".codex"),
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "CODEX_HOME"} {
		if v := os.Getenv(name); filepath.IsAbs(v) {
			roots[name] = v
		}
	}
	return roots, nil
}

// entries lists a root's top-level names; a root that does not exist has none.
func entries(dir string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), prefix) {
			names = append(names, de.Name())
		}
	}
	return names, nil
}

// Main runs a package's tests between two readings of every real root and returns
// the exit code: m.Run's, or 1 when any root gained or lost an entry. Before the
// tests run, every variable points into a package directory, so a test that forgot
// Isolate still writes nowhere real; Isolate gives each test its own.
func Main(m *testing.M) int { return run(m.Run, os.Stdout) }

func run(tests func() int, out io.Writer) int {
	roots, err := realRoots()
	if err != nil {
		fmt.Fprintln(out, "witness:", err)
		return 1
	}
	before := map[string][]string{}
	for _, v := range vars {
		if before[v.name], err = entries(roots[v.name]); err != nil {
			fmt.Fprintln(out, "witness:", err)
			return 1
		}
	}
	pkg, err := os.MkdirTemp(roots["TMPDIR"], prefix)
	if err != nil {
		fmt.Fprintln(out, "witness:", err)
		return 1
	}
	defer os.RemoveAll(pkg)
	for _, v := range vars {
		if err := redirect(pkg, v.name, v.dir, os.Setenv); err != nil {
			fmt.Fprintln(out, "witness:", err)
			return 1
		}
	}

	code := tests()

	for _, v := range vars {
		after, err := entries(roots[v.name])
		if err != nil {
			fmt.Fprintln(out, "witness:", err)
			return 1
		}
		added, removed := diff(before[v.name], after), diff(after, before[v.name])
		fmt.Fprintf(out, "FOLIOT_TEST_WITNESS var=%s root=%s before=%d after=%d delta=%d\n",
			v.name, roots[v.name], len(before[v.name]), len(after), len(after)-len(before[v.name]))
		if len(added)+len(removed) > 0 {
			fmt.Fprintf(out, "FOLIOT_TEST_WITNESS var=%s added=%q removed=%q\n", v.name, added, removed)
			code = 1
		}
		// The package directory is the second witness: t.TempDir cleans up after
		// itself, so anything left here came from a test that skipped Isolate.
		if left, _ := entries(filepath.Join(pkg, v.dir)); len(left) > 0 {
			fmt.Fprintf(out, "FOLIOT_TEST_WITNESS var=%s not isolated, left=%q\n", v.name, left)
			code = 1
		}
	}
	return code
}

func diff(a, b []string) []string {
	var out []string
	for _, x := range b {
		if !slices.Contains(a, x) {
			out = append(out, x)
		}
	}
	return out
}

func redirect(base, name, dir string, setenv func(string, string) error) error {
	p := filepath.Join(base, dir)
	if err := os.MkdirAll(p, 0o700); err != nil {
		return err
	}
	return setenv(name, p)
}

// Isolate points every redirected variable into a fresh directory for this test
// alone, restored when the test ends, and returns that directory.
func Isolate(t testing.TB) string {
	t.Helper()
	base := t.TempDir()
	for _, v := range vars {
		if err := redirect(base, v.name, v.dir, func(k, val string) error { t.Setenv(k, val); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return base
}
