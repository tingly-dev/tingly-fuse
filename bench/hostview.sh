#!/usr/bin/env bash
# 让会话视图在宿主上可见的几种做法，哪种适合非 root（issue #12）：
#
#   overlay        内核 overlayfs 直接挂在宿主上（需要 root，作为基准）
#   fuse-overlay   fuse-overlayfs 挂在宿主上（非 root 可用，走 FUSE）
#   copy           完整复制 base；在 XFS 上 cp 自动 reflink（写时复制），在 ext4 上是真复制
#
# 每种做法在 N 个会话上依次量：创建、宿主上 git status（IDE 打开目录就是这种读）、
# 会话里 git status、pnpm install（依赖已在 base 里，离线）、前端构建（vite build）、
# go build（共用预热好的缓存，-trimpath）。每个会话的磁盘占用用 statfs 增量。
# 一次性准备（clone、装依赖、预热 Go 缓存、建 base）不计入，每个文件系统做一次。
#
# 用法：bench/hostview.sh [ext4|xfs]...    （默认两个都跑）
# 环境变量：BENCH_DIR（回环盘镜像和挂载点，默认 /var/tmp/hostview）、BENCH_N（默认 3）、
#           BENCH_UPSTREAM、TINGLY_FUSE（二进制，默认 tingly-fuse）、BENCH_OUT
# 需要 root（挂回环盘）、fuse-overlayfs、pnpm、go。
set -euo pipefail

UPSTREAM=${BENCH_UPSTREAM:-https://github.com/tingly-dev/tingly-box}
DIR=${BENCH_DIR:-/var/tmp/hostview}
N=${BENCH_N:-3}
OUT=${BENCH_OUT:-$PWD/bench-out}
TF=${TINGLY_FUSE:-tingly-fuse}
SIZE=${BENCH_SIZE:-12G}

export CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0 GOTOOLCHAIN=auto GOFLAGS=-modcacherw
export GIT_AUTHOR_NAME=bench GIT_AUTHOR_EMAIL=bench@example.com
export GIT_COMMITTER_NAME=bench GIT_COMMITTER_EMAIL=bench@example.com

log() { echo "[$(date +%T)] $*" >&2; }
now() { date +%s.%N; }
mb() { awk -v b="$1" 'BEGIN{printf "%.1f", b/1048576}'; }

mkfs_mount() {  # fstype -> sets FS
  local t=$1 img=$DIR/$1.img
  FS=$DIR/mnt-$t
  mkdir -p "$FS"
  if findmnt -rn "$FS" >/dev/null; then umount -R "$FS"; fi
  rm -f "$img"; truncate -s "$SIZE" "$img"
  case $t in
    ext4) mkfs.ext4 -q "$img" ;;
    xfs) mkfs.xfs -q -m reflink=1 "$img" ;;
    *) echo "unknown fs $t" >&2; exit 2 ;;
  esac
  mount -o loop "$img" "$FS"
}

used() { sync -f "$FS"; df --output=used -B1 "$FS" | tail -1; }

tf() { TINGLY_FUSE_HOME=$FS/tf "$TF" "$@"; }

prepare() {
  local prep=$FS/prep
  git clone -q --recurse-submodules "$UPSTREAM" "$prep"
  (cd "$prep/frontend" && pnpm install --frozen-lockfile --store-dir "$FS/pnpm-store" >/dev/null)
  (cd "$prep" && GOMODCACHE=$FS/gomod GOCACHE=$FS/gocache go build -trimpath -o /dev/null ./cli/tingly-box)
  (cd "$prep/frontend" && pnpm build >/dev/null)
  tf git clone "$UPSTREAM" tingly-box >/dev/null
  tf git worktree add tingly-box --from-dir "$prep" --id seed --backend copy >/dev/null
  tf drop seed
  BASE=$(ls "$FS/tf/repos/tingly-box/bases" | grep -m1 '^dir-')
  rm -rf "$prep"
}

record() {  # method session step seconds disk_bytes
  printf '%s,%s,%s,%s,%.3f,%s\n' "$FSTYPE" "$1" "$2" "$3" "$4" "$(mb "$5")" | tee -a "$CSV"
}

measure() {  # method session step command...
  local m=$1 a=$2 step=$3; shift 3
  local u0 t0 t1 u1
  u0=$(used); t0=$(now)
  "$@" >>"$OUT/hostview-log.txt" 2>&1 || { log "FAILED: $FSTYPE $m $a $step"; tail -20 "$OUT/hostview-log.txt" >&2; return 1; }
  t1=$(now); u1=$(used)
  record "$m" "$a" "$step" "$(awk -v a="$t0" -v b="$t1" 'BEGIN{print b-a}')" $((u1 - u0))
}

inside() { tf exec "$1" -- bash -c "$2"; }
host() { (cd "$FS/tf/sessions/$1/root" && bash -c "$2"); }

run() {  # method
  local m=$1
  log "== $FSTYPE $m"
  for i in $(seq 1 "$N"); do
    local id=$m-$i
    measure "$m" "$id" create tf git worktree add tingly-box --base "$BASE" --id "$id" --backend "$m"
    measure "$m" "$id" host-git-status host "$id" "git status --porcelain"
    measure "$m" "$id" git-status inside "$id" "git status --porcelain"
    measure "$m" "$id" pnpm-install inside "$id" "cd frontend && pnpm install --frozen-lockfile --offline --store-dir $FS/pnpm-store"
    measure "$m" "$id" vite-build inside "$id" "cd frontend && pnpm build"
    measure "$m" "$id" go-build inside "$id" "GOMODCACHE=$FS/gomod GOCACHE=$FS/gocache go build -trimpath -o /dev/null ./cli/tingly-box"
  done
  for i in $(seq 1 "$N"); do tf drop "$m-$i"; done
  if findmnt -rn | grep -q " $FS/tf/sessions/"; then log "mounts left after drop"; exit 1; fi
}

mkdir -p "$DIR" "$OUT"
CSV=$OUT/hostview.csv
[ -f "$CSV" ] || echo "fs,method,session,step,seconds,disk_mb" >"$CSV"
echo "date: $(date -u +%FT%TZ)  upstream: $UPSTREAM @ $(git ls-remote "$UPSTREAM" HEAD | cut -c1-7)  N=$N  kernel: $(uname -r)  cpus: $(nproc)  git: $(git --version | cut -d' ' -f3)  $(fuse-overlayfs --version 2>&1 | grep -i 'fuse-overlayfs:')  tingly-fuse: $(git -C "$(dirname "$0")" rev-parse --short HEAD)" | tee -a "$OUT/hostview-env.txt"

for t in ${*:-ext4 xfs}; do
  FSTYPE=$t
  mkfs_mount "$t"
  prepare
  for m in copy overlay fuse-overlay; do run "$m"; done
  umount -R "$FS"
done
