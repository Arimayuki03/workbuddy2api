#!/usr/bin/env bash
# trial.sh — global trial 加油包领取辅助工具（薄包装）
#
# 不调 HTTP：cmd/trial 已自带逐账号表格输出（uid/nick/status/detail
# + total=N ok=N already=N na=N fail=N 汇总行），并对 CN 账号明确
# 提示不适用（只有 global 账号会被执行 /billing/ide/trial）。
#
# 用法:
#   ./trial.sh            # cmd/trial 表格
#   ./trial.sh auths_dir  # 指定 auths 目录
#
# 常见状态:
#   OK      领取成功（一次性加油包到账）
#   ALREADY 已领取过（幂等，不算失败）
#   N/A     CN 账号不适用（global 专属端点）
#
# 依赖: go（首次构建 trial_bin）
set -euo pipefail
cd "$(dirname "$0")"

AUTHS_DIR="auths"
if [[ $# -gt 0 ]]; then
    AUTHS_DIR="$1"
fi

CACHE_DIR="${TMPDIR:-/tmp}/workbuddy2api-bin"
mkdir -p "$CACHE_DIR"

# 构建缓存：源码变更才重编（与 checkin.sh 同策略）。
TRIAL_BIN="$CACHE_DIR/trial_bin"
if [[ ! -x "$TRIAL_BIN" ]] || find . -name '*.go' -newer "$TRIAL_BIN" -print -quit | grep -q .; then
    echo "构建 trial.." >&2
    go build -o "$TRIAL_BIN" ./cmd/trial
fi

"$TRIAL_BIN" "$AUTHS_DIR"