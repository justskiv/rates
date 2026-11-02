#!/usr/bin/env bash
# Local test of push.sh against a temporary bare repository. Nothing leaves
# the machine: the "remote" is a directory under mktemp.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
# Keep the developer's git config (signing, hooks) out of the test.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
trap 'rm -rf "$work"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

git_q() { git -c init.defaultBranch=main -c user.name=t -c user.email=t@example.invalid "$@"; }
remote_has() { git_q --git-dir "$work/remote.git" cat-file -e "$1" 2>/dev/null; }

# Remote with the two orphan branches as the owner sets them up.
git_q init -q --bare "$work/remote.git"
git_q clone -q "$work/remote.git" "$work/seed" 2>/dev/null
git_q -C "$work/seed" checkout -q --orphan data
touch "$work/seed/.nojekyll"
echo rates.tuzov.dev >"$work/seed/CNAME"
echo "# rates data" >"$work/seed/README.md"
git_q -C "$work/seed" add .nojekyll CNAME README.md
git_q -C "$work/seed" commit -q -m "init data"
git_q -C "$work/seed" push -q origin data
git_q -C "$work/seed" checkout -q --orphan archive
git_q -C "$work/seed" rm -rq --cached .
rm -f "$work/seed/.nojekyll" "$work/seed/CNAME" "$work/seed/README.md"
echo '{}' >"$work/seed/overrides.json"
git_q -C "$work/seed" add overrides.json
git_q -C "$work/seed" commit -q -m "init archive"
git_q -C "$work/seed" push -q origin archive

clone() { git_q clone -q --branch "$2" --depth 1 "file://$work/remote.git" "$1"; }
clone "$work/data" data
clone "$work/archive" archive
out="$work/github_output"
export GITHUB_OUTPUT="$out"

# 1. Nothing changed: no commit.
[[ $("$here/push.sh" "$work/data" data v1) == clean ]] || fail "clean tree pushed"

# 2. A run writes state and a new latest.json: both branches get a commit,
#    the purge flag is raised.
mkdir -p "$work/archive/state" "$work/archive/raw" "$work/data/v1"
echo '{"schemaVersion":1}' >"$work/archive/state/state.json"
echo 'gz' >"$work/archive/raw/body.json.gz"
echo '{"rates":{}}' >"$work/data/v1/latest.json"
[[ $("$here/push.sh" "$work/archive" archive state raw) == pushed ]] || fail "archive not pushed"
[[ $("$here/push.sh" "$work/data" data v1) == pushed ]] || fail "data not pushed"
grep -qx 'latest_changed=true' "$out" || fail "latest change not reported"
remote_has data:v1/latest.json || fail "latest.json not on remote"
remote_has archive:raw/body.json.gz || fail "raw body not on remote"

# 3. Only health.json changes: pushed, but no purge.
: >"$out"
echo '{"checkedAt":1}' >"$work/data/v1/health.json"
[[ $("$here/push.sh" "$work/data" data v1) == pushed ]] || fail "health not pushed"
[[ ! -s "$out" ]] || fail "purge requested without a latest.json change"

# 4. Root files are not the bot's: a run that loses or rewrites them,
#    or drops a stray file next to them, changes nothing on the remote.
rm "$work/data/CNAME" "$work/data/.nojekyll"
echo "tampered" >"$work/data/README.md"
echo "stray" >"$work/data/stray.txt"
echo '{"checkedAt":2}' >"$work/data/v1/health.json"
[[ $("$here/push.sh" "$work/data" data v1) == pushed ]] || fail "health not pushed after root changes"
remote_has data:CNAME || fail "CNAME deleted on the remote"
remote_has data:.nojekyll || fail ".nojekyll deleted on the remote"
remote_has data:stray.txt && fail "stray root file pushed"
[[ $(git_q --git-dir "$work/remote.git" show data:README.md) == "# rates data" ]] || fail "README changed"
rm -f "$work/archive/overrides.json"
echo '{"schemaVersion":1,"run":1}' >"$work/archive/state/state.json"
[[ $("$here/push.sh" "$work/archive" archive state raw) == pushed ]] || fail "archive not pushed"
remote_has archive:overrides.json || fail "overrides.json deleted on the remote"

# 5. The owner edits overrides.json on the remote meanwhile: rebase, no loss.
clone "$work/human" archive
echo '{"SYP":{"rate":121.8,"until":"2026-12-31"}}' >"$work/human/overrides.json"
git_q -C "$work/human" add overrides.json
git_q -C "$work/human" commit -q -m "override"
git_q -C "$work/human" push -q origin archive
echo '{"schemaVersion":1,"run":2}' >"$work/archive/state/state.json"
[[ $("$here/push.sh" "$work/archive" archive state raw) == pushed ]] || fail "archive push after remote edit"
git_q --git-dir "$work/remote.git" show archive:overrides.json | grep -q SYP || fail "owner edit lost"
git_q --git-dir "$work/remote.git" show archive:state/state.json | grep -q '"run":2' || fail "state lost"

# 6. An explicit REMOTE (the workflow's token URL) is used for pull and push.
echo '{"checkedAt":3}' >"$work/data/v1/health.json"
[[ $(REMOTE="file://$work/remote.git" "$here/push.sh" "$work/data" data v1) == pushed ]] || fail "push via REMOTE"
git_q --git-dir "$work/remote.git" show data:v1/health.json | grep -q '"checkedAt":3' || fail "REMOTE push lost"

echo "PASS push.sh"
