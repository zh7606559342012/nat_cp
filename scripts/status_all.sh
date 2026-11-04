#!/bin/bash
# 查看 CP 状态（仿 jxtx status_all.sh）。实际工作由同目录的 cpctl 完成。
SCRIPT_DIR="$(dirname "$(readlink -f "$0")")"
if [ -x "${SCRIPT_DIR}/cpctl" ]; then
    exec "${SCRIPT_DIR}/cpctl" status
else
    exec cpctl status
fi
