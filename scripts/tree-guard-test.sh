#!/bin/sh
# Proves scripts/tree-guard.sh goes red: every string planted in a scratch
# repository under the real guard files must fail it, look-alike words must not,
# and the allow-list and the zero-file guard must behave as that script documents.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd -P)
check=$root/scripts/tree-guard.sh
scratch=$(mktemp -d)
trap '[ -n "${scratch:-}" ] && [ -d "$scratch" ] && rm -rf "$scratch"' EXIT
cases=0
failed=0

# expect <guard> <exit-code> <text-the-output-must-contain> <description>
expect() {
  cases=$((cases + 1))
  rc=0
  sh "$check" "$1" "$repo" > "$scratch/out" 2>&1 || rc=$?
  if [ "$rc" -eq "$2" ] && grep -qF -- "$3" "$scratch/out"; then
    echo "ok   $4"
  else
    echo "FAIL $4: exit $rc (want $2), output must contain '$3':"
    sed 's/^/     /' "$scratch/out"
    failed=$((failed + 1))
  fi
}

# fresh <name> <guard>: a new repository with one clean tracked file and the
# named real guard file with its allow entries dropped
fresh() {
  repo=$scratch/$1
  git init -q "$repo"
  mkdir "$repo/.guard"
  grep -E '^(#|pattern:)' "$root/.guard/$2" > "$repo/.guard/$2"
  echo 'nothing to see' > "$repo/clean.txt"
  git -C "$repo" add clean.txt ".guard/$2"
}

# plant <file> <line>
plant() {
  mkdir -p "$(dirname "$repo/$1")"
  printf '%s\n' "$2" > "$repo/$1"
  git -C "$repo" add "$1"
}

fork=.guard/fork-identifiers
former=.guard/former-name

repo=$scratch/empty
git init -q "$repo"
mkdir "$repo/.guard"
cp "$root/$fork" "$repo/$fork"
expect "$fork" 1 'examined=0' 'an empty tree fails'

fresh clean fork-identifiers
expect "$fork" 0 'examined=2' 'a clean tree passes, and the guard file does not match itself'

for id in fm- FM_ crewmate secondmate firstmate herdr treehouse kunchenguid; do
  fresh "fork$cases" fork-identifiers
  plant planted.txt "a planted $id here"
  expect "$fork" 1 "planted.txt:1:a planted $id here" "'$id' in file content fails"
done

fresh forkpath fork-identifiers
plant treehouse/a.txt 'nothing to see'
expect "$fork" 1 'treehouse/a.txt' 'a fork identifier in a path fails'

for line in 'run orc status' 'Orc is planned' '# ORC' 'export ORC_HOME=x' 'orc-verb push' 'see cmd/orc'; do
  fresh "former$cases" former-name
  plant planted.txt "$line"
  expect "$former" 1 "planted.txt:1:$line" "former name in '$line' fails"
done

fresh formerpath former-name
plant cmd/orc/main.go 'package main'
expect "$former" 1 'cmd/orc/main.go' 'the former name in a path fails'

fresh lookalike former-name
plant words.txt 'the orchestrator force Orca porcelain torch forc_x orcs'
expect "$former" 0 'no matches' 'words that merely contain the letters pass'

fresh allowed fork-identifiers
plant cite.md 'cites herdr'
echo 'cite.md # a citation' >> "$repo/$fork"
expect "$fork" 0 'allowed=1' 'an allowed occurrence passes'

fresh noreason fork-identifiers
plant cite.md 'cites herdr'
echo 'cite.md' >> "$repo/$fork"
expect "$fork" 1 "has no '# <reason>'" 'an allow-list entry without a reason fails'

fresh stale fork-identifiers
echo 'clean.txt # no longer needed' >> "$repo/$fork"
expect "$fork" 1 'does not match; remove the entry' 'an allow-list entry with nothing to allow fails'

fresh nopattern fork-identifiers
grep -v '^pattern:' "$root/$fork" > "$repo/$fork"
expect "$fork" 1 "exactly one non-empty 'pattern:' line" 'a guard file without a pattern fails'

echo "examined=$cases cases, failed=$failed"
[ "$cases" -gt 0 ] && [ "$failed" -eq 0 ]
