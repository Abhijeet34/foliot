package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfineCheckDeniesARealPathBehindATmpSymlink is the regression for a Seatbelt
// profile built from an unresolved deny path. On stock macOS /tmp is a symlink to
// /private/tmp, and Seatbelt checks the kernel's resolved path; a deny rule written
// from the unresolved /tmp/... path never matches a file reached through its real
// /private/tmp/... path, so a payload can read it straight through. Before RealPath
// this test failed (the secret leaked); after it, the deny rule matches and blocks it.
func TestConfineCheckDeniesARealPathBehindATmpSymlink(t *testing.T) {
	if _, ok := confineCheck("true", t.TempDir(), nil, nil); !ok {
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
	cmd, _ := confineCheck(payload, checkout, nil, []string{secretDir})
	exec.Command("sh", "-c", cmd).Run()
	b, _ := os.ReadFile(leak)
	if len(b) > 0 {
		t.Fatalf("deny path %q (real %q) did not block a read through its resolved real path: leaked %q", secretDir, real, b)
	}
}

// TestConfineCheckDeniesTheRealHome is the regression for the check phase running the
// worker's own applied diff under a weaker sandbox than the worker itself: a deny list
// built without the real home (as runCheck's used to be) lets a planted check or setup
// script read an operator secret straight through; the same call with the real home in the
// deny list, as runCheck now builds it, blocks it, and a toolchain path named in allowed
// stays reachable even though it sits under that denied home.
func TestConfineCheckDeniesTheRealHome(t *testing.T) {
	if _, ok := confineCheck("true", t.TempDir(), nil, nil); !ok {
		t.Skip("no working check confinement on this platform or inside this sandbox")
	}
	realHome := t.TempDir()
	secret := filepath.Join(realHome, ".aws", "credentials")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(realHome, "toolchain", "bin")
	if err := os.MkdirAll(toolPath, 0o700); err != nil {
		t.Fatal(err)
	}
	toolFile := filepath.Join(toolPath, "node")
	if err := os.WriteFile(toolFile, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	checkout := t.TempDir()
	leak := filepath.Join(checkout, "leaked")
	payload := "cat " + secret + " > " + leak + " 2>/dev/null; cat " + toolFile + " >> " + leak + " 2>/dev/null"

	// RED: the deny list omits the real home, as runCheck's did before this fix, and the
	// secret reaches the check's own output.
	cmd, _ := confineCheck(payload, checkout, nil, nil)
	exec.Command("sh", "-c", cmd).Run()
	b, _ := os.ReadFile(leak)
	if !strings.Contains(string(b), "hunter2") {
		t.Fatalf("red case: expected the unconfined-by-home run to read the secret, got %q", b)
	}
	os.Remove(leak)

	// GREEN: the real home is in the deny list, as runCheck now builds it, and the
	// toolchain path is named in allowed so it stays readable despite sitting under it.
	cmd, _ = confineCheck(payload, checkout, []string{toolPath}, []string{realHome})
	exec.Command("sh", "-c", cmd).Run()
	b, _ = os.ReadFile(leak)
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("green case: the real home's secret reached the check despite being denied: %q", b)
	}
	if !strings.Contains(string(b), "#!/bin/sh") {
		t.Fatalf("green case: the allowed toolchain path under the real home was not readable: %q", b)
	}
}
