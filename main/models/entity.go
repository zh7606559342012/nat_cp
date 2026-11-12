// Package models 是数据模型与数据库操作服务层（对应 jxtx models）。
//
// 本包只放 SQLite 实体与 Repository 的 CRUD/事务/校验；
// 数据库连接的建立与建表在 httpServer，内存快照的构建在 conf。
package models

// 实体字段的类型约定：
//
//   - 主键 ID 用 uint32：OAM 侧主键就是数字，收发都是 JSON 数字。
//     DB 的 id 列是 TEXT，但 database/sql 内置支持数字字段与文本列的互转
//     （写入时按整数下发、读取时用 strconv.ParseUint 解析），
//     所以不需要自定义类型，也不需要改表结构。
//   - 绑定的 IMSI / fttrbId 用 string：CP 内部全部当标识串处理（拼接、比较、查表）；
//     OAM 传数字时在 controllers 层转成字符串。
//   - VNI 相关用 uint32：VNI 是 24 位，uint32 足够且与 DB 的 INTEGER 对齐。

// VxlanConfig 是 vxlan_config 表的实体（说明书 10.5）。
type VxlanConfig struct {
	ID               uint32 `json:"id"`
	VNIBegin         uint32 `json:"vniBegin"`
	VNIEnd           uint32 `json:"vniEnd"`
	LocalVtepIP      string `json:"localVtepIp"`
	RemoteVtepIP     string `json:"remoteVtepIp"`
	LocalOverlayMAC  string `json:"localOverlayMac"`
	RemoteOverlayMAC string `json:"remoteOverlayMac"`
	SrcNatAddr       string `json:"srcNatAddr"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
}

// VxlanImsiBinding 是 vxlan_imsi_binding 表的实体（说明书 10.6）。
type VxlanImsiBinding struct {
	ID        uint32 `json:"id"`
	IMSI      string `json:"imsi"`
	DNN       string `json:"dnn"`
	VNI       uint32 `json:"vni"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// FttrbConfig 是 fttrb_config 表的实体（说明书 10.7）。
type FttrbConfig struct {
	ID         uint32 `json:"id"`
	FttrbID    string `json:"fttrbId"`
	SrcNatAddr string `json:"srcNatAddr"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// FttrbImsiBinding 是 fttrb_imsi_binding 表的实体（说明书 10.8）。
type FttrbImsiBinding struct {
	ID        uint32 `json:"id"`
	IMSI      string `json:"imsi"`
	DNN       string `json:"dnn"`
	FttrbID   string `json:"fttrbId"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}
