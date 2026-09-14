package bench

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

const sandboxExec = "/usr/bin/sandbox-exec"

// confineCheck wraps the hidden check's command, which executes the worker's code, in a
// Seatbelt profile: it reads nothing under the denied paths but its own checkout and the
// toolchain paths named in allowed, writes only to its checkout, and reaches no host beyond
// this one. Without it, code a worker planted could copy every task's check to a path the
// next run's sandbox leaves readable, or read the operator's real home the way the worker's
// own run profile already denies it.
func confineCheck(command, checkout string, allowed, denied []string) (string, bool) {
	if !seatbeltWorks() {
		return command, false
	}
	checkout = RealPath(checkout)
	var p strings.Builder
	p.WriteString("(version 1)(allow default)(deny network*)(allow network* (remote unix-socket))(allow network* (remote ip \"localhost:*\"))")
	for _, d := range denied {
		fmt.Fprintf(&p, "(deny file-read* (subpath %s))", sbplString(RealPath(d)))
	}
	fmt.Fprintf(&p, "(allow file-read* (subpath %s))", sbplString(checkout))
	for _, a := range allowed {
		fmt.Fprintf(&p, "(allow file-read* (subpath %s))", sbplString(RealPath(a)))
	}
	// mktemp -d under sandbox-exec ignores TMPDIR and uses the per-user temp directory, so
	// writes there are allowed; every worker's run profile denies reading it back.
	fmt.Fprintf(&p, "(deny file-write*)(allow file-write* (subpath %s) (subpath %s) (literal \"/dev/null\") (literal \"/dev/tty\") (regex #\"^/dev/fd/\"))", sbplString(checkout), sbplString(userTemp()))
	return sandboxExec + " -p " + shellQuote(p.String()) + " sh -c " + shellQuote(command), true
}

func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// seatbeltWorks applies an empty profile once: inside another sandbox, sandbox-exec cannot
// apply one, and the verdict then records the check as unconfined rather than failing it.
// userTemp is the per-user temp directory with its symlinks resolved, as Seatbelt matches
// real paths.
var userTemp = sync.OnceValue(func() string {
	out, err := exec.Command("getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		return "/nonexistent"
	}
	return RealPath(strings.TrimSpace(string(out)))
})

var seatbeltWorks = sync.OnceValue(func() bool {
	return exec.Command(sandboxExec, "-p", "(version 1)(allow default)", "/usr/bin/true").Run() == nil
})
