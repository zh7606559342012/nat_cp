#!/bin/bash
# 升级脚本（仿 jxtx upgrade.sh 的流程）。
#
# 用法（在解压后的新版本安装包目录内执行，需 root）：
#   ./upgrade.sh            # 升级并自动重启服务
#   ./upgrade.sh --no-start # 只替换文件，不重启（-u 同义）
#
# 行为：停服务 → 备份旧配置到 /home/natcp_conf → 替换二进制与脚本 →
# 恢复旧配置 →（默认）重启服务。配置文件、SQLite 数据库与日志不动。
set -e

CUR_PATH="$(dirname "$(readlink -f "$0")")"
NATCP_ROOT="/opt/nat_cp"
NATCP_BIN="${NATCP_ROOT}/bin"
NATCP_CONF_DIR="${NATCP_BIN}/conf"
CONF_BACKUP="/home/natcp_conf"
SERVICE_NAME="natcp"
NoAutoStart=0

die() {
    echo "error: $1" >&2
    exit 1
}

while [[ $# -ge 1 ]]; do
    case "$1" in
        -u|--no-start)
            NoAutoStart=1
            ;;
        -h|--help)
            echo "./upgrade.sh [--no-start]"
            exit 0
            ;;
    esac
    shift
done

[ "$(id -u)" = "0" ] || die "upgrade.sh must run as root"
[ -f "${CUR_PATH}/bin/cp" ] || die "bin/cp not found, run inside the extracted package dir"

# 停服务并备份旧配置（仿 jxtx upgrade.sh 的 env_check）
if command -v systemctl >/dev/null 2>&1; then
    systemctl stop "${SERVICE_NAME}" >/dev/null 2>&1 || true
fi
rm -rf "${CONF_BACKUP}"
if [ -f "${NATCP_CONF_DIR}/cp.yaml" ]; then
    mkdir -p "${CONF_BACKUP}"
    cp -f "${NATCP_CONF_DIR}/cp.yaml" "${CONF_BACKUP}/"
fi

# 替换二进制与脚本，恢复配置
cp -f "${CUR_PATH}/bin/cp" "${NATCP_BIN}/"
chmod +x "${NATCP_BIN}/cp"
cp -f "${CUR_PATH}/scripts/"* "${NATCP_ROOT}/scripts/" 2>/dev/null || true
chmod +x "${NATCP_ROOT}/scripts/"* 2>/dev/null || true
if [ -f "${CONF_BACKUP}/cp.yaml" ]; then
    cp -f "${CONF_BACKUP}/cp.yaml" "${NATCP_CONF_DIR}/"
fi
if [ -f "${CUR_PATH}/bin/conf/cp_version.txt" ]; then
    cp -f "${CUR_PATH}/bin/conf/cp_version.txt" "${NATCP_CONF_DIR}/"
fi
if [ -f "${CUR_PATH}/deployments/natcp.service" ]; then
    cp -f "${CUR_PATH}/deployments/natcp.service" /usr/lib/systemd/system/natcp.service
    systemctl daemon-reload 2>/dev/null || true
fi

if [[ "${NoAutoStart}" -eq 0 ]]; then
    systemctl restart "${SERVICE_NAME}"
    sleep 1
    systemctl status "${SERVICE_NAME}" --no-pager || true
else
    echo "upgrade done, service not started (--no-start)."
fi
