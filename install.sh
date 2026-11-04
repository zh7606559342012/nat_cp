#!/bin/bash
# 安装脚本（仿 jxtx install.sh 的流程，针对 CP 精简：CP 不需要 redis、
# td-agent、ncpa、ntp 等 jxtx agent 的依赖，只装 CP 自身）。
#
# 用法（在解压后的安装包目录内执行，需 root）：
#   ./install.sh
#
# 效果：
#   - 程序装到 /opt/nat_cp/bin/cp，配置到 /opt/nat_cp/bin/conf/cp.yaml
#   - 脚本装到 /opt/nat_cp/scripts/，并提供 /usr/local/bin/cpctl 快捷入口
#   - 注册 systemd 服务 natcp（/usr/lib/systemd/system/natcp.service），
#     开机自启，并立即启动
#   - 日志目录 /var/log/nat_cp，数据目录 /var/lib/fusion-access-gw，
#     UP socket 目录 /var/run/fusion-access-gw
#   - 若此前已安装：先停服务，把旧配置备份到 /home/natcp_conf，
#     装完后恢复旧配置（新配置示例不会覆盖你的已有配置）
set -e

CUR_PATH="$(dirname "$(readlink -f "$0")")"
NATCP_ROOT="/opt/nat_cp"
NATCP_BIN="${NATCP_ROOT}/bin"
NATCP_CONF_DIR="${NATCP_BIN}/conf"
LOG_PATH="/var/log/nat_cp"
DB_DIR="/var/lib/fusion-access-gw"
RUN_DIR="/var/run/fusion-access-gw"
CONF_BACKUP="/home/natcp_conf"
SERVICE_NAME="natcp"

die() {
    echo "error: $1" >&2
    exit 1
}

[ "$(id -u)" = "0" ] || die "install.sh must run as root"
[ -f "${CUR_PATH}/bin/cp" ] || die "bin/cp not found, run inside the extracted package dir"
[ -f "${CUR_PATH}/deployments/natcp.service" ] || die "deployments/natcp.service not found"

env_check() {
    # 停掉旧服务（若存在），备份旧配置
    if command -v systemctl >/dev/null 2>&1; then
        systemctl stop "${SERVICE_NAME}" >/dev/null 2>&1 || true
    fi
    rm -rf "${CONF_BACKUP}"
    if [ -f "${NATCP_CONF_DIR}/cp.yaml" ]; then
        mkdir -p "${CONF_BACKUP}"
        cp -f "${NATCP_CONF_DIR}/cp.yaml" "${CONF_BACKUP}/"
        echo "old config backed up to ${CONF_BACKUP}/cp.yaml"
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

install_service() {
    cp -f "${CUR_PATH}/deployments/natcp.service" /usr/lib/systemd/system/natcp.service
    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}"
}

install_logrotate() {
    cat > /etc/logrotate.d/natcp << EOF
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
install_service
install_logrotate

echo "starting ${SERVICE_NAME}..."
systemctl restart "${SERVICE_NAME}"
sleep 1
systemctl status "${SERVICE_NAME}" --no-pager || true
echo
echo "installed. control it with: cpctl {start|stop|restart|status}"
echo "config: ${NATCP_CONF_DIR}/cp.yaml"
echo "logs:   journalctl -u ${SERVICE_NAME} -f  (or ${LOG_PATH}/cp.log when run via cpctl pidfile mode)"
