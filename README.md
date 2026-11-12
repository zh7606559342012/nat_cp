# nat_cp — Fusion Access GW 控制面（CP）

实现《Fusion Access GW CP 接口与数据结构设计说明书》V0.5（DNN 事件驱动
全量补偿版）的 CP 侧完整交互代码：CP ↔ UPF（智云随行北向 REST）、
CP ↔ UP（socket 规则下发）、OAM → CP（静态配置）。

代码分层刻意对齐 jxtx 项目 dev 分支的目录结构（见下表），
便于熟悉 jxtx 的同事直接上手；Go 依赖只有 Gin、Logrus、SQLite3、YAML，
与 jxtx 技术栈一致。

## 目录结构

```
main/
  main.go                  # 程序入口：配置加载 → 日志初始化 → 数据库初始化 → modules.Run
  conf/                    # 配置加载、日志初始化、缓存初始化（不可变 Snapshot）& 示例配置文件
    cp.yaml                # 生产示例配置
    cp-test.yaml           # 联调配置
  webTypes/                # Web 类型定义：CP-UP 信封、ToH/ToB 规则、UPF 会话、配置变更事件
  models/                  # 数据模型与数据库操作服务层：SQLite 实体 + Repository（CRUD/事务/校验）
  httpServer/              # 数据库连接初始化（SQLite3）与建表
  upf/                     # UPF REST 客户端：token、ping、DNN 列表、全量会话、订阅
  up/                      # UP socket 客户端：UDS + NDJSON 信封、ADD/DELETE、SNAPSHOT
  handler/                 # 业务处理层：会话匹配、规则生成、DNN 定向全量补偿、批量同步
  controllers/             # HTTP 控制器层：UPF 回调、OAM 配置接口、健康检查与调试状态
  server/                  # Gin 路由注册与中间件
  modules/                 # 模块初始化与启动编排（12 步启动顺序）
  monitor/                 # 监控模块：UPF 链路心跳、订阅补注册
  crontab/                 # 定时任务：pending DNN 补偿重试
  utils/                   # 工具函数：requestId、时间戳
  mock/                    # 联调用假 UPF + 假 UP（仅开发测试用）
  scripts/                 # Shell 脚本集：cpctl、start_all.sh、stop_all.sh、status_all.sh
make_package.sh            # 编译打包脚本
install.sh                 # 一键安装脚本
upgrade.sh                 # 平滑升级脚本
initial_compile.sh         # Go 工具链初始化脚本
smoke_test.sh              # 本地联调冒烟脚本
go.mod / go.sum
README.md
```

## 与 jxtx 目录的对照

| jxtx（dev 分支） | nat_cp 对应 | 说明 |
| --- | --- | --- |
| main/main.go | main/main.go | 入口编排：配置 → 日志 → 数据库 → modules |
| main/conf/ | main/conf/ | 配置加载、日志初始化、缓存初始化（CP 的缓存即不可变 Snapshot） |
| main/webTypes/ | main/webTypes/ | 共享类型定义 |
| main/models/ | main/models/ | 数据模型与数据库操作服务层 |
| main/httpServer/ | main/httpServer/ | 数据库连接初始化（CP 只用 SQLite3，不用 Redis） |
| main/rabbitMQ/ | main/upf/ + main/up/ | 外部连接层：jxtx 接 RabbitMQ，CP 接 UPF REST 与 UP socket |
| main/handler/ | main/handler/ | 业务处理层 |
| main/controllers/ | main/controllers/ | HTTP 控制器层 |
| main/server/ | main/server/ | Gin 路由注册与中间件 |
| main/modules/ | main/modules/ | 模块初始化与启动编排 |
| main/monitor/ | main/monitor/ | 监控模块 |
| main/crontab/ | main/crontab/ | 定时任务 |
| main/utils/ | main/utils/ | 工具函数 |
| main/scripts/ | main/scripts/ | Shell 脚本集 |
| make_package.sh / install.sh / upgrade.sh / initial_compile.sh | 同名 | 打包、安装、升级、工具链初始化（install 精简掉了 CP 不需要的 redis、td-agent、ncpa、ntp） |

**刻意没有照搬的目录**：jxtx 的 `command/`、`commandMQ/`（MQ 命令与连接）、
`telnetcmd/`（Telnet 调试）、`trace/`（数据面抓包）、`uploads/`（上传暂存）
是 OMS Agent 特有的，CP 对外只用 UPF REST 与 UP socket，没有 MQ、
Telnet 调试、抓包与上传需求，故不设这些目录，避免空目录误导。

分层依赖方向（只允许上层依赖下层，不反向）：

```
webTypes / utils
  ← models / httpServer / upf / up
    ← conf（含 Snapshot 构建）
      ← handler（业务核心）
        ← controllers / monitor / crontab
          ← server（路由装配）
            ← modules（编排）
              ← main（入口）
```

## 核心流程（说明书 V1.1 第 3 章）

1. **启动**：加载 SQLite 静态配置 → 构建内存索引 → 启动回调与 OAM 服务 →
   Token → Ping → DNN List → EffectiveDNN（Configured ∩ Upf）→
   按 DNN 分页拉全量 Session → 匹配生成期望规则 →
   RULE_SYNC SNAPSHOT 批量下发 UP → 注册订阅 → RUNNING。
   启动全量只收集期望规则，不逐条下发（见 3.9.23）。
2. **增量**：UPF POST create/delete 到回调地址，CP 按 IMSI+DNN 匹配，
   生成/删除 ToH/ToB 规则；UP 在 ACK 中返回从 SNAT 地址段内分配的
   具体 IP（`allocatedSrcNatIp`），CP 记入安装状态。
3. **运行期 DNN 新增事件驱动全量补偿（V0.5）**：OAM 新增 IMSI+DNN
   绑定后，配置模块在 SQLite 事务提交并原子刷新 Snapshot 后发布
   `DNN_CONFIG_CHANGED`；业务模块只对该 DNN 定向拉一次全量会话，
   补齐配置前已在线的 UE。同一 DNN 执行期间的多次事件合并为
   running + dirty；链路/Token/DNN 不可用时标记 pending，恢复后由
   crontab 定时重试。删除配置不触发 UPF 全量查询，直接重算受影响
   会话删规则。
4. **批量恢复**：CP/UP 重启或主备切换后，用 RULE_SYNC SNAPSHOT
   按批次（默认 500 条）重放期望规则；逐规则失败走单条 UPSERT
   只重试失败项，整批调用失败的规则保留在期望集等下次快照；
   实时事件优先级高于快照（按 Desired 版本校验，旧 ACK 不覆盖）。
   UP 返回失败或超时且重试耗尽时直接删除规则（见 3.9.19 注记）。

## 启动使用

### 方式一：本地直接跑（开发联调用）

```bash
./initial_compile.sh       # 检查 Go >= 1.22，设置 GO111MODULE 与 GOPROXY
go build -o cp ./main
./cp -config main/conf/cp.yaml       # 生产示例配置，先改里面的 UPF 地址和 pwdMD5
./cp -config main/conf/cp-test.yaml  # 联调配置，配合 main/mock 使用
```

### 方式二：打包部署到目标机（仿 jxtx 的 make_package/install 流程）

```bash
# 1. 在开发机上打包（需要 Go 环境），得到 fagw-cp_setup_v0.1.tar.gz
#    注意：从 git 刚拉下来的脚本可能没有执行权限，先 chmod 一次
chmod +x make_package.sh install.sh upgrade.sh initial_compile.sh main/scripts/*
./make_package.sh v0.1

# 2. 把 tar.gz 传到目标机，解压后执行安装（需 root）
tar -xzf fagw-cp_setup_v0.1.tar.gz
cd fagw-cp_setup_v0.1
./install.sh
```

安装完成后程序在 `/opt/fagw/fagw-cp/bin/cp`（与 OAM 的 `/opt/fagw/fagw-oam` 同级），配置在
`/opt/fagw/fagw-cp/bin/conf/cp.yaml`（首次安装装的是示例配置，**启动前
务必先改** `upf.baseURL`、`upf.pwdMD5`（32 位小写 MD5，不要放明文密码）、
`upf.callbackURL` 和 `up.socket`），systemd 服务名为 `fagw-cp`，开机自启，
日志由程序自己写到 `/var/log/fagw-cp/cp.log`（配置 `logPath`，install.sh 已配好
logrotate：每天、保留 10 份、单份 20M、copytruncate，与 jxtx 的 agent.log
策略一致）；systemd 的 journal 里只剩启动早期（读配置失败等）或日志
文件打不开时的输出。从旧版本（`/opt/nat_cp` + `natcp.service`）升级会自动
停用旧服务并沿用旧配置。

