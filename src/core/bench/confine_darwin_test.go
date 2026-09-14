package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestConfineCheckDeniesARealPathBehindATmpSymlink is the regression for a Seatbelt
// profile built from an unresolved deny path. On stock macOS /tmp is a symlink to
// /private/tmp, and Seatbelt checks the kernel's resolved path; a deny rule written
// from the unresolved /tmp/... path never matches a file reached through its real
// /private/tmp/... path, so a payload can read it straight through. Before RealPath
// this test failed (the secret leaked); after it, the deny rule matches and blocks it.
func TestConfineCheckDeniesARealPathBehindATmpSymlink(t *testing.T) {
	if _, ok := confineCheck("true", t.TempDir(), nil); !ok {
		t.Skip("no working check confinement on this platform or inside this sandbox")
	}
	secretDir, err := os.MkdirTemp("/tmp", "foliot-confine-secret-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(secretDir)
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(secretDir)
	if err != nil {
		t.Fatal(err)
	}
	if real == secretDir {
		t.Skip("/tmp is not a symlink on this machine")
	}

	checkout := t.TempDir()
	leak := filepath.Join(checkout, "leaked")
	// A worker's own tools resolve the path it reads; the deny list is given exactly the
	// unresolved /tmp/... path the runner supplies, as s.Root and DenyRead entries are.
	payload := "cat " + real + "/secret.txt > " + leak
	cmd, _ := confineCheck(payload, checkout, []string{secretDir})
	exec.Command("sh", "-c", cmd).Run()
	b, _ := os.ReadFile(leak)
	if len(b) > 0 {
		t.Fatalf("deny path %q (real %q) did not block a read through its resolved real path: leaked %q", secretDir, real, b)
	}
}
