#!/usr/bin/env bash
# End-to-end check of base lifecycle on a real repository (ADR 0008).
#
# Uses a local --mirror of the repository as "upstream" so the script can
# delete branches and force-push main, then fetches and runs the most
# aggressive gc on every tingly-fuse mirror and checks every session with
# git fsck. Prints one line per step: name, seconds, result.
#
#   bench/lifecycle.sh [repo-url]            default: tingly-dev/tingly-box
#
# Environment:
#   TF_OLD   optional binary of an older tingly-fuse: bases are created with
#            it, then checked and repaired with the current one (upgrade path)
#   TF_NEW   current binary (default: go build from this checkout)
#   BACKEND  backend for the sessions (default: the platform default)
#   FIX=0    skip `repo check --fix` before the rewrite (control run: shows
#            what happens to bases that are not pinned)
set -euo pipefail

url=${1:-https://github.com/tingly-dev/tingly-box}
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/tf-lifecycle.XXXXXX")
export TINGLY_FUSE_HOME=$work/home TINGLY_FUSE_VIEW_ROOT=$work/views
mkdir -p "$TINGLY_FUSE_VIEW_ROOT"
export GIT_AUTHOR_NAME=bench GIT_AUTHOR_EMAIL=bench@localhost GIT_COMMITTER_NAME=bench GIT_COMMITTER_EMAIL=bench@localhost

TF_NEW=${TF_NEW:-$work/tf}
[ -x "$TF_NEW" ] || (cd "$root" && go build -o "$TF_NEW" ./cmd/tingly-fuse)
TF_OLD=${TF_OLD:-$TF_NEW}
backend=${BACKEND:+--backend $BACKEND}
failures=0

step() { # step <name> <cmd...>: run, time, report; never aborts the script
	local name=$1; shift
	local t0 t1 rc=0
	t0=$(date +%s.%N)
	"$@" >"$work/out" 2>&1 || rc=$?
	t1=$(date +%s.%N)
	printf '%-34s %6.2fs  %s\n' "$name" "$(echo "$t1 - $t0" | bc)" "$([ $rc = 0 ] && echo ok || echo "FAIL ($rc)")"
	if [ $rc != 0 ]; then
		sed 's/^/    /' "$work/out" | head -8
		failures=$((failures + 1))
	fi
	return 0
}
info() { # like step, but a failure is reported and not counted
	local f=$failures; step "$@"; failures=$f
}
fsck_session() {
	"$TF_NEW" exec "$1" -- sh -c 'git status --porcelain >/dev/null &&
		git fsck --no-progress --no-dangling &&
		git submodule --quiet foreach --recursive "git fsck --no-progress --no-dangling"'
}

up=$work/upstream.git
step "mirror upstream locally" git clone --quiet --mirror "$url" "$up"
step "add topic branch upstream" sh -c "
	git clone --quiet '$up' '$work/w' &&
	cd '$work/w' && git switch --quiet -c topic && echo topic > TOPIC && git add TOPIC &&
	git commit --quiet -m topic && git push --quiet origin topic"
step "repo add" "$TF_OLD" repo add r "$up"
step "spawn --from main" "$TF_OLD" spawn r --id main $backend
step "spawn --from topic" "$TF_OLD" spawn r --id topic --from topic $backend

# A prepared directory with content upstream never sees.
prep=$work/prep
step "prepare directory" sh -c "
	git clone --quiet --recurse-submodules '$up' '$prep' 2>/dev/null || git clone --quiet '$up' '$prep'
	cd '$prep' && echo local > LOCAL && git add LOCAL && git commit --quiet -m unpushed &&
	echo stashed >> LOCAL && git stash --quiet && echo staged > STAGED && git add STAGED"
step "spawn --from-dir" "$TF_OLD" spawn r --id dir --from-dir "$prep" $backend
rm -rf "$prep"

info "repo check (before fix)" "$TF_NEW" repo check r
if [ "${FIX:-1}" = 1 ]; then
	step "repo check --fix" "$TF_NEW" repo check r --fix
fi

step "upstream: delete topic" git -C "$up" branch --quiet -D topic
step "upstream: force-push main" sh -c "
	cd '$work/w' && git switch --quiet main && git reset --quiet --hard HEAD~3 &&
	echo rewritten > REWRITTEN && git add REWRITTEN && git commit --quiet -m rewrite &&
	git push --quiet --force origin main"
step "repo fetch" "$TF_NEW" repo fetch r
gc_all() {
	for m in "$TINGLY_FUSE_HOME"/repos/r/objects.git "$TINGLY_FUSE_HOME"/repos/r/modules/*.git; do
		[ -d "$m" ] || continue
		git -C "$m" fetch --quiet --prune origin
		git -C "$m" -c gc.pruneExpire=now gc --quiet --prune=now
	done
}
step "fetch --prune + gc --prune=now" gc_all

for s in main topic dir; do
	step "fsck session $s" fsck_session "$s"
done
step "repo check (after gc)" "$TF_NEW" repo check r
base=$("$TF_NEW" list | awk '$1 == "topic" { print $4 }')
step "spawn --base <old topic base>" "$TF_NEW" spawn r --id again --base "$(ls "$TINGLY_FUSE_HOME/repos/r/bases" | grep "^$base")" $backend
step "spawn --from main follows upstream" sh -c "
	'$TF_NEW' spawn r --id fresh $backend >/dev/null 2>&1 &&
	'$TF_NEW' exec fresh -- test -e REWRITTEN"

echo "pinned refs: $(for m in "$TINGLY_FUSE_HOME"/repos/r/objects.git "$TINGLY_FUSE_HOME"/repos/r/modules/*.git; do git -C "$m" for-each-ref refs/tingly/ | wc -l; done | paste -sd+ | bc)"
for s in $("$TF_NEW" list | awk 'NR > 1 { print $1 }'); do "$TF_NEW" drop "$s" >/dev/null 2>&1 || true; done
rm -rf "$work"
echo "failures: $failures"
[ $failures = 0 ]
