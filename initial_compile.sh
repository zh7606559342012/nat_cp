#!/bin/bash
# Go 工具链初始化脚本（仿 jxtx initial_compile.sh 的用途）。
#
# 用法：
#   ./initial_compile.sh
#
# 行为：检查 go 是否可用且版本 >= 1.22，并设置 Go 模块与代理环境
# （GO111MODULE=on，GOPROXY=https://goproxy.cn,direct）。
# 若本机没有 Go，会直接报错并提示安装方式，不自动下载安装。
set -e

die() {
    echo "error: $1" >&2
    exit 1
}

if ! command -v go >/dev/null 2>&1; then
    die "go toolchain not found. install Go >= 1.22, e.g.: download https://go.dev/dl/ and add its bin/ to PATH"
fi

GO_VERSION="$(go version | awk '{print $3}' | sed 's/^go//')"
GO_MAJOR="$(echo "${GO_VERSION}" | cut -d. -f1)"
GO_MINOR="$(echo "${GO_VERSION}" | cut -d. -f2)"
if [ "${GO_MAJOR}" -lt 1 ] || { [ "${GO_MAJOR}" -eq 1 ] && [ "${GO_MINOR}" -lt 22 ]; }; then
    die "go >= 1.22 required, found ${GO_VERSION}"
fi

go env -w GO111MODULE=on
go env -w GOPROXY=https://goproxy.cn,direct
echo "go ${GO_VERSION} ready (GO111MODULE=on, GOPROXY=https://goproxy.cn,direct)"
