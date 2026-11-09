#!/bin/bash
# 升级脚本（仿 jxtx upgrade.sh 的流程）。
#
# 用法（在解压后的新版本安装包目录内执行，需 root）：
#   ./upgrade.sh            # 升级并自动重启服务
#   ./upgrade.sh --no-start # 只替换文件，不重启（-u 同义）
#
# 行为：停服务 → 备份旧配置到 /home/fagw-cp_conf → 替换二进制与脚本 →
# 恢复旧配置 → 刷新 systemd 单元 →（默认）重启服务。
# 配置文件、SQLite 数据库与日志不动。
set -e

CUR_PATH="$(dirname "$(readlink -f "$0")")"
NATCP_ROOT="/opt/fagw/fagw-cp"
NATCP_BIN="${NATCP_ROOT}/bin"
NATCP_CONF_DIR="${NATCP_BIN}/conf"
CONF_BACKUP="/home/fagw-cp_conf"
LEGACY_CONF_BACKUP="/home/natcp_conf"
SERVICE_NAME="fagw-cp"
LEGACY_SERVICE_NAME="natcp"
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

_gen_service() {
cat << EOF > /usr/lib/systemd/system/${SERVICE_NAME}.service
[Unit]
Description=Fusion Access GW Control Plane
After=network.target
Wants=network.target

[Service]
Type=simple
User=root
Group=root
ExecStart=${NATCP_BIN}/cp -config ${NATCP_CONF_DIR}/cp.yaml
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload 2>/dev/null || true
}

# 停服务并备份旧配置（仿 jxtx upgrade.sh 的 env_check）
if command -v systemctl >/dev/null 2>&1; then
    systemctl stop "${SERVICE_NAME}" >/dev/null 2>&1 || true
    # 从旧版本（natcp + /opt/nat_cp）升级时先停用旧服务，避免抢端口
    if systemctl cat "${LEGACY_SERVICE_NAME}" >/dev/null 2>&1; then
        systemctl stop "${LEGACY_SERVICE_NAME}" >/dev/null 2>&1 || true
        if systemctl is-enabled --quiet "${LEGACY_SERVICE_NAME}" 2>/dev/null; then
            systemctl disable "${LEGACY_SERVICE_NAME}" >/dev/null 2>&1 || true
            echo "legacy service ${LEGACY_SERVICE_NAME} stopped and disabled" \
                 "(unit file kept, remove /usr/lib/systemd/system/${LEGACY_SERVICE_NAME}.service when confirmed)"
        fi
    fi
fi
rm -rf "${CONF_BACKUP}"
if [ -f "${NATCP_CONF_DIR}/cp.yaml" ]; then
    mkdir -p "${CONF_BACKUP}"
    cp -f "${NATCP_CONF_DIR}/cp.yaml" "${CONF_BACKUP}/"
elif [ -f "${LEGACY_CONF_BACKUP}/cp.yaml" ]; then
    # 新目录首次安装：沿用旧目录的配置，避免装成示例配置
    mkdir -p "${CONF_BACKUP}"
    cp -f "${LEGACY_CONF_BACKUP}/cp.yaml" "${CONF_BACKUP}/"
    echo "config migrated from ${LEGACY_CONF_BACKUP}/cp.yaml"
fi
mkdir -p "${NATCP_BIN}" "${NATCP_CONF_DIR}" "${NATCP_ROOT}/scripts"

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
_gen_service

if [[ "${NoAutoStart}" -eq 0 ]]; then
    systemctl restart "${SERVICE_NAME}"
    sleep 1
    systemctl status "${SERVICE_NAME}" --no-pager || true
else
    echo "upgrade done, service not started (--no-start)."
fi
