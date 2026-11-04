// Package models 是数据模型与数据库操作服务层（对应 jxtx models）。
//
// 本包只放 SQLite 实体与 Repository 的 CRUD/事务/校验；
// 数据库连接的建立与建表在 httpServer，内存快照的构建在 conf。
package models

// VxlanConfig 是 vxlan_config 表的实体（说明书 10.5）。
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

// VxlanImsiBinding 是 vxlan_imsi_binding 表的实体（说明书 10.6）。
type VxlanImsiBinding struct {
	ID        string
	IMSI      string
	DNN       string
	VNI       uint32
	CreatedAt int64
	UpdatedAt int64
}

// FttrbConfig 是 fttrb_config 表的实体（说明书 10.7）。
type FttrbConfig struct {
	ID         string
	FttrbID    string
	SrcNatAddr string
	CreatedAt  int64
	UpdatedAt  int64
}

// FttrbImsiBinding 是 fttrb_imsi_binding 表的实体（说明书 10.8）。
type FttrbImsiBinding struct {
	ID        string
	IMSI      string
	DNN       string
	FttrbID   string
	CreatedAt int64
	UpdatedAt int64
}
