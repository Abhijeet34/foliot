#!/bin/sh
# Proves scripts/fork-identifiers.sh goes red: every identifier planted in a
# scratch repository must fail it, and the allow-list and the zero-file guard
# must behave as that script documents.
set -eu

check=$(cd "$(dirname "$0")" && pwd -P)/fork-identifiers.sh
scratch=$(mktemp -d)
trap '[ -n "${scratch:-}" ] && [ -d "$scratch" ] && rm -rf "$scratch"' EXIT
cases=0
failed=0

# expect <exit-code> <text-the-output-must-contain> <description>
expect() {
  cases=$((cases + 1))
  rc=0
  sh "$check" "$repo" > "$scratch/out" 2>&1 || rc=$?
  if [ "$rc" -eq "$1" ] && grep -qF -- "$2" "$scratch/out"; then
    echo "ok   $3"
  else
    echo "FAIL $3: exit $rc (want $1), output must contain '$2':"
    sed 's/^/     /' "$scratch/out"
    failed=$((failed + 1))
  fi
}

# fresh <name>: a new repository with one clean tracked file
fresh() {
  repo=$scratch/$1
  git init -q "$repo"
  echo 'nothing to see' > "$repo/clean.txt"
  git -C "$repo" add clean.txt
}

repo=$scratch/empty
git init -q "$repo"
expect 1 'examined=0' 'an empty tree fails'

fresh clean
expect 0 'examined=1' 'a clean tree passes'

for id in fm- FM_ crewmate secondmate firstmate herdr treehouse kunchenguid; do
  fresh "content$cases"
  printf 'a planted %s here\n' "$id" > "$repo/planted.txt"
  git -C "$repo" add planted.txt
  expect 1 "planted.txt:1:a planted $id here" "'$id' in file content fails"
done

fresh path
mkdir "$repo/treehouse"
echo 'nothing to see' > "$repo/treehouse/a.txt"
git -C "$repo" add treehouse
expect 1 'treehouse/a.txt' 'an identifier in a path fails'

fresh allowed
echo 'cites herdr' > "$repo/cite.md"
echo 'cite.md # a citation' > "$repo/.fork-identifiers-allow"
git -C "$repo" add cite.md .fork-identifiers-allow
expect 0 'allowed=1' 'an allowed occurrence passes'

fresh noreason
echo 'cites herdr' > "$repo/cite.md"
echo 'cite.md' > "$repo/.fork-identifiers-allow"
git -C "$repo" add cite.md .fork-identifiers-allow
expect 1 "has no '# <reason>'" 'an allow-list entry without a reason fails'

fresh stale
echo 'clean.txt # no longer needed' > "$repo/.fork-identifiers-allow"
git -C "$repo" add .fork-identifiers-allow
expect 1 'holds no fork identifier' 'an allow-list entry with nothing to allow fails'

echo "examined=$cases cases, failed=$failed"
[ "$cases" -gt 0 ] && [ "$failed" -eq 0 ]
