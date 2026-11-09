#!/bin/bash
# 打包脚本（仿 jxtx make_package.sh 的用法与包结构）。
#
# 用法：
#   ./make_package.sh <version>     例如：./make_package.sh v0.1
#
# 产物：
#   fagw-cp_setup_<version>.tar.gz，解压后得到 fagw-cp_setup_<version>/：
#     bin/cp                 CP 主程序（安装到 /opt/fagw/fagw-cp/bin/cp）
#     bin/conf/cp.yaml       生产配置示例（安装时不会覆盖已有配置）
#     bin/conf/cp_version.txt 打包版本
#     scripts/cpctl          启停/status 统一入口
#     scripts/start_all.sh / stop_all.sh / status_all.sh
#     install.sh / upgrade.sh
#     README.md
#
# 部署流程：把 tar.gz 传到目标机，解压后进目录执行 ./install.sh。
set -e

VERSION="$1"
CURRENT_PATH="$(pwd)"

die() {
    echo "error: $1" >&2
    exit 1
}

usage() {
    echo "./$(basename "$0") <version_tag>"
    echo "example: ./$(basename "$0") v0.1"
    exit 0
}

[ -n "${VERSION}" ] || usage
command -v go >/dev/null 2>&1 || die "go toolchain not found in PATH (run ./initial_compile.sh first)"

compile_code() {
    cd "${CURRENT_PATH}/main"
    go env -w GO111MODULE=on
    go env -w GOPROXY=https://goproxy.cn,direct

    # SQLite 驱动 mattn/go-sqlite3 是 CGO 库。必须显式要求 CGO：
    # CGO_ENABLED=0 时它会被编译成 stub —— 编译能过、打包能成，
    # 直到目标机上启动才报 "Binary was compiled with 'CGO_ENABLED=0'",
    # 属于最难排查的一类问题，所以这里提前卡死。
    command -v gcc >/dev/null 2>&1 \
        || die "gcc not found: mattn/go-sqlite3 needs CGO. run: sudo apt-get install -y build-essential"
    export CGO_ENABLED=1

    # 用 download 而不是 tidy：
    # tidy 会重算依赖并改写 go.mod / go.sum，当构建机的 Go 版本高于开发机时
    # 可能悄悄抬高 go 指令或升级间接依赖，导致开发机反而编不过。
    go mod download

    go build -ldflags "-X 'main.Version=${VERSION}'" -o cp .
    [ -f cp ] || die "go build failed"
    echo "built: $(go version | awk '{print $3}') / $(go env GOOS)/$(go env GOARCH) / CGO_ENABLED=$(go env CGO_ENABLED)"
    cd "${CURRENT_PATH}"
}

package_all() {
    BINARY_FILENAME="fagw-cp_setup_${VERSION}.tar.gz"
    PKG_PATHNAME="fagw-cp_setup_${VERSION}"

    [ -f "${BINARY_FILENAME}" ] && rm -f "${BINARY_FILENAME}"
    [ -d "${PKG_PATHNAME}" ] && rm -rf "${PKG_PATHNAME}"
    mkdir -p "${PKG_PATHNAME}/bin/conf"
    mkdir -p "${PKG_PATHNAME}/scripts"

    # bin
    cp "${CURRENT_PATH}/main/cp" "${PKG_PATHNAME}/bin/"
    chmod +x "${PKG_PATHNAME}/bin/cp"
    # conf（安装时若已有配置则不覆盖，见 install.sh）
    cp "${CURRENT_PATH}/main/conf/cp.yaml" "${PKG_PATHNAME}/bin/conf/"
    echo "${VERSION}" > "${PKG_PATHNAME}/bin/conf/cp_version.txt"
    # scripts（源自 main/scripts，与 jxtx 的 main/scripts 对应）
    cp "${CURRENT_PATH}/main/scripts/"* "${PKG_PATHNAME}/scripts/"
    chmod +x "${PKG_PATHNAME}/scripts/"*
    # top-level
    cp "${CURRENT_PATH}/install.sh" "${PKG_PATHNAME}/"
    cp "${CURRENT_PATH}/upgrade.sh" "${PKG_PATHNAME}/"
    cp "${CURRENT_PATH}/README.md" "${PKG_PATHNAME}/"
    chmod +x "${PKG_PATHNAME}/install.sh" "${PKG_PATHNAME}/upgrade.sh"

    tar -czf "${BINARY_FILENAME}" "${PKG_PATHNAME}"
    echo "package built: ${CURRENT_PATH}/${BINARY_FILENAME}"
}

compile_code
package_all
# 清理构建产物，不进包
rm -f "${CURRENT_PATH}/main/cp"