### 方式三：Windows 本地开发

本地与生产用**同一个驱动**（`mattn/go-sqlite3`，CGO），不存在“本地通过、上线行为不同”的问题。
代价是构建机必须有 gcc，所以 **Windows 只负责开发，出包统一在 Ubuntu 上做**。

| 依赖 | 要求 |
|---|---|
| Go | 1.22+（本机放在 `D:\go1.22`） |
| gcc | MinGW-w64，x86_64 目标（本机放在 `D:\mingw64`，GCC 16.2.0） |
| 不能用 | MinGW.org 32 位发行版（如 `D:\MinGW`，无法作为 64 位 Go 的 CGO 编译器） |

> **本机辅助文件不入库**：`dev-win.bat`、`main/conf/cp-win.yaml`、`.vscode/` 内含
> `D:\go1.22`、`D:\mingw64`、仓库绝对路径等硬编码，换一台机器就不成立，已被 `.gitignore` 排除。
> 换机器时按下面约定自建。

等价的手工命令（Git Bash）：

```bash
export CGO_ENABLED=1 GOTOOLCHAIN=local
export PATH="/d/mingw64/bin:/d/go1.22/go/bin:$PATH"
go build -o bin/cp.exe   ./main
go build -o bin/mock.exe ./main/mock
```

启动顺序与端口：mock UPF `18153`、mock UP UDS `data/up.sock`、
CP `18080`（UE 回调）/ `10001`（OAM）/ `17070`（pprof）。

`cp-win.yaml` 有两个 Windows 专属的坑：

1. `dbPath` 写**相对路径**（`data/cp.db`）——`file:` DSN 按 URL 解析，`D:/x` 会被当成
   scheme 截断；相对路径最稳，所以启动时 cwd 必须是仓库根目录。
2. `up.socket` 写 **Windows 绝对路径**（`unix://D:\...\data\up.sock`），且必须与 mock 的
   `-up-sock` 完全一致——AF_UNIX 在 Windows 上要求文件系统路径，`/tmp/xxx` 会落到当前盘符根目录下。

首次编译约 2 分钟（全量编译 `sqlite3.c`），之后走编译缓存。日志落在 `data\`（已忽略）。

> 因为用 CGO，**不能**在 Windows 上交叉编译出 Linux 二进制（MinGW 只产出 Windows 目标）。
> 出包在目标架构的 Ubuntu 上执行 `./initial_compile.sh && ./make_package.sh v0.1`。

### 断点调试（VSCode）

本地 `.vscode/launch.json`（不入库）里配两个 launch 配置 + 一个 compound
**CP + mock（本地全链路）**，F5 即可同时以调试模式拉起两进程，断点可以直接打在
`handler` / `upf` / `up` 任何位置。执行顺序上 mock 先起、CP 后起；CP 首次 dial 若失败会自动重试，
日志里几条 `get upf token failed` 属正常。

两个要点：

- 两进程读不同配置——CP 用 `main/conf/cp-win.yaml`，mock 用 `-up-sock` 指向同一个 socket。
- debug 配置的 `env` 必须带上 `CGO_ENABLED=1`、`GOTOOLCHAIN=local`、`PATH` 含 mingw 的 `bin`，
  否则 dlv 会用系统里的旧 Go 编译，报一堆找不到 `sqlite3.h` 的错。

前提是 dlv 与 Go 版本匹配：**dlv ≤ 1.21 不支持 Go 1.22**，会报
`Version of Delve is too old for Go version 1.22.12`（默认不打日志，表现为静默退出）。升级：

```bash
go install github.com/go-delve/delve/cmd/dlv@v1.24.2
```


### 联调怎么造场景

三个 mock 辅助接口就是"人造流量"的入口：

```bash
# 看假 UPF/假 UP 的内部状态
curl --noproxy '*' http://127.0.0.1:18153/__mock/state
# 看 UP 已安装的规则（CP 下发了什么）
curl --noproxy '*' http://127.0.0.1:18153/__mock/up/rules
# 模拟 UE 上线 / 下线（向 CP 的回调地址推送）
curl --noproxy '*' -X POST http://127.0.0.1:18153/__mock/trigger \
     -d '{"action":"create","imsi":"460001111111111","dnn":"area1","privateIpv4":"20.20.1.1"}'
