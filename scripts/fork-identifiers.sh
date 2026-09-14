#!/bin/sh
# Fails when a distinctive identifier of the fork this project replaces appears
# in a tracked file's path or content (a5 section 2.18). A legitimate occurrence,
# such as a citation, is allowed by naming its file in .fork-identifiers-allow
# as `<path> # <reason>`.
# Usage: scripts/fork-identifiers.sh [repository-root]
# Exit: 0 clean, 1 an occurrence or an invalid allow-list, 2 the scan itself failed.
set -eu

cd "${1:-.}"
pattern='fm-|FM_|crewmate|secondmate|firstmate|herdr|treehouse|kunchenguid'
allow=.fork-identifiers-allow

files=$(git ls-files | wc -l | tr -d ' ')
echo "examined=$files tracked files"
if [ "$files" -eq 0 ]; then
  echo "error: no tracked files, so this check examined nothing" >&2
  exit 1
fi

# Each allowed path becomes an exclude pathspec in "$@". An entry that no longer
# matches anything is refused, so the allow-list cannot outlive its reason.
set --
if [ -f "$allow" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    path=$(printf '%s' "${line%%#*}" | sed 's/[[:space:]]*$//')
    [ -n "$path" ] || continue
    reason=$(printf '%s' "${line#*#}" | sed 's/^[[:space:]]*//')
    if [ "$reason" = "$line" ] || [ -z "$reason" ]; then
      echo "error: $allow entry '$path' has no '# <reason>'" >&2
      exit 1
    fi
    if ! git ls-files -- ":(literal)$path" | grep -qE "$pattern" &&
      ! git grep -qE "$pattern" -- ":(literal)$path"; then
      echo "error: $allow names '$path', which is not tracked or holds no fork identifier; remove the entry" >&2
      exit 1
    fi
    set -- "$@" ":(exclude,literal)$path"
  done < "$allow"
fi
echo "allowed=$# files named in $allow"

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
  echo "error: fork identifiers found above; remove them, or allow the file in $allow with a reason" >&2
  exit 1
fi
echo "no fork identifiers found"
