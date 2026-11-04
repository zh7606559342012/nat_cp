#!/bin/bash
# 停止 CP（仿 jxtx stop_all.sh）。实际工作由同目录的 cpctl 完成。
SCRIPT_DIR="$(dirname "$(readlink -f "$0")")"
if [ -x "${SCRIPT_DIR}/cpctl" ]; then
    exec "${SCRIPT_DIR}/cpctl" stop
else
    exec cpctl stop
fi
