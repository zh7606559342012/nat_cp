#!/bin/bash
# 安装脚本（仿 jxtx install.sh 的流程，针对 CP 精简：CP 不需要 redis、
# td-agent、ncpa、ntp 等 jxtx agent 的依赖，只装 CP 自身；
# systemd 单元文件仿 jxtx _gen_agent_service 由本脚本现场生成）。
#
# 用法（在解压后的安装包目录内执行，需 root）：
#   ./install.sh
#
# 效果：
#   - 程序装到 /opt/fagw/fagw-cp/bin/cp，配置到 /opt/fagw/fagw-cp/bin/conf/cp.yaml
#     （与 OAM 的 /opt/fagw/fagw-oam 同级，遵循 fagw 组件的目录惯例）
#   - 脚本装到 /opt/fagw/fagw-cp/scripts/，并提供 /usr/local/bin/cpctl 快捷入口
#   - 注册 systemd 服务 fagw-cp（/usr/lib/systemd/system/fagw-cp.service），
#     开机自启，并立即启动
#   - 日志目录 /var/log/fagw-cp，数据目录 /var/lib/fusion-access-gw，
#     UP socket 目录 /var/run/fusion-access-gw
#   - 若此前已安装：先停服务，把旧配置备份到 /home/fagw-cp_conf，
#     装完后恢复旧配置（新配置示例不会覆盖你的已有配置）
#   - 兼容旧版本：自动停用 natcp.service，并从 /home/natcp_conf 迁移旧配置
set -e

CUR_PATH="$(dirname "$(readlink -f "$0")")"
NATCP_ROOT="/opt/fagw/fagw-cp"
NATCP_BIN="${NATCP_ROOT}/bin"
NATCP_CONF_DIR="${NATCP_BIN}/conf"
LOG_PATH="/var/log/fagw-cp"
DB_DIR="/var/lib/fusion-access-gw"
RUN_DIR="/var/run/fusion-access-gw"
CONF_BACKUP="/home/fagw-cp_conf"
LEGACY_CONF_BACKUP="/home/natcp_conf"
SERVICE_NAME="fagw-cp"
LEGACY_SERVICE_NAME="natcp"

die() {
    echo "error: $1" >&2
    exit 1
}

[ "$(id -u)" = "0" ] || die "install.sh must run as root"
[ -f "${CUR_PATH}/bin/cp" ] || die "bin/cp not found, run inside the extracted package dir"

env_check() {
    # 停掉旧服务（若存在）
    if command -v systemctl >/dev/null 2>&1; then
        systemctl stop "${SERVICE_NAME}" >/dev/null 2>&1 || true
        # 旧版本（/opt/nat_cp + natcp.service）：停用后再装新版，
        # 否则两个实例会抢 10001/18080 端口。
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
        echo "old config backed up to ${CONF_BACKUP}/cp.yaml"
    elif [ -f "${LEGACY_CONF_BACKUP}/cp.yaml" ]; then
        # 首次装到新目录、旧版本还在 /opt/nat_cp：沿用旧配置，
        # 避免把示例配置装上导致 active/端口被改。
        mkdir -p "${CONF_BACKUP}"
        cp -f "${LEGACY_CONF_BACKUP}/cp.yaml" "${CONF_BACKUP}/"
        echo "config migrated from ${LEGACY_CONF_BACKUP}/cp.yaml"
    fi
    mkdir -p "${NATCP_ROOT}" "${NATCP_BIN}" "${NATCP_CONF_DIR}" "${LOG_PATH}" "${DB_DIR}" "${RUN_DIR}"
}

install_cp() {
    cp -f "${CUR_PATH}/bin/cp" "${NATCP_BIN}/"
    chmod +x "${NATCP_BIN}/cp"
    # 配置：有备份就恢复备份，否则装示例配置
    if [ -f "${CONF_BACKUP}/cp.yaml" ]; then
        cp -f "${CONF_BACKUP}/cp.yaml" "${NATCP_CONF_DIR}/"
        echo "restored previous config"
    else
        cp -f "${CUR_PATH}/bin/conf/cp.yaml" "${NATCP_CONF_DIR}/"
        echo "installed example config, edit it before starting: ${NATCP_CONF_DIR}/cp.yaml"
    fi
    if [ -f "${CUR_PATH}/bin/conf/cp_version.txt" ]; then
        cp -f "${CUR_PATH}/bin/conf/cp_version.txt" "${NATCP_CONF_DIR}/"
    fi
    mkdir -p "${NATCP_ROOT}/scripts"
    cp -f "${CUR_PATH}/scripts/"* "${NATCP_ROOT}/scripts/"
    chmod +x "${NATCP_ROOT}/scripts/"*
    ln -sf "${NATCP_ROOT}/scripts/cpctl" /usr/local/bin/cpctl
}

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

    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}"
}

install_logrotate() {
    cat > /etc/logrotate.d/${SERVICE_NAME} << EOF
${LOG_PATH}/cp.log {
    daily
    rotate 10
    size 20M
    missingok
    notifempty
    copytruncate
}
EOF
}

env_check
install_cp
_gen_service
install_logrotate

echo "starting ${SERVICE_NAME}..."
systemctl restart "${SERVICE_NAME}"
sleep 1
systemctl status "${SERVICE_NAME}" --no-pager || true
echo
echo "installed. control it with: cpctl {start|stop|restart|status}"
echo "config: ${NATCP_CONF_DIR}/cp.yaml"
echo "logs:   journalctl -u ${SERVICE_NAME} -f  (or ${LOG_PATH}/cp.log when run via cpctl pidfile mode)"