curl --noproxy '*' -X POST http://127.0.0.1:18153/__mock/trigger \
     -d '{"action":"delete","imsi":"460001111111111","dnn":"area1","privateIpv4":"20.20.1.1"}'
```

OAM 侧（10001）用同一套 `call` 接口下发静态配置，完整例子见 `smoke_test.sh`。

### 启停、状态、日志、升级、卸载

```bash
cpctl start            # 启动（等价于 main/scripts/start_all.sh）
cpctl stop             # 停止（等价于 main/scripts/stop_all.sh）
cpctl restart          # 重启
cpctl status           # 看状态（等价于 main/scripts/status_all.sh）
tail -f /var/log/fagw-cp/cp.log # 跟日志（程序自己写文件，systemd / cpctl 两种模式通用）
journalctl -u fagw-cp -f        # 只看 journal（启动早期输出，日志文件打不开时才有内容）

# 升级：新包解压后执行（默认自动重启；--no-start 只替换不重启）
./upgrade.sh
./upgrade.sh --no-start

# 卸载
systemctl disable --now fagw-cp
rm -rf /opt/fagw/fagw-cp /usr/local/bin/cpctl /usr/lib/systemd/system/fagw-cp.service
# 配置、SQLite（/var/lib/fusion-access-gw/cp.db）、日志按需保留或删除
```

说明：`cpctl` 在有 systemd 的目标机上走 `systemctl`；在没有 systemd
的开发机/容器里自动退化为 pidfile 模式（nohup 后台运行），
可用 `NATCP_HOME`、`NATCP_CONFIG`、`NATCP_PIDFILE`、`NATCP_LOGFILE`
环境变量覆盖默认路径。

## 本地联调（假 UPF + 假 UP）

```bash
# 终端 1：启动 mock（假 UPF 在 127.0.0.1:18153，假 UP 在 /tmp/mock-up.sock）
go run ./main/mock

# 终端 2：启动 CP（联调配置）
rm -f /tmp/cp-test.db*
go run ./main -config main/conf/cp-test.yaml

# 或一键跑冒烟脚本（OAM 下发 → 定向补偿 → 下线删除 → 批量同步全流程）
bash smoke_test.sh
```

联调手工验证（CP 启动后按顺序执行）：

```bash
# 1. OAM 下发 ToH 配置：VNI 范围 + IMSI 绑定（area1）
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t1","command":"add_vxlan","reqArgs":{"id":1,"vniBegin":100,"vniEnd":199,"localVtepIp":"172.16.123.100","remoteVtepIp":"10.88.123.100","localOverlayMac":"20:88:10:91:15:22","remoteOverlayMac":"86:1f:ac:b1:79:8a","srcNatAddr":"10.10.0.0/24"}}'
# 1.1 修改已有配置：其余字段全量下发（缺字段会被拒，避免误清空），
#     vniBegin/vniEnd 可以不带——OAM 的 mod 报文不带这两个字段，CP 会沿用库中
#     已有范围（范围本就不可变）。只带其中一个会报错，要求两个一起给。
#     换范围须先 del_vxlan 再 add_vxlan，并重新下发该范围内的绑定；
#     del_vxlan 仅当该 VNI 范围内已无 imsi 绑定时才成功，否则报错提示先删绑定
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t1b","command":"mod_vxlan","reqArgs":{"id":1,"localVtepIp":"172.16.123.100","remoteVtepIp":"10.88.123.100","localOverlayMac":"20:88:10:91:15:22","remoteOverlayMac":"86:1f:ac:b1:79:8a","srcNatAddr":"10.10.9.0/24"}}'
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t2","command":"add_vxlan_imsi","reqArgs":{"id":2,"imsi":"460001111111111","dnn":"area1","vni":100}}'
# 触发 area1 定向全量补偿：mock 中 area1 已在线的 20.20.1.1 应被安装
sleep 2
curl -s http://127.0.0.1:18153/__mock/up/rules

