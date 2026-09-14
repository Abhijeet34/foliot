#!/bin/sh
# Fails when a guard file's pattern appears in a tracked file's path or content.
# A guard file holds one `pattern: <extended regex>` line and then the files
# allowed to match it, one per line as `<path> # <reason>`; the guard file itself
# is never scanned, since it holds the pattern. Guards live in .guard/.
# Usage: scripts/tree-guard.sh <guard-file> [repository-root]
# Exit: 0 clean, 1 an occurrence or an invalid guard file, 2 the scan itself failed.
set -eu

guard=${1:?usage: scripts/tree-guard.sh <guard-file> [repository-root]}
cd "${2:-.}"
if [ ! -f "$guard" ]; then
  echo "error: guard file '$guard' does not exist" >&2
  exit 1
fi
pattern=$(sed -n 's/^pattern:[[:space:]]*//p' "$guard")
if [ -z "$pattern" ] || [ "$(printf '%s\n' "$pattern" | wc -l | tr -d ' ')" -ne 1 ]; then
  echo "error: $guard must hold exactly one non-empty 'pattern:' line" >&2
  exit 1
fi
echo "guard=$guard pattern=$pattern"

files=$(git ls-files | wc -l | tr -d ' ')
echo "examined=$files tracked files"
if [ "$files" -eq 0 ]; then
  echo "error: no tracked files, so this check examined nothing" >&2
  exit 1
fi

# Each allowed path becomes an exclude pathspec in "$@". An entry that no longer
# matches anything is refused, so the allow-list cannot outlive its reason.
set -- ":(exclude,literal)$guard"
while IFS= read -r line || [ -n "$line" ]; do
  case $line in pattern:*) continue ;; esac
  path=$(printf '%s' "${line%%#*}" | sed 's/[[:space:]]*$//')
  [ -n "$path" ] || continue
  reason=$(printf '%s' "${line#*#}" | sed 's/^[[:space:]]*//')
  if [ "$reason" = "$line" ] || [ -z "$reason" ]; then
    echo "error: $guard entry '$path' has no '# <reason>'" >&2
    exit 1
  fi
  if ! git ls-files -- ":(literal)$path" | grep -qE "$pattern" &&
    ! git grep -qE "$pattern" -- ":(literal)$path"; then
    echo "error: $guard names '$path', which is not tracked or does not match; remove the entry" >&2
    exit 1
  fi
  set -- "$@" ":(exclude,literal)$path"
done < "$guard"
echo "allowed=$(($# - 1)) files named in $guard"

found=0
rc=0
git grep -nE "$pattern" -- . "$@" || rc=$?
case $rc in
  0) found=1 ;;
  1) ;;
  *) echo "error: git grep exited $rc" >&2; exit 2 ;;
esac
if git ls-files -- . "$@" | grep -E "$pattern"; then
  found=1
fi

if [ "$found" -eq 1 ]; then
  echo "error: matches for $guard found above; remove them, or allow the file in $guard with a reason" >&2
  exit 1
fi
echo "no matches for $guard"
