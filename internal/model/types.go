// Package model 定义 CP 内部共享数据结构。
//
// 字段命名与 JSON tag 严格对应《Fusion Access GW CP 接口与数据结构设计说明书》
// V0.5：CP-UP 统一消息信封、ToH/ToB 规则、UPF 会话字段。
// 本包只放数据结构与纯函数，不依赖任何外部服务，便于 OAM 配置模块
// （工程师 A）与业务/UPF/UP 模块（工程师 B）共享。
package model

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// CP-UP 统一消息信封（说明书 3.2 / 3.3）
// ---------------------------------------------------------------------------

// MessageEnvelope 是 CP-UP socket 消息的外层信封。
// CP-UP socket 帧格式约定（联调约定，待 Q1 冻结）：NDJSON，即每条消息为
// 一行 JSON，发送方写一行、接收方回一行。
type MessageEnvelope struct {
	RequestID string          `json:"requestId"`
	MsgType   string          `json:"msgType"`   // TOH_RULE / TOB_RULE / RULE_SYNC
	Operation string          `json:"operation"` // ADD / DELETE / UPSERT / QUERY / SNAPSHOT
	Payload   json.RawMessage `json:"payload"`
}

// ResponseEnvelope 是 UP 对 CP 的通用响应（说明书 3.3）。
type ResponseEnvelope struct {
	RequestID string          `json:"requestId"`
	Code      int             `json:"code"` // 0 成功；非 0 见 UPErrorCode
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// UP 错误码（说明书 6.8，CP 工程建议值）。
const (
	UPCodeOK               = 0    // 成功
	UPCodeInvalidParam     = 1001 // 参数非法：不重试，告警
	UPCodeRuleNotFound     = 1002 // 规则不存在（DELETE）：按幂等成功处理
	UPCodeResourceExhaust  = 2001 // UP 资源不足/地址段无可分配 IP：退避重试并告警
	UPCodeInternalError    = 2002 // UP 内部处理失败：有限次数重试，之后告警
)

// RuleAckData 是 UPSERT 成功 ACK 中的 data（说明书 6.2/6.4/6.6）。
type RuleAckData struct {
	SrcIP             string `json:"srcIp"`
	AllocatedSrcNatIP string `json:"allocatedSrcNatIp"`
}

// ---------------------------------------------------------------------------
// 业务规则（说明书 6.2 / 6.4）
// ---------------------------------------------------------------------------

// ToHRule 是下发给 UP 的 ToH 规则。SrcNatAddr 为 CIDR 地址段，
// 具体 SNAT IP 由 UP 从段内分配并在 ACK 中返回，CP 不自行选址。
type ToHRule struct {
	SrcIP            string `json:"srcIp"`
	VNI              uint32 `json:"vni"`
	LocalVtepIP      string `json:"localVtepIp"`
	RemoteVtepIP     string `json:"remoteVtepIp"`
	LocalOverlayMAC  string `json:"localOverlayMac"`
	RemoteOverlayMAC string `json:"remoteOverlayMac"`
	SrcNatAddr       string `json:"srcNatAddr"` // CIDR
}

// ToBRule 是下发给 UP 的 ToB 规则。
type ToBRule struct {
	SrcIP      string `json:"srcIp"`
	SrcNatAddr string `json:"srcNatAddr"` // CIDR
}

// RuleDelete 是删除规则的 payload：只带原 SRC IP（说明书 6.3/6.5）。
type RuleDelete struct {
	SrcIP string `json:"srcIp"`
}

// BusinessType 业务类型。IMSI+DNN 在 CP 内是唯一业务键，
// 同一个 Key 不能同时属于 ToH 和 ToB（说明书 9.2）。
const (
	BusinessToH = "TOH"
	BusinessToB = "TOB"
)

// RuleKey 是规则的运行键：BusinessType + SrcIP（说明书 9）。
type RuleKey struct {
	BusinessType string
	SrcIP        string
}

func (k RuleKey) String() string { return k.BusinessType + "|" + k.SrcIP }

// ForwardRule 是 CP 内部的期望规则（Desired Rule），是静态配置 +
// UPF 动态会话的派生结果。ToB 规则的 VNI/VTEP/MAC 字段为空。
type ForwardRule struct {
	Key              RuleKey
	SessionKey       string
	IMSI             string
	DNN              string
	VNI              uint32 // ToH 有效；ToB 为 0
	LocalVtepIP      string
	RemoteVtepIP     string
	LocalOverlayMAC  string
	RemoteOverlayMAC string
	SrcNatAddr       string
	DesiredVersion   uint64 // 内容变化时递增；重复 create 不递增
}

// EqualContent 比较两条规则除版本外的业务内容是否相同。
func (r *ForwardRule) EqualContent(o *ForwardRule) bool {
	if r == nil || o == nil {
		return r == o
	}
	return r.Key == o.Key &&
		r.VNI == o.VNI &&
		r.LocalVtepIP == o.LocalVtepIP &&
		r.RemoteVtepIP == o.RemoteVtepIP &&
		r.LocalOverlayMAC == o.LocalOverlayMAC &&
		r.RemoteOverlayMAC == o.RemoteOverlayMAC &&
		r.SrcNatAddr == o.SrcNatAddr
}

// ToToHRule / ToToBRule 把内部期望规则转成 CP-UP 协议体。
func (r *ForwardRule) ToToHRule() ToHRule {
	return ToHRule{
		SrcIP: r.Key.SrcIP, VNI: r.VNI,
		LocalVtepIP: r.LocalVtepIP, RemoteVtepIP: r.RemoteVtepIP,
		LocalOverlayMAC: r.LocalOverlayMAC, RemoteOverlayMAC: r.RemoteOverlayMAC,
		SrcNatAddr: r.SrcNatAddr,
	}
}

func (r *ForwardRule) ToToBRule() ToBRule {
	return ToBRule{SrcIP: r.Key.SrcIP, SrcNatAddr: r.SrcNatAddr}
}

// ---------------------------------------------------------------------------
// RULE_SYNC SNAPSHOT（说明书 6.7）
// ---------------------------------------------------------------------------

// SnapshotRule 是快照中的一条规则，复用 ToHRule / ToBRule，
// 外层 businessType 决定 rule 的解释方式。
type SnapshotRule struct {
	BusinessType string          `json:"businessType"`
	Rule         json.RawMessage `json:"rule"`
}

// SnapshotPayload 是 RULE_SYNC/SNAPSHOT 的 payload。
type SnapshotPayload struct {
	SnapshotID string         `json:"snapshotId"`
	BatchNo    uint32         `json:"batchNo"`
	LastBatch  bool           `json:"lastBatch"`
	Rules      []SnapshotRule `json:"rules"`
}

// SnapshotRuleResult 是批次 ACK 中逐规则的结果。
type SnapshotRuleResult struct {
	BusinessType      string `json:"businessType"`
	SrcIP             string `json:"srcIp"`
	Code              int    `json:"code"`
	Message           string `json:"message,omitempty"`
	AllocatedSrcNatIP string `json:"allocatedSrcNatIp,omitempty"`
}

// SnapshotAckData 是批次 ACK 的 data（说明书 6.7.2）。
type SnapshotAckData struct {
	SnapshotID   string               `json:"snapshotId"`
	BatchNo      uint32               `json:"batchNo"`
	SuccessCount uint32               `json:"successCount"`
	FailedCount  uint32               `json:"failedCount"`
	Results      []SnapshotRuleResult `json:"results"`
}

// ---------------------------------------------------------------------------
// UPF 会话（说明书 5.6 / 5.7，UPF V1.1 字段原样移植）
// ---------------------------------------------------------------------------

// SessionAction：UPF 推送与全量查询中的动作。
const (
	SessionCreate = "create"
	SessionDelete = "delete"
)

// UeSession 是 UPF 的一条 UE 会话。SrcIP 由 CP 归一化得到：
// 优先 privateIpv4，其次 privateIpv6（Q3：当前实现 IPv4 为主）。
type UeSession struct {
	Action     string `json:"action"`
	IMSI       string `json:"imsi"`
	DNN        string `json:"dnn"`
	MSISDN     string `json:"msisdn"`
	UeIPv4     string `json:"ueIpv4"`
	UeIPv6     string `json:"ueIpv6"`
	PrivateIPv4 string `json:"privateIpv4"`
	PrivateIPv6 string `json:"privateIpv6"`

	// 以下为 CP 内部派生字段，不参与 UPF JSON 编解码。
	SrcIP  string `json:"-"`
	Source string `json:"-"` // CALLBACK / FULL_SYNC
}

// SessionKey 生成稳定的会话键：imsi+dnn+private_ip（说明书 10.9）。
func (s *UeSession) SessionKey() string {
	return s.IMSI + "|" + s.DNN + "|" + s.SrcIP
}

// NormalizeSrcIP 按说明书规则确定规则 SRC IP。
func (s *UeSession) NormalizeSrcIP() string {
	if s.PrivateIPv4 != "" {
		return s.PrivateIPv4
	}
	return s.PrivateIPv6
}

// ---------------------------------------------------------------------------
// 静态配置实体（说明书 10.5~10.8，SQLite 表映射）
// ---------------------------------------------------------------------------

type VxlanConfig struct {
	ID               string
	VNIBegin, VNIEnd uint32
	LocalVtepIP      string
	RemoteVtepIP     string
	LocalOverlayMAC  string
	RemoteOverlayMAC string
	SrcNatAddr       string
	CreatedAt        int64
	UpdatedAt        int64
}

type VxlanImsiBinding struct {
	ID        string
	IMSI      string
	DNN       string
	VNI       uint32
	CreatedAt int64
	UpdatedAt int64
}

type FttrbConfig struct {
	ID         string
	FttrbID    string
	SrcNatAddr string
	CreatedAt  int64
	UpdatedAt  int64
}

type FttrbImsiBinding struct {
	ID        string
	IMSI      string
	DNN       string
	FttrbID   string
	CreatedAt int64
	UpdatedAt int64
}

// BindingKey 是 CP 内部统一业务索引的键：IMSI+DNN（说明书 9.2）。
type BindingKey struct {
	IMSI string
	DNN  string
}

// BusinessBinding 是统一绑定：BusinessType 决定解释 VNI 还是 FttrbID。
type BusinessBinding struct {
	BusinessType string
	VNI          uint32 // ToH 使用
	FttrbID      string // ToB 使用
}

// ---------------------------------------------------------------------------
// 运行态（说明书 9 / 10.11：第一阶段只放内存，不建 SQLite 表）
// ---------------------------------------------------------------------------

// InstalledState 记录 UP ACK 返回的安装结果。
type InstalledState struct {
	Key               RuleKey
	AllocatedSrcNatIP string
	DesiredVersion    uint64
	Status            string // INSTALLED / PENDING / FAILED / DELETE_PENDING
	LastCode          int
	LastMessage       string
	RetryCount        int
	LastRequestID     string
	InstalledAt       int64 // Unix ms，最近成功安装时间
	UpdatedAt         int64 // Unix ms
}

const (
	InstalledStatusInstalled      = "INSTALLED"
	InstalledStatusPending        = "PENDING"
	InstalledStatusFailed         = "FAILED"
	InstalledStatusDeletePending  = "DELETE_PENDING"
	DesiredStatusPending          = "PENDING"
	DesiredStatusApplied          = "APPLIED"
	DesiredStatusFailed           = "FAILED"
)

// ---------------------------------------------------------------------------
// 配置变更事件（说明书 10.24 / 5.10.2）
// ---------------------------------------------------------------------------

// ConfigChangedEvent 由配置模块在 SQLite 事务提交成功并完成
// Snapshot 原子刷新后发布。业务模块异步消费，绝不阻塞 OAM 接口。
type ConfigChangedEvent struct {
	Op      string       // ADD / MODIFY / DELETE / BATCH
	Keys    []BindingKey // 受影响的 IMSI+DNN
	DNNs    []string     // 受影响的 DNN（去重后）
	// NewDNNs 是本次变更后首次出现本机绑定的 DNN（MODIFY 时用于区分
	// “新增绑定”与“删除/变更绑定”）。只有这些 DNN 需要定向全量补偿。
	NewDNNs []string
	Version uint64
}

// DNNConfigChangedOp：触发定向全量补偿的操作。
// 只有 ADD/BATCH_ADD，或使某 DNN 从"无本机绑定"变为"存在本机绑定"的
// MODIFY，才触发首次定向全量补偿；DELETE 不触发 UPF 全量查询。
func (e ConfigChangedEvent) TriggersFullCompensation() bool {
	return e.Op == "ADD" || e.Op == "BATCH" || e.Op == "BATCH_ADD" || e.Op == "MODIFY"
}

// ---------------------------------------------------------------------------
// requestId 生成
// ---------------------------------------------------------------------------

var requestCounter uint64

// NewRequestID 生成 CP-UP 请求唯一 ID，用于 ACK 匹配和幂等。
func NewRequestID() string {
	n := atomic.AddUint64(&requestCounter, 1)
	return fmt.Sprintf("cp-%d-%d", nowMilli(), n)
}
