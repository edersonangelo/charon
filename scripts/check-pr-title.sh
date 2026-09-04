#!/bin/sh
# Validate a pull request title against Conventional Commits.
#
# Pull requests are squash-merged, so the title becomes the commit message on
# develop. It is the only text that reaches history, which is why it is the one
# thing enforced.
#
# Usage: scripts/check-pr-title.sh "feat(ingest): store raw payload"
set -eu

title="${1:-}"
pattern='^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([a-z0-9._/-]+\))?!?: [a-z].{0,70}[^.]$'

if [ -n "$title" ] && printf '%s' "$title" | grep -Eq "$pattern"; then
	printf 'ok: %s\n' "$title"
	exit 0
fi

{
	echo 'The pull request title is not a valid Conventional Commit.'
	echo
	echo '  <type>[(scope)][!]: <description>'
	echo
	echo '  type         build chore ci docs feat fix perf refactor revert style test'
	echo '  description  starts lowercase, at most 71 characters, no trailing period'
	echo '  !            marks a breaking change'
	echo
	echo 'Examples:'
	echo '  feat(ingest): store raw payload before acknowledging'
	echo '  fix(delivery): stop counting dead-lettered events as delivered'
	echo '  refactor(store)!: replace the event key with a ULID'
	echo
	printf 'Given:\n  %s\n' "$title"
} >&2
exit 1
