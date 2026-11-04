# nat_cp — Fusion Access GW 控制面（CP）

实现《Fusion Access GW CP 接口与数据结构设计说明书》V0.5（DNN 事件驱动
全量补偿版）的 CP 侧完整交互代码：CP ↔ UPF（智云随行北向 REST）、
CP ↔ UP（socket 规则下发）、OAM → CP（静态配置）。

参考实现风格：jxtx 项目 dev 分支（Go + Gin + Logrus + SQLite + YAML 配置）。

## 目录结构

```
cmd/cp        CP 主程序（启动编排见 internal/app）
cmd/mock      本地联调用的假 UPF + 假 UP（仅开发测试用）
configs/      cp.yaml（生产示例）、cp-test.yaml（联调配置）
internal/
  model/      共享数据结构：CP-UP 信封、ToH/ToB 规则、UPF 会话、事件
  config/     YAML 配置加载与默认值
  store/      SQLite 持久化（5 张静态表 + 事务 + 快照构建）
  snapshot/   不可变配置快照：BindingIndex、VNI 范围二分、FTTRB 索引
  upf/        UPF 客户端：token、ping、DNN 列表、全量会话、订阅
  up/         UP 客户端：UDS + NDJSON 信封、ADD/DELETE、SNAPSHOT
  engine/     业务核心：会话匹配、规则生成、DNN 定向全量补偿、批量同步
  callback/   UPF 推送回调服务（POST create/delete）
  oam/        OAM 配置接口（事务落库 → 快照刷新 → 配置变更事件）
  app/        启动顺序：配置 → 索引 → 回调服务 → Token → Ping →
              DNN List → EffectiveDNN → 全量恢复 → 订阅 → RUNNING
```

## 核心流程（说明书 5.10）

1. **启动**：加载 SQLite 静态配置 → 构建内存索引 → 启动回调服务 →
   Token → Ping → DNN List → EffectiveDNN（Configured ∩ Upf）→
   按 DNN 分页拉全量 Session → 匹配生成规则 → ADD/UPSERT 下发 UP →
   注册订阅 → RUNNING。
2. **增量**：UPF POST create/delete 到回调地址，CP 按 IMSI+DNN 匹配，
   生成/删除 ToH/ToB 规则；UP 在 ACK 中返回从 SNAT 地址段内分配的
   具体 IP（`allocatedSrcNatIp`），CP 记入安装状态。
3. **运行期 DNN 新增事件驱动全量补偿（V0.5）**：OAM 新增 IMSI+DNN
   绑定后，配置模块在 SQLite 事务提交并原子刷新 Snapshot 后发布
   `DNN_CONFIG_CHANGED`；业务模块只对该 DNN 定向拉一次全量会话，
   补齐配置前已在线的 UE。同一 DNN 执行期间的多次事件合并为
   running + dirty；链路/Token/DNN 不可用时标记 pending，恢复后重试。
   删除配置不触发 UPF 全量查询，直接重算受影响会话删规则。
4. **批量恢复**：CP/UP 重启或主备切换后，用 RULE_SYNC SNAPSHOT
   按批次（默认 500 条）重放期望规则；失败只重试失败项；
   实时事件优先级高于快照（按 Desired 版本校验，旧 ACK 不覆盖）。

## CP-UP socket 约定（待联调冻结，见说明书第 12 章 Q1）

- 传输：Unix Domain Socket，默认 `unix:///var/run/fusion-access-gw/up.sock`。
- 帧格式：NDJSON，每条消息一行 JSON，CP 写一行、UP 回一行。
- 信封：`{requestId, msgType, operation, payload}`；
  响应：`{requestId, code, message, data}`。
- 错误码：0 成功；1001 参数非法（不重试）；1002 删除时规则不存在
  （按幂等成功）；2001 资源不足；2002 内部失败（有限重试）。
- 当前实现 IPv4（Q3），SNAT 分配策略与重复 UPSERT 是否保持原 IP
  待联调冻结（Q2）。

## 启动使用

### 方式一：本地直接跑（开发联调用）

```bash
go build -o cp ./cmd/cp
./cp -config configs/cp.yaml          # 生产示例配置，先改里面的 UPF 地址和 pwdMD5
./cp -config configs/cp-test.yaml     # 联调配置，配合 cmd/mock 使用
```

### 方式二：打包部署到目标机（仿 jxtx 的 make_package/install 流程）

```bash
# 1. 在开发机上打包（需要 Go 环境），得到 natcp_setup_v0.1.tar.gz
#    注意：从 git 刚拉下来的脚本可能没有执行权限，先 chmod 一次
chmod +x make_package.sh install.sh upgrade.sh scripts/*
./make_package.sh v0.1

# 2. 把 tar.gz 传到目标机，解压后执行安装（需 root）
tar -xzf natcp_setup_v0.1.tar.gz
cd natcp_setup_v0.1
./install.sh
```

安装完成后程序在 `/opt/nat_cp/bin/cp`，配置在
`/opt/nat_cp/bin/conf/cp.yaml`（首次安装装的是示例配置，**启动前
务必先改** `upf.baseURL`、`upf.pwdMD5`（32 位小写 MD5，不要放明文密码）、
`upf.callbackURL` 和 `up.socket`），systemd 服务名为 `natcp`，开机自启。

### 启停、状态、日志、升级、卸载

```bash
cpctl start            # 启动（等价于 scripts/start_all.sh）
cpctl stop             # 停止（等价于 scripts/stop_all.sh）
cpctl restart          # 重启
cpctl status           # 看状态（等价于 scripts/status_all.sh）
journalctl -u natcp -f # 跟日志（systemd 模式）

# 升级：新包解压后执行（默认自动重启；--no-start 只替换不重启）
./upgrade.sh
./upgrade.sh --no-start

# 卸载
systemctl disable --now natcp
rm -rf /opt/nat_cp /usr/local/bin/cpctl /usr/lib/systemd/system/natcp.service
# 配置、SQLite（/var/lib/fusion-access-gw/cp.db）、日志按需保留或删除
```

说明：`cpctl` 在有 systemd 的目标机上走 `systemctl`；在没有 systemd
的开发机/容器里自动退化为 pidfile 模式（nohup 后台运行），
可用 `NATCP_HOME`、`NATCP_CONFIG`、`NATCP_PIDFILE`、`NATCP_LOGFILE`
环境变量覆盖默认路径。

## 构建与运行

```bash
go mod tidy
go build ./...
./cp -config configs/cp.yaml
```

## 本地联调（假 UPF + 假 UP）

```bash
# 终端 1：启动 mock（假 UPF 在 127.0.0.1:18153，假 UP 在 /tmp/mock-up.sock）
go run ./cmd/mock

# 终端 2：启动 CP（联调配置）
rm -f /tmp/cp-test.db*
go run ./cmd/cp -config configs/cp-test.yaml
```

随后按顺序验证：

```bash
# 1. OAM 下发 ToH 配置：VNI 范围 + IMSI 绑定（area1）
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t1","command":"add_vxlan","reqArgs":{"id":"vx1","vniBegin":100,"vniEnd":199,"localVtepIp":"172.16.123.100","remoteVtepIp":"10.88.123.100","localOverlayMac":"20:88:10:91:15:22","remoteOverlayMac":"86:1f:ac:b1:79:8a","srcNatAddr":"10.10.0.0/24"}}'
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t2","command":"add_vxlan_imsi","reqArgs":{"id":"b1","imsi":"460001111111111","dnn":"area1","vni":100}}'
# 触发 area1 定向全量补偿：mock 中 area1 已在线的 20.20.1.1 应被安装
sleep 2
curl -s http://127.0.0.1:18153/__mock/up/rules

# 2. OAM 下发 ToB 配置，触发 area2 定向全量补偿
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t3","command":"add_fttrb","reqArgs":{"id":"f1","fttrbId":"FTTRB-1","srcNatAddr":"10.10.1.0/24"}}'
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t4","command":"add_fttrb_imsi","reqArgs":{"id":"b2","imsi":"460002222222222","dnn":"area2","fttrbId":"FTTRB-1"}}'
sleep 2
curl -s http://127.0.0.1:18153/__mock/up/rules

# 3. 模拟 UPF 推送 UE 下线：area1 的规则应被删除
curl -s -X POST http://127.0.0.1:18153/__mock/trigger -d \
 '{"action":"delete","imsi":"460001111111111","dnn":"area1","msisdn":"13800000001","privateIpv4":"20.20.1.1"}'
sleep 1
curl -s http://127.0.0.1:18153/__mock/up/rules

# 4. 查看 CP 运行态与手动触发批量同步
curl -s http://127.0.0.1:18080/debug/state
curl -s -X POST http://127.0.0.1:18080/debug/snapshot-sync
```

## 数据持久化

- SQLite 只持久化 5 张静态表：`schema_meta`、`vxlan_config`、
  `vxlan_imsi_binding`、`fttrb_config`、`fttrb_imsi_binding`（WAL 模式）。
- UPF 会话、期望规则、安装状态只放内存，重启后由 UPF 全量会话
  + UP 批量同步重建。
- 静态配置写采用“事务落库 → 构建新 Snapshot → 原子替换”；
  业务热路径只查内存索引，不查库。

## 注意事项

- 仅 Active CP（`active: true`）向 UPF 注册订阅、向 UP 写规则；
  Standby 只维护配置与索引。
- 心跳失败不删除已有规则，不把 UPF 短时不可达等同于 UE 全部下线。
- 配置文件中 `upf.pwdMD5` 为 32 位小写 MD5，不得放明文密码。
- 本仓库代码为 CP 侧实现；UPF 网元内部实现与 UP 数据面转发实现
  不在范围内。
