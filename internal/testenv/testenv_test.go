package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// initHome is read before TestMain redirects anything, which is how real code
// leaks: a package-level variable captured at init still names the real home.
var initHome = os.Getenv("HOME")

// plant is the environment variable that arms the planted tests below; they
// only run inside the subprocess the witness tests start.
const plant = "FOLIOT_TESTENV_PLANT"

func TestPlantedLeak(t *testing.T) {
	switch os.Getenv(plant) {
	case "real-root":
		Isolate(t)
		if err := os.WriteFile(filepath.Join(initHome, "planted"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	case "real-tmpdir":
		Isolate(t)
		if err := os.WriteFile(filepath.Join(initTmp, "planted"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	case "not-isolated":
		if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), "planted"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	case "":
		t.Skip("armed only by the witness tests")
	}
}

// runPlanted runs TestPlantedLeak in a subprocess whose "real" roots are
// directories inside this test's own isolation, so the red case touches nothing
// real either.
func runPlanted(t *testing.T, mode string, env ...string) (string, int) {
	base := Isolate(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestPlantedLeak$", "-test.v")
	cmd.Env = append(append(os.Environ(), plant+"="+mode), env...) // exec keeps the last duplicate
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "root="+filepath.Join(base, "home")+" ") {
		t.Fatalf("the subprocess did not read the fake home as its real root:\n%s", out)
	}
	return string(out), code
}

var witnessLine = regexp.MustCompile(`(?m)^FOLIOT_TEST_WITNESS var=(\S+) root=\S+ before=\d+ after=\d+ delta=(-?\d+)$`)

func TestWitnessGreenWhenNothingLeaks(t *testing.T) {
	out, code := runPlanted(t, "")
	lines := witnessLine.FindAllStringSubmatch(out, -1)
	if code != 0 || len(lines) != len(vars) {
		t.Fatalf("exit %d with %d witness lines, want exit 0 and %d:\n%s", code, len(lines), len(vars), out)
	}
	for _, l := range lines {
		if l[2] != "0" {
			t.Fatalf("var=%s delta=%s, want 0:\n%s", l[1], l[2], out)
		}
	}
}

// On a GitHub runner, systemd removed its root-owned systemd-private-* directory
// from the shared /tmp mid-run and the witness failed a passing package. A test
// can only create or remove entries its own user owns, so only those count;
// /usr stands in for a root holding nothing but another user's entries.
func TestWitnessIgnoresEntriesAnotherUserOwns(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root every entry is the test's own")
	}
	out, code := runPlanted(t, "", "CODEX_HOME=/usr")
	if code != 0 || !regexp.MustCompile(`var=CODEX_HOME root=/usr before=0 after=0 delta=0\n`).MatchString(out) {
		t.Fatalf("exit %d, want 0 with /usr's root-owned entries uncounted:\n%s", code, out)
	}
}

var initTmp = os.TempDir()

// The tests run in the re-executed binary, whose TMPDIR from init onwards is a
// directory holding nothing but this package's own entries.
func TestTestsRunUnderAPrivateTmpdir(t *testing.T) {
	Isolate(t)
	if os.Getenv(privateEnv) == "" || !strings.HasPrefix(filepath.Base(initTmp), prefix+"tmpdir-") {
		t.Fatalf("TMPDIR at init was %q with %s=%q, want a re-exec under a %stmpdir-* directory", initTmp, privateEnv, os.Getenv(privateEnv), prefix)
	}
	des, err := os.ReadDir(initTmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), prefix) {
			t.Errorf("the private TMPDIR holds %q, which this package did not create", de.Name())
		}
	}
}

func TestWitnessRedOnWriteUnderRealRoot(t *testing.T) {
	out, code := runPlanted(t, "real-root")
	if code != 1 || !regexp.MustCompile(`var=HOME root=\S+ before=\d+ after=\d+ delta=1\n`).MatchString(out) ||
		!strings.Contains(out, `var=HOME added=["planted"] removed=[]`) {
		t.Fatalf("exit %d, want 1 with HOME delta=1 naming the planted entry:\n%s", code, out)
	}
	// The run's own tests passed; only the witness turned it red.
	if !strings.Contains(out, "--- PASS: TestPlantedLeak") {
		t.Fatalf("the planted test itself should pass:\n%s", out)
	}
}

func TestWitnessRedOnWriteUnderTheTmpdirCapturedAtInit(t *testing.T) {
	out, code := runPlanted(t, "real-tmpdir")
	if code != 1 || !strings.Contains(out, `var=TMPDIR added=["planted"] removed=[]`) {
		t.Fatalf("exit %d, want 1 naming the planted TMPDIR entry:\n%s", code, out)
	}
}

func TestWitnessRedOnTestThatSkippedIsolate(t *testing.T) {
	out, code := runPlanted(t, "not-isolated")
	if code != 1 || !strings.Contains(out, `var=HOME not isolated, left=["planted"]`) {
		t.Fatalf("exit %d, want 1 naming the unisolated write:\n%s", code, out)
	}
}

func TestIsolatePointsEveryVariableIntoItsOwnDirectory(t *testing.T) {
	outer := map[string]string{}
	for _, v := range vars {
		outer[v.name] = os.Getenv(v.name)
	}
	var seen string
	t.Run("inner", func(t *testing.T) {
		seen = Isolate(t)
		for _, v := range vars {
			if got, want := os.Getenv(v.name), filepath.Join(seen, v.dir); got != want {
				t.Errorf("%s=%q, want %q", v.name, got, want)
			}
			if fi, err := os.Stat(os.Getenv(v.name)); err != nil || !fi.IsDir() {
				t.Errorf("%s does not name a directory: %v", v.name, err)
			}
		}
	})
	for _, v := range vars {
		if got := os.Getenv(v.name); got != outer[v.name] {
			t.Errorf("%s=%q after the test, want it restored to %q", v.name, got, outer[v.name])
		}
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("the test's directory %s survived the test: %v", seen, err)
	}
}