# 2. OAM 下发 ToB 配置，触发 area2 定向全量补偿
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t3","command":"add_fttrb","reqArgs":{"id":3,"fttrbId":"FTTRB-1","srcNatAddr":"10.10.1.0/24"}}'
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"t4","command":"add_fttrb_imsi","reqArgs":{"id":4,"imsi":"460002222222222","dnn":"area2","fttrbId":"FTTRB-1"}}'
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

# 5. 查询列表：四个 list_* 都支持分页
#    reqArgs 传 [offset, limit]：第一个是下标（0 起），第二个是数量
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"q1","command":"list_vxlan","reqArgs":[0,10]}'
#    不传 / {} / [] 返回全部；limit=0 表示不限条数；单次最多 1000 条
#    等价写法：{"offset":0,"limit":10}（limit 也接受别名 count）
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"q2","command":"list_vxlan_imsi","reqArgs":{"offset":0,"limit":10}}'
#    响应：{"uuid":"q1","ret":0,"body":{"ret":0,"total":23,"records":[...]}}
#    body.records 始终是数组（无数据时为 []），body.total 为满足条件的总条数；
#    记录字段名（全部 camelCase，主键是 id）：
#      vxlan_config        id / vniBegin / vniEnd / localVtepIp / remoteVtepIp /
#                          localOverlayMac / remoteOverlayMac / srcNatAddr /
#                          createdAt / updatedAt
#      vxlan_imsi_binding  id / imsi / dnn / vni / createdAt / updatedAt
#      fttrb_config        id / fttrbId / srcNatAddr / createdAt / updatedAt
#      fttrb_imsi_binding  id / imsi / dnn / fttrbId / createdAt / updatedAt
#    list_vxlan / list_vxlan_imsi / list_fttrb / list_fttrb_imsi 规则一致
#
#    id 是 uint32，收发都是 JSON 数字："id":999101。
#    DB 的 id 列虽然是 TEXT，但 database/sql 会用内置的 ParseUint 完成文本与
#    数字的互转，落库仍是 '999101'，读出即 uint32，不需要自定义类型。

# 6. 删除：四个 del_* 只传一个 id（uint32），数组与对象两种写法都接受
#      del_vxlan         [11]   或  {"id": 11}
#      del_fttrb         [11]
#      del_vxlan_imsi    [11]
#      del_fttrb_imsi    [11]
#    id 必须是 1..4294967295 的数字，字符串形态（"11" / ["11"]）会直接报错；
#    对象里的键名大小写不敏感（{"ID":...} 也能解析），多余字段会被忽略。
#    成功响应：{"uuid":"d1","ret":0,"body":{"ret":0,"msg":"删除成功"}}
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"d1","command":"del_vxlan_imsi","reqArgs":[11]}'
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"d2","command":"del_vxlan","reqArgs":{"id":1}}'
```

字段类型：`id` 是数字，`imsi` / `fttrbId` 是文本：

```bash
# id：uint32，必须发 JSON 数字。传 "11" / ["11"] 会报错
#     （reqArgs.id must be a uint32 number），不做字符串兼容。
#     入参键名写 "ID" 也能解析（Go 的 encoding/json 匹配字段名大小写不敏感），
#     但出参一律输出小写 id，请统一按小写收发。
#
# imsi / fttrbId：文本字段，但两种形态都收，方便 OAM 直接下发：
#   {"imsi":"460001111111111"}  与  {"imsi":460001111111111}  等价
#   {"fttrbId":"F1"}            与  {"fttrbId":123213}        等价
# 原因：15 位 imsi 用 JS 拼 JSON 极易退化成 number，
# 若只按 string 接收会报 cannot unmarshal number ... of type string。
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"n1","command":"add_fttrb","reqArgs":{"id":999001,"fttrbId":777888,"srcNatAddr":"10.10.2.0/24"}}'
curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d \
 '{"uuid":"n2","command":"del_fttrb","reqArgs":[999001]}'
