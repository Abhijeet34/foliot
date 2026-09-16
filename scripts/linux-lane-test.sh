#!/bin/sh
# Proves scripts/linux-lane.sh goes red: a lane that cannot fail guards nothing, and the whole
# point of this one is to fail on a change this macOS host calls green. Each case is a scratch
# Go module the lane is pointed at, so the proof costs one small module per case and never the
# project's own suite.
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

# The defect itself: green on this host, red on Linux. The host leg is asserted first, because
# "the lane went red" only means something once this machine has called the same tree green.
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
cases=$((cases + 1))
if (cd "$mod" && go test ./... > "$scratch/host.out" 2>&1); then
  echo "ok   the darwin-only module is green on this host, which is the defect"
else
  echo "FAIL the darwin-only module should be green on this host:"
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

echo "examined=$cases cases, failed=$failed"
[ "$cases" -gt 0 ] && [ "$failed" -eq 0 ]
