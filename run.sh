#!/bin/bash
# 情谊有账 — 本地/裸机启动脚本（TCP 模式）
# 用 QY_ 前缀传参，避免与宿主机环境里可能存在的同名 PORT / DATA_FILE 冲突。
set -e
base=$(cd "$(dirname "$0")" && pwd)
cd "$base"

export PORT=${QY_PORT:-6189}
export WWW=${QY_WWW:-$base/www}
export DATA_FILE=${QY_DATA:-$base/data/data.json}
export TRIM_DATA_SHARE_PATHS=${QY_SHARES:-$base/exports}

mkdir -p "$(dirname "$DATA_FILE")"
[ -n "$TRIM_DATA_SHARE_PATHS" ] && mkdir -p "$TRIM_DATA_SHARE_PATHS"

if [ -z "${QY_PASSWORD:-}" ]; then
  echo "错误：未设置 QY_PASSWORD，拒绝启动（不设密码会导致账本对任何能访问该端口的人开放）" >&2
  exit 1
fi
export QY_PASSWORD

exec ./qingyi-web >> "${QY_LOG:-$base/run.log}" 2>&1
