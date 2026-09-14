package bench

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

const sandboxExec = "/usr/bin/sandbox-exec"

// confineCheck wraps the hidden check's command, which executes the worker's code, in a
// Seatbelt profile: it reads nothing under the denied paths but its own checkout, writes
// only there, and reaches no host beyond this one. Without it, code a worker planted could
// copy every task's check to a path the next run's sandbox leaves readable.
func confineCheck(command, checkout string, denied []string) (string, bool) {
	if !seatbeltWorks() {
		return command, false
	}
	var p strings.Builder
	p.WriteString("(version 1)(allow default)(deny network*)(allow network* (remote unix-socket))(allow network* (remote ip \"localhost:*\"))")
	for _, d := range denied {
		fmt.Fprintf(&p, "(deny file-read* (subpath %s))", sbplString(d))
	}
	fmt.Fprintf(&p, "(allow file-read* (subpath %s))", sbplString(checkout))
	fmt.Fprintf(&p, "(deny file-write*)(allow file-write* (subpath %s) (literal \"/dev/null\") (literal \"/dev/tty\") (regex #\"^/dev/fd/\"))", sbplString(checkout))
	return sandboxExec + " -p " + shellQuote(p.String()) + " sh -c " + shellQuote(command), true
}

func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// seatbeltWorks applies an empty profile once: inside another sandbox, sandbox-exec cannot
// apply one, and the verdict then records the check as unconfined rather than failing it.
var seatbeltWorks = sync.OnceValue(func() bool {
	return exec.Command(sandboxExec, "-p", "(version 1)(allow default)", "/usr/bin/true").Run() == nil
})
