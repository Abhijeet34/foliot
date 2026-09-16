#!/bin/sh
# Runs this module's vet and test suite on Linux from a macOS workstation, so a change that
# assumes a darwin path, a darwin facility or Seatbelt is red here rather than after the pull
# request is open. It answers the same question as the `go` job in .github/workflows/ci.yml and
# counts what it examined the same way, so its verdict and CI's mean the same thing.
#
# Usage: scripts/linux-lane.sh [module-root]        (default: this repository)
# Exit:  0 both legs green, 1 a leg is red, 2 the lane itself could not run.
#
# Two legs, cheapest first. The cross-vet leg needs no container and catches a Linux build
# break in seconds. The container leg is the one that matters: the failure this lane exists
# for was a test that compiled everywhere and only behaved differently on Linux, which no
# amount of cross-compiling reads.
#
# The tree is streamed in as a tar and unpacked to a writable copy rather than bind-mounted.
# That is not tidiness: a read-only bind mount of the source produced a red this tree does not
# have on Linux (measured 2026-09-16 on two different base images, against a commit CI passed),
# and colima, the container runtime on this machine, shares $HOME but not /tmp, so a mount of a
# scratch tree under TMPDIR arrives in the container empty and the suite tests nothing at all.
set -eu

root=$(cd "${1:-$(dirname "$0")/..}" && pwd -P)
docker=${FOLIOT_LINUX_LANE_DOCKER:-docker}
[ -f "$root/go.mod" ] || { echo "error: no go.mod at $root" >&2; exit 2; }
go_version=$(sed -n 's/^go[[:space:]]*\([0-9][0-9.]*\)$/\1/p' "$root/go.mod")
[ -n "$go_version" ] || { echo "error: $root/go.mod names no go version" >&2; exit 2; }
# The local/ prefix marks an image that has no registry behind it, so this machine's weekly
# maintenance run skips the whole namespace instead of failing to pull a name that exists
# only here (measured: two foliot images reported as hard ERRORs by the 2026-09-16 run).
image=local/foliot-linux-lane:$go_version
work=$(mktemp -d)
trap '[ -n "${work:-}" ] && [ -d "$work" ] && rm -rf "$work"' EXIT

echo "lane: $root on linux, go $go_version, via $docker"

echo "--- cross-vet: GOOS=linux go vet ./..."
# go list's own exit code, not its line count: a toolchain that cannot read this tree at all
# (an unwritable GOCACHE under a sandbox, a go.mod it refuses) prints nothing and is a lane
# that never read Linux, which is exit 2 like a missing runtime. Zero packages from a go list
# that succeeded is a different thing, and stays red.
if ! list=$(cd "$root" && go list ./... 2> "$work/list.err"); then
  echo "error: go list ./... could not read $root, so the lane read no Linux at all:" >&2
  sed 's/^/  /' "$work/list.err" >&2
  exit 2
fi
packages=$(printf '%s\n' "$list" | grep -c . || true)
echo "examined=$packages packages"
[ "$packages" -gt 0 ] || { echo "error: the cross-vet leg examined 0 packages" >&2; exit 1; }
if ! (cd "$root" && GOOS=linux go vet ./...); then
  echo "error: this tree does not vet for linux" >&2
  exit 1
fi

echo "--- container: go vet ./... && go test -v ./..."
command -v "$docker" > /dev/null 2>&1 || { echo "error: '$docker' is not on PATH; the lane cannot read Linux without it" >&2; exit 2; }
"$docker" info > /dev/null 2>&1 || { echo "error: '$docker' is installed but its daemon does not answer" >&2; exit 2; }
"$docker" build -q --build-arg "GO_VERSION=$go_version" -t "$image" \
  -f "$(dirname "$0")/linux-lane.Dockerfile" "$(dirname "$0")" > /dev/null ||
  { echo "error: building $image failed" >&2; exit 2; }

# .git is left out: nothing under test reads this repository's own history, and it is the
# largest thing in the tree. The container's user is this host's uid, because root ignores a
# read-only mode bit and the hostile-root cases in src/core/log would pass proving nothing.
tar --no-xattrs -cf "$work/src.tar" --exclude ./.git -C "$root" .
rc=0
"$docker" run --rm -i --user "$(id -u):$(id -g)" -e HOME=/tmp/home -e GOCACHE=/tmp/gocache \
  "$image" sh -c 'mkdir -p /tmp/src && cd /tmp/src && tar -xf - && go vet ./... && go test -v ./...' \
  < "$work/src.tar" > "$work/test.log" 2>&1 || rc=$?
cat "$work/test.log"
passed=$(grep -cE '^ *--- PASS: ' "$work/test.log" || true)
failed=$(grep -cE '^ *--- FAIL: ' "$work/test.log" || true)
echo "examined=$((passed + failed)) tests, passed=$passed, failed=$failed"
if [ "$rc" -ne 0 ]; then
  echo "error: this tree is red on linux; it would be red in CI too" >&2
  exit 1
fi
[ "$passed" -gt 0 ] || { echo "error: the container leg ran 0 tests, so it proves nothing" >&2; exit 1; }
echo "linux lane green"
