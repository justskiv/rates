#!/usr/bin/env bash
# Commit and push the paths the rates job owns on one branch of this
# repository. rates-publish.yml calls it for "archive" (state raw) first and
# for "data" (v1) second: if the run dies in between, the state is saved
# and the data follows on the next run, never the other way round.
#
# Usage: push.sh DIR BRANCH PATHSPEC...
#
# Only PATHSPECs are staged, so files the owner placed on the branch
# (.nojekyll, CNAME, README.md, overrides.json) can never be deleted or
# changed by the bot, even if they vanish from the worktree.
#
# REMOTE overrides the push target (default "origin"); the workflow passes
# an https URL with the job's GITHUB_TOKEN because the checkouts keep no
# credentials. Prints "pushed" or "clean". When v1/latest.json changed it
# writes latest_changed=true to $GITHUB_OUTPUT (if set) for the CDN purge.
set -euo pipefail

if [[ $# -lt 3 ]]; then
	echo "usage: push.sh DIR BRANCH PATHSPEC..." >&2
	exit 2
fi
dir=$1 branch=$2
shift 2
remote=${REMOTE:-origin}

# The identity is needed by the commit and by a rebase onto a newer remote.
export GIT_AUTHOR_NAME=rates-bot GIT_COMMITTER_NAME=rates-bot
export GIT_AUTHOR_EMAIL=41898282+github-actions[bot]@users.noreply.github.com
export GIT_COMMITTER_EMAIL=41898282+github-actions[bot]@users.noreply.github.com

# git add fails on a pathspec that matches nothing (no raw/ yet), so keep
# the paths that exist in the worktree or are tracked.
paths=()
for p in "$@"; do
	if [[ -e "$dir/$p" || -n $(git -C "$dir" ls-files -- "$p") ]]; then
		paths+=("$p")
	fi
done
if [[ ${#paths[@]} -gt 0 ]]; then
	git -C "$dir" add -A -- "${paths[@]}"
fi
if git -C "$dir" diff --cached --quiet; then
	echo clean
	exit 0
fi

if [[ -n $(git -C "$dir" diff --cached --name-only -- v1/latest.json) ]]; then
	if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
		echo "latest_changed=true" >>"$GITHUB_OUTPUT"
	fi
fi

git -C "$dir" commit -q -m "chore(data): rates run $(date -u +%Y-%m-%dT%H:%MZ)"

# The owner may have edited overrides.json in the web UI since checkout.
# --autostash keeps unstaged leftovers outside the bot's paths out of the way.
git -C "$dir" pull -q --rebase --autostash "$remote" "$branch" >&2
git -C "$dir" push -q "$remote" "HEAD:$branch"
echo pushed