```

### OAM 统一调用 /cp/v1/common/call 的响应信封

所有响应都是**三层同构**：`{uuid, ret, body}`。`ret` 表示成败，`0` 成功、`-9` 失败，
外层与 `body` 内的 `ret` 同值。区别只在 `body` 的形状——查询装数据，增删改装结果。

```jsonc
// 查询成功（list_*）：body 里 records 为数据数组，total 为满足条件的总条数
{"uuid": "q1", "ret": 0,
 "body": { "ret": 0, "total": 2, "records": [ /* ... */ ] }}

// 增删改成功（add_* / mod_* / del_* / batch_*）：body 只有 ret 和 msg，
// msg 是中文动作描述
{"uuid": "a1", "ret": 0,
 "body": { "ret": 0, "msg": "新增成功" }}

// 失败（查询失败与写命令失败同形）：body 内 ret = -9，原因在 msg，
// 不带 total / records
{"uuid": "a2", "ret": -9,
 "body": { "ret": -9, "msg": "vxlan 1 already exists, use mod_vxlan to modify it" }}
```

| 字段 | 出现场景 | 含义 |
|---|---|---|
| `uuid` | 全部 | 原样回显请求的 `uuid`，供 OAM 做请求-响应匹配 |
| `ret` | 全部 | `0` 成功，`-9` 失败；与 `body.ret` 同值 |
| `body` | 全部 | 业务体，形状随命令类型不同，见下 |
| `body.ret` | 全部 | 同上，与外层 `ret` 一致 |
| `body.total` | 仅查询成功 | 满足条件的总条数（不受分页影响） |
| `body.records` | 仅查询成功 | 数据数组，无数据时为 `[]`，不会是 `null` |
| `body.msg` | 增删改 + 全部失败 | 成功为「新增/修改/删除/批量新增/批量删除 + 成功」；失败为具体原因 |

`body` 只有两种形状：

- 查询成功 → `{ret, total, records}`
- 增删改成功、所有失败 → `{ret, msg}`

失败时不补 `total` / `records`——失败时业务数据没有意义。
记录字段与 `reqArgs` 同名（camelCase）。
传参形态：`add_*` / `mod_*` / `batch_*` 用对象（`{"id":...}`），
`del_*` 用长度为 1 的数字数组（`[11]`）或对象（`{"id":11}`），
`list_*` 用分页数组（`[offset, limit]`）。
注意 `/ue/callback`（UPF 回调）与 `/debug/*` 仍是各自的 `{code, message}` 格式，
不走这套信封。

### 旧库升级：字符串 ID 自动迁移

acb91f99 之前主键 id 是字符串（如 vx1/b1）。新版启动时 `migrate()` 会自动
把四张表里解析不成数字（或为 0）的 id 重编号（取该表最大数字 id +1 起），
其余字段不动；数字串（如 '999101'）本身就能读，不用迁。迁移过程记在日志里
（`migrated non-numeric config id to uint32`）。绑定表的外键只认 fttrb_id /
VNI，不认数字 id，所以改 id 不影响关联。OAM 侧若还存着旧字符串 id，迁移后
需按 `list_*` 返回的新数字 id 重新对齐。

## CP-UP socket 约定（待联调冻结，见说明书第 12 章 Q1）

- 传输：Unix Domain Socket，默认 `unix:///var/run/fusion-access-gw/up.sock`。
- 帧格式：NDJSON，每条消息一行 JSON，CP 写一行、UP 回一行。
- 信封：`{requestId, msgType, operation, payload}`；
  响应：`{requestId, code, message, data}`。
- 错误码：0 成功；1001 参数非法（不重试）；1002 删除时规则不存在
  （按幂等成功）；2001 资源不足；2002 内部失败（有限重试）。
- 当前实现 IPv4（Q3），SNAT 分配策略与重复 UPSERT 是否保持原 IP
  待联调冻结（Q2）。

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
- pprof 调试服务默认只监听 `127.0.0.1:17070`，`pprof.listen` 为空则不启动。
- 本仓库代码为 CP 侧实现；UPF 网元内部实现与 UP 数据面转发实现
  不在范围内。
