#!/bin/bash
# 启动重写版「情谊有账」Web 服务（TCP 模式，非 fpk）
# 用 QY_ 前缀传参，避免继承 Hermes 运行时的同名 TRIM_* / PORT 变量。
export HOME=${HOME:-/vol3/@apphome/hermes-agent/data}
base=$(cd "$(dirname "$0")" && pwd) || exit 1
cd "$base" || exit 1

export PORT=${QY_PORT:-6189}
export WWW=${QY_WWW:-$base/www}
export DATA_FILE=${QY_DATA:-$base/data/data.json}
export TRIM_DATA_SHARE_PATHS=${QY_SHARES:-$base/exports}

mkdir -p "$(dirname "$DATA_FILE")" "$TRIM_DATA_SHARE_PATHS"
exec ./qingyi-web > "${QY_LOG:-$base/run.log}" 2>&1
