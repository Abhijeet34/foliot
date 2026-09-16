#!/bin/sh
# Proves scripts/linux-lane.sh goes red: a lane that cannot fail guards nothing, and the whole
# point of this one is to fail on a change a workstation calls green. Each case is a scratch Go
# module the lane is pointed at, so the proof costs one small module per case and never the
# project's own suite. It runs on the workstation and on the Linux runner, and nothing in it may
# assume which of the two it is on; the one case whose answer depends on the host says so and
# asserts both answers.
set -eu

lane=$(cd "$(dirname "$0")" && pwd -P)/linux-lane.sh
scratch=$(mktemp -d)
trap '[ -n "${scratch:-}" ] && [ -d "$scratch" ] && rm -rf "$scratch"' EXIT
cases=0
failed=0

# module <name>: a fresh module with one test that passes everywhere
module() {
  mod=$scratch/$1
  mkdir -p "$mod"
  printf 'module example.test\n\ngo 1.27\n' > "$mod/go.mod"
  cat > "$mod/lane_test.go" <<'EOF'
package lane

import "testing"

func TestArithmeticHoldsOnEveryPlatform(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic")
	}
}
EOF
}

# expect <exit-code> <text-the-output-must-contain> <description>
expect() {
  cases=$((cases + 1))
  rc=0
  sh "$lane" "$mod" > "$scratch/out" 2>&1 || rc=$?
  if [ "$rc" -eq "$1" ] && grep -qF -- "$2" "$scratch/out"; then
    echo "ok   $3"
  else
    echo "FAIL $3: exit $rc (want $1), output must contain '$2':"
    sed 's/^/     /' "$scratch/out" | tail -30
    failed=$((failed + 1))
  fi
}

module clean
expect 0 'linux lane green' 'a module that passes on both platforms is green'

# The defect itself: a tree this host calls green that Linux calls red. The host leg is asserted
# first, because "the lane went red" only means something once the host's own verdict is known.
module linuxonly
cat >> "$mod/lane_test.go" <<'EOF'

// Stands in for the real thing: a test that reads a path or a facility only darwin has.
func TestPlatformAssumption(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Fatalf("this assumes darwin, and is running on %s", runtime.GOOS)
	}
}
EOF
sed -i.bak 's/^import "testing"$/import (\n\t"runtime"\n\t"testing"\n)/' "$mod/lane_test.go"
rm -f "$mod/lane_test.go.bak"
# What the host must say about this module is decided by what the host is, and asserting it
# either way is what proves the lane reads Linux rather than the machine it was started on. A
# darwin host calls it green, and that gap against the lane's red is the whole defect. A linux
# host calls it red for the same reason the lane does, so there is no gap to read there, and
# saying so is not the same as not checking: a darwin host that called it red, or a linux host
# that called it green, would mean this fixture had stopped standing for a platform assumption.
cases=$((cases + 1))
host_os=$(uname -s)
host_rc=0
(cd "$mod" && go test ./... > "$scratch/host.out" 2>&1) || host_rc=$?
if [ "$host_os" = Darwin ]; then
  want_rc=0
  want="green on this $host_os host, which is the gap the lane exists to close"
else
  want_rc=1
  want="red on this $host_os host too, so there is no gap here for the lane to read"
fi
if [ "$host_rc" -eq "$want_rc" ]; then
  echo "ok   the darwin-only module is $want"
else
  echo "FAIL the darwin-only module should be $want, but go test exited $host_rc:"
  sed 's/^/     /' "$scratch/host.out"
  failed=$((failed + 1))
fi
expect 1 'this tree is red on linux' 'a test that only passes on darwin is red in the lane'

# A build that only exists on darwin never reaches a test binary, so the cheap leg catches it.
module darwinbuild
cat > "$mod/only_darwin.go" <<'EOF'
package lane

func Platform() string { return "darwin" }
EOF
cat >> "$mod/lane_test.go" <<'EOF'

func TestPlatformIsNamed(t *testing.T) {
	if Platform() == "" {
		t.Fatal("unnamed")
	}
}
EOF
expect 1 'does not vet for linux' 'a package that only builds on darwin is red in the lane'

module notests
rm "$mod/lane_test.go"
cat > "$mod/lane.go" <<'GOEOF'
package lane

func Platform() string { return "any" }
GOEOF
expect 1 'ran 0 tests' 'a module with code but no tests is refused, never passed'

# A lane that reports green when it could not read Linux at all is the failure this whole file
# exists to prevent, so both ways of not having a runtime are their own exit code.
module noruntime
cases=$((cases + 1))
rc=0
FOLIOT_LINUX_LANE_DOCKER=$scratch/definitely-not-here sh "$lane" "$mod" > "$scratch/out" 2>&1 || rc=$?
if [ "$rc" -eq 2 ] && grep -qF 'is not on PATH' "$scratch/out"; then
  echo "ok   no container runtime is exit 2, never a pass"
else
  echo "FAIL no container runtime should be exit 2: exit $rc"
  sed 's/^/     /' "$scratch/out" | tail -10
  failed=$((failed + 1))
fi

printf '#!/bin/sh\nexit 1\n' > "$scratch/dead-docker"
chmod +x "$scratch/dead-docker"
cases=$((cases + 1))
rc=0
FOLIOT_LINUX_LANE_DOCKER=$scratch/dead-docker sh "$lane" "$mod" > "$scratch/out" 2>&1 || rc=$?
if [ "$rc" -eq 2 ] && grep -qF 'daemon does not answer' "$scratch/out"; then
  echo "ok   a runtime whose daemon is down is exit 2, never a pass"
else
  echo "FAIL a dead daemon should be exit 2: exit $rc"
  sed 's/^/     /' "$scratch/out" | tail -10
  failed=$((failed + 1))
fi

# A third way not to read Linux: a toolchain that cannot list the tree. It printed nothing and
# the lane called the tree red, which is the misreport this case pins (measured 2026-09-16, a
# sandbox denying go's default GOCACHE).
module unreadable
printf 'module example.test\n\ngo 1.27\n\nrequire nonsense\n' > "$mod/go.mod"
expect 2 'could not read' 'a toolchain that cannot list the tree is exit 2, never a red tree'

echo "examined=$cases cases, failed=$failed"
[ "$cases" -gt 0 ] && [ "$failed" -eq 0 ]
