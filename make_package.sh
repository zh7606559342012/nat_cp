#!/bin/bash
# 打包脚本（仿 jxtx make_package.sh 的用法与包结构）。
#
# 用法：
#   ./make_package.sh <version>     例如：./make_package.sh v0.1
#
# 产物：
#   natcp_setup_<version>.tar.gz，解压后得到 natcp_setup_<version>/：
#     bin/cp                 CP 主程序
#     bin/conf/cp.yaml       生产配置示例（安装时不会覆盖已有配置）
#     bin/conf/cp_version.txt 打包版本
#     scripts/cpctl          启停/status 统一入口
#     scripts/start_all.sh / stop_all.sh / status_all.sh
#     deployments/natcp.service  systemd 单元文件
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
command -v go >/dev/null 2>&1 || die "go toolchain not found in PATH"

compile_code() {
    cd "${CURRENT_PATH}"
    go env -w GO111MODULE=on
    go mod tidy
    CGO_ENABLED=1 go build -trimpath -o cp ./cmd/cp
    [ -f cp ] || die "go build failed"
    cd "${CURRENT_PATH}"
}

package_all() {
    BINARY_FILENAME="natcp_setup_${VERSION}.tar.gz"
    PKG_PATHNAME="natcp_setup_${VERSION}"

    [ -f "${BINARY_FILENAME}" ] && rm -f "${BINARY_FILENAME}"
    [ -d "${PKG_PATHNAME}" ] && rm -rf "${PKG_PATHNAME}"
    mkdir -p "${PKG_PATHNAME}/bin/conf"
    mkdir -p "${PKG_PATHNAME}/scripts"
    mkdir -p "${PKG_PATHNAME}/deployments"

    # bin
    cp "${CURRENT_PATH}/cp" "${PKG_PATHNAME}/bin/"
    chmod +x "${PKG_PATHNAME}/bin/cp"
    # conf（安装时若已有配置则不覆盖，见 install.sh）
    cp "${CURRENT_PATH}/configs/cp.yaml" "${PKG_PATHNAME}/bin/conf/"
    echo "${VERSION}" > "${PKG_PATHNAME}/bin/conf/cp_version.txt"
    # scripts
    cp "${CURRENT_PATH}/scripts/cpctl" "${PKG_PATHNAME}/scripts/"
    cp "${CURRENT_PATH}/scripts/start_all.sh" "${PKG_PATHNAME}/scripts/"
    cp "${CURRENT_PATH}/scripts/stop_all.sh" "${PKG_PATHNAME}/scripts/"
    cp "${CURRENT_PATH}/scripts/status_all.sh" "${PKG_PATHNAME}/scripts/"
    chmod +x "${PKG_PATHNAME}/scripts/"*
    # systemd unit
    cp "${CURRENT_PATH}/deployments/natcp.service" "${PKG_PATHNAME}/deployments/"
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
rm -f "${CURRENT_PATH}/cp"
