package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Abhijeet34/foliot/internal/testenv"
)

// The gate for AGENTS.md section 8: the table there and `commands` here must name
// the same families in the same order with the same builtness, and the count the
// section states must be the one the code holds. It enumerates every row rather
// than sampling, and a section that yields no rows is itself a problem, since a
// gate that examined nothing proves nothing.
const manual = "../../AGENTS.md"

var (
	countLine = regexp.MustCompile(`Built today: \d+ of \d+ command families; the other \d+ are planned\.`)
	tableRow  = regexp.MustCompile("^\\| `foliot ([^` ]+)[^|]*\\|[^|]*\\| (yes|planned) \\|")
	nextHead  = regexp.MustCompile(`(?m)^## `)
)

// indexProblems returns every disagreement between the manual's section 8 and the
// command index, and the number of rows it examined.
func indexProblems(section string) (problems []string, examined int) {
	var documented []string
	for _, line := range strings.Split(section, "\n") {
		if m := tableRow.FindStringSubmatch(line); m != nil {
			documented = append(documented, m[1]+" "+m[2])
		}
	}
	examined = len(documented)
	if examined == 0 {
		return []string{"section 8 yielded no table rows, so this check examined nothing"}, 0
	}

	var coded []string
	for _, c := range commands {
		built := "planned"
		if c.built() {
			built = "yes"
		}
		coded = append(coded, c.name+" "+built)
	}
	if got, want := strings.Join(documented, "\n  "), strings.Join(coded, "\n  "); got != want {
		problems = append(problems, fmt.Sprintf("the table and cmd/foliot/commands.go disagree.\nmanual:\n  %s\ncode:\n  %s", got, want))
	}

	want := fmt.Sprintf("Built today: %d of %d command families; the other %d are planned.",
		len(builtNames()), len(commands), len(commands)-len(builtNames()))
	switch got := countLine.FindString(section); got {
	case want:
	case "":
		problems = append(problems, fmt.Sprintf("section 8 states no count; it must state %q", want))
	default:
		problems = append(problems, fmt.Sprintf("section 8 states %q; the code holds %q", got, want))
	}
	return problems, examined
}

func commandIndexSection(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(manual)
	if err != nil {
		t.Fatal(err)
	}
	_, after, found := strings.Cut(string(b), "## 8. Command index\n")
	if !found {
		t.Fatalf("%s has no section 8", manual)
	}
	if end := nextHead.FindStringIndex(after); end != nil {
		after = after[:end[0]]
	}
	return after
}

func TestManualCommandIndexMatchesTheCode(t *testing.T) {
	testenv.Isolate(t)
	problems, examined := indexProblems(commandIndexSection(t))
	t.Logf("examined=%d rows of %s section 8", examined, manual)
	for _, p := range problems {
		t.Errorf("%s: %s", manual, p)
	}
}

// A gate that cannot go red guards nothing: every way section 8 can drift from the
// code must produce a problem, and a row that merely looks like one must not.
func TestManualCommandIndexGateGoesRed(t *testing.T) {
	testenv.Isolate(t)
	real := commandIndexSection(t)
	rowOf := func(name, built string) string {
		return "| `foliot " + name + " --now` | a command nobody built | " + built + " | **declared here** |\n"
	}
	for _, c := range []struct {
		name    string
		section string
		want    int
	}{
		{"the manual as it stands", real, 0},
		{"an empty section", "", 1},
		{"a table with no rows", strings.Join(strings.Split(real, "\n| `foliot ")[:1], ""), 1},
		{"a fabricated command marked built", real + rowOf("frobnicate", "yes"), 1},
		{"a fabricated command marked planned", real + rowOf("frobnicate", "planned"), 1},
		{"a built command demoted to planned", strings.Replace(real, "fold the log twice and compare | yes |", "fold the log twice and compare | planned |", 1), 1},
		{"two families swapped", strings.Replace(real, "| `foliot digest`", "| `foliot ledger`", 1), 1},
		{"a count that does not match", strings.Replace(real, "Built today: 2 of 19", "Built today: 3 of 19", 1), 1},
		{"a row outside the table", real + "Run `foliot status` to see the tasks.\n", 0},
	} {
		problems, examined := indexProblems(c.section)
		if len(problems) != c.want {
			t.Errorf("%s: %d problems, want %d (examined=%d): %s", c.name, len(problems), c.want, examined, strings.Join(problems, "; "))
		}
	}
}

func TestPlannedCommandsRefuseByNameAndBuiltOnesDoNot(t *testing.T) {
	testenv.Isolate(t)
	for _, c := range commands {
		code, out, errOut := foliot("/unused", c.name)
		switch {
		case c.built():
			if strings.Contains(errOut, "planned, not built") {
				t.Errorf("foliot %s is built but refused as planned: %q", c.name, errOut)
			}
		case code != 2 || out != "" ||
			!strings.HasPrefix(errOut, "foliot "+c.name+": planned, not built (AGENTS.md section 8)\n") ||
			!strings.Contains(errOut, "built today: foliot replay, foliot bench (2 of 19 command families)\n"):
			t.Errorf("foliot %s exited %d, stdout %q, stderr %q", c.name, code, out, errOut)
		}
	}
}
