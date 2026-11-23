// Package models 是数据模型与数据库操作服务层（对应 jxtx models）。
//
// 本文件是 Repository：静态配置表的 CRUD/事务/校验。
// 实体定义见 entity.go；连接建立与建表见 httpServer；
// 内存快照构建见 conf。
package models

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"natcp/main/utils"
)

// Repository 是静态配置表的数据库操作服务层。
// 调用方传入 httpServer.DbConnInit 建立的 *sql.DB。
type Repository struct {
	db      *sql.DB
	version uint64 // 每次成功写入后递增，作为 Snapshot 版本
}

// New 创建 Repository。
func New(db *sql.DB) *Repository { return &Repository{db: db} }

// NextVersion 在一次成功写入后递增并返回新版本。
func (s *Repository) NextVersion() uint64 { return atomic.AddUint64(&s.version, 1) }

// ---------------------------------------------------------------------------

// Page 是 list_* 命令的分页参数：Offset 为起始下标（0 起），Limit 为条数。
// Limit <= 0 表示不限条数（只按 Offset 偏移）。
type Page struct {
	Offset int
	Limit  int
}

// MaxPageSize 是单次查询允许的最大条数，防止一次拉全表把响应撑爆。
const MaxPageSize = 1000

// Validate 校验分页参数取值。
func (p Page) Validate() error {
	if p.Offset < 0 {
		return fmt.Errorf("offset must be >= 0, got %d", p.Offset)
	}
	if p.Limit < 0 {
		return fmt.Errorf("limit must be >= 0, got %d", p.Limit)
	}
	if p.Limit > MaxPageSize {
		return fmt.Errorf("limit must be <= %d, got %d", MaxPageSize, p.Limit)
	}
	return nil
}

// 四张表的查询语句。统一 ORDER BY created_at, id，保证分页顺序稳定
// （created_at 写入后不再变，id 唯一，翻页不会错位或重复）。
const (
	sqlSelectVxlanConfigs = `SELECT id,vni_begin,vni_end,local_vtep_ip,remote_vtep_ip,
		local_overlay_mac,remote_overlay_mac,src_nat_addr,created_at,updated_at
		FROM vxlan_config ORDER BY created_at, id`
	sqlCountVxlanConfigs = `SELECT COUNT(*) FROM vxlan_config`

	sqlSelectVxlanBindings = `SELECT id,imsi,dnn,vni,created_at,updated_at
		FROM vxlan_imsi_binding ORDER BY created_at, id`
	sqlCountVxlanBindings = `SELECT COUNT(*) FROM vxlan_imsi_binding`

	sqlSelectFttrbConfigs = `SELECT id,fttrb_id,src_nat_addr,created_at,updated_at
		FROM fttrb_config ORDER BY created_at, id`
	sqlCountFttrbConfigs = `SELECT COUNT(*) FROM fttrb_config`

	sqlSelectFttrbBindings = `SELECT id,imsi,dnn,fttrb_id,created_at,updated_at
		FROM fttrb_imsi_binding ORDER BY created_at, id`
	sqlCountFttrbBindings = `SELECT COUNT(*) FROM fttrb_imsi_binding`
)

// listPaged 执行分页查询，返回当前页 + 总行数。
// 未分页（Offset 与 Limit 都为 0）时不额外跑 COUNT，直接用行数作为总数。
func listPaged[T any](db *sql.DB, selectSQL, countSQL string, p Page,
	scan func(*sql.Rows) (T, error)) ([]T, int, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = -1 // SQLite: LIMIT -1 表示不限制条数
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := db.Query(selectSQL+" LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if limit < 0 && offset == 0 {
		return out, len(out), nil
	}
	var total int
	if err := db.QueryRow(countSQL).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func scanVxlanConfig(rows *sql.Rows) (VxlanConfig, error) {
	var c VxlanConfig
	err := rows.Scan(&c.ID, &c.VNIBegin, &c.VNIEnd, &c.LocalVtepIP, &c.RemoteVtepIP,
		&c.LocalOverlayMAC, &c.RemoteOverlayMAC, &c.SrcNatAddr, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func scanVxlanBinding(rows *sql.Rows) (VxlanImsiBinding, error) {
	var b VxlanImsiBinding
	err := rows.Scan(&b.ID, &b.IMSI, &b.DNN, &b.VNI, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

func scanFttrbConfig(rows *sql.Rows) (FttrbConfig, error) {
	var c FttrbConfig
	err := rows.Scan(&c.ID, &c.FttrbID, &c.SrcNatAddr, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func scanFttrbBinding(rows *sql.Rows) (FttrbImsiBinding, error) {
	var b FttrbImsiBinding
	err := rows.Scan(&b.ID, &b.IMSI, &b.DNN, &b.FttrbID, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

// ListVxlanConfigs 返回全部 VXLAN 配置（快照构建、事件影响面计算使用）。
func (s *Repository) ListVxlanConfigs() ([]VxlanConfig, error) {
	out, _, err := listPaged(s.db, sqlSelectVxlanConfigs, sqlCountVxlanConfigs, Page{}, scanVxlanConfig)
	return out, err
}

// ListVxlanConfigsPage 按分页参数返回 VXLAN 配置，并给出总行数。
func (s *Repository) ListVxlanConfigsPage(p Page) ([]VxlanConfig, int, error) {
	return listPaged(s.db, sqlSelectVxlanConfigs, sqlCountVxlanConfigs, p, scanVxlanConfig)
}

func (s *Repository) ListVxlanBindings() ([]VxlanImsiBinding, error) {
	out, _, err := listPaged(s.db, sqlSelectVxlanBindings, sqlCountVxlanBindings, Page{}, scanVxlanBinding)
	return out, err
}

// ListVxlanBindingsPage 按分页参数返回 ToH 绑定，并给出总行数。
func (s *Repository) ListVxlanBindingsPage(p Page) ([]VxlanImsiBinding, int, error) {
	return listPaged(s.db, sqlSelectVxlanBindings, sqlCountVxlanBindings, p, scanVxlanBinding)
}

func (s *Repository) ListFttrbConfigs() ([]FttrbConfig, error) {
	out, _, err := listPaged(s.db, sqlSelectFttrbConfigs, sqlCountFttrbConfigs, Page{}, scanFttrbConfig)
	return out, err
}

// ListFttrbConfigsPage 按分页参数返回 FTTR-B 配置，并给出总行数。
func (s *Repository) ListFttrbConfigsPage(p Page) ([]FttrbConfig, int, error) {
	return listPaged(s.db, sqlSelectFttrbConfigs, sqlCountFttrbConfigs, p, scanFttrbConfig)
}

func (s *Repository) ListFttrbBindings() ([]FttrbImsiBinding, error) {
	out, _, err := listPaged(s.db, sqlSelectFttrbBindings, sqlCountFttrbBindings, Page{}, scanFttrbBinding)
	return out, err
}

// ListFttrbBindingsPage 按分页参数返回 ToB 绑定，并给出总行数。
func (s *Repository) ListFttrbBindingsPage(p Page) ([]FttrbImsiBinding, int, error) {
	return listPaged(s.db, sqlSelectFttrbBindings, sqlCountFttrbBindings, p, scanFttrbBinding)
}

// 校验
// ---------------------------------------------------------------------------

func validateIP(ip string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid ip %q", ip)
	}
	return nil
}

func validateMAC(mac string) error {
	if _, err := net.ParseMAC(mac); err != nil {
		return fmt.Errorf("invalid mac %q: %w", mac, err)
	}
	return nil
}

func validateCIDR(cidr string) error {
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		return fmt.Errorf("invalid cidr %q: %w", cidr, err)
	}
	return nil
}

func validateVxlanConfig(c VxlanConfig) error {
	if c.ID == 0 {
		return fmt.Errorf("vxlan id is required")
	}
	// add/mod 都是 OAM 全量下发，空字段视为缺参直接拒绝，避免 mod 把库中值清空。
	required := []struct{ name, val string }{
		{"localVtepIp", c.LocalVtepIP},
		{"remoteVtepIp", c.RemoteVtepIP},
		{"localOverlayMac", c.LocalOverlayMAC},
		{"remoteOverlayMac", c.RemoteOverlayMAC},
		{"srcNatAddr", c.SrcNatAddr},
	}
	for _, f := range required {
		if f.val == "" {
			return fmt.Errorf("%s is required", f.name)
		}
	}
	if c.VNIBegin == 0 || c.VNIEnd == 0 {
		return fmt.Errorf("vniBegin and vniEnd are required for add_vxlan")
	}
	if c.VNIBegin > 16777215 || c.VNIEnd < c.VNIBegin || c.VNIEnd > 16777215 {
		return fmt.Errorf("invalid vni range %d-%d", c.VNIBegin, c.VNIEnd)
	}
	for _, ip := range []string{c.LocalVtepIP, c.RemoteVtepIP} {
		if err := validateIP(ip); err != nil {
			return err
		}
	}
	for _, mac := range []string{c.LocalOverlayMAC, c.RemoteOverlayMAC} {
		if err := validateMAC(mac); err != nil {
			return err
		}
	}
	return validateCIDR(c.SrcNatAddr)
}

// checkVxlanOverlap 检查 VNI 区间是否与已有配置重叠（说明书 10.5）。
func checkVxlanOverlap(tx *sql.Tx, id uint32, begin, end uint32) error {
	var cnt int
	err := tx.QueryRow(`SELECT COUNT(*) FROM vxlan_config
		WHERE id != ? AND NOT (vni_end < ? OR vni_begin > ?)`, id, begin, end).Scan(&cnt)
	if err != nil {
		return err
	}
	if cnt > 0 {
		return fmt.Errorf("vni range %d-%d overlaps existing vxlan config", begin, end)
	}
	return nil
}

// checkVniInVxlanRange 确认绑定的 vni 落在某个 vxlan 配置的区间内。
//
// 绑定的 vni 最终要归属到一个 VXLAN 配置（buildRule 按 vni 找配置取 vtep 等
// 参数），区间外的 vni 写进去只会生成不完整规则，所以 add/mod/batch 都先校验。
func checkVniInVxlanRange(tx *sql.Tx, vni uint32) error {
	var cnt int
	err := tx.QueryRow(`SELECT COUNT(*) FROM vxlan_config
		WHERE vni_begin <= ? AND vni_end >= ?`, vni, vni).Scan(&cnt)
	if err != nil {
		return err
	}
	if cnt == 0 {
		return fmt.Errorf("vni %d is not within any vxlan config range, add_vxlan covering it first", vni)
	}
	return nil
}

// checkCrossBindingConflict 检查同一 IMSI+DNN 是否已属于另一业务（说明书 10.8）。
//
// 绑定表内部由 UNIQUE(imsi,dnn) 约束兜底，但两张绑定表（vxlan_imsi_binding /
// fttrb_imsi_binding）之间没有数据库层约束，一条 IMSI 同时挂在 ToH 和 ToB 上
// 时 ApplySession 只会按 LookupBinding 先查到的那张表出规则，另一条静默失效，
// 所以这里显式跨表查重。add 的幂等重发（同表同键）走 ON CONFLICT 更新，
// 不会进入本检查的冲突分支。
func checkCrossBindingConflict(tx *sql.Tx, otherTable, imsi, dnn string) error {
	var cnt int
	err := tx.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE imsi = ? AND dnn = ?`, otherTable), imsi, dnn).Scan(&cnt)
	if err != nil {
		return err
	}
	if cnt > 0 {
		return fmt.Errorf("imsi %s dnn %q already belongs to %s binding, it cannot be bound to the other business type as well; delete it from %s first",
			imsi, dnn, otherTable, otherTable)
	}
	return nil
}

// checkSrcNatAddrConflict 检查 srcNatAddr（转发 NAT 地址池）是否与其它配置重叠。
//
// NAT 地址池是设备级资源，VXLAN 与 FTTR-B 的池子在同一个 UP 上转发，两条配置
// 用同一网段（或一个套在另一个里面，如 10.20.0.1/24 与 10.20.0.0/16）会导致
// NAT 会话互相抢地址，所以这里跨两张表按“有交集”查重。比较走 ParseCIDR 规范化，
// 10.10.0.1/24 与 10.10.0.0/24 视为同一个池，不靠字符串比对。
//
// selfTable/selfID/selfFttrbID 用于 mod：改自己时不把自己当成冲突源（批量按
// fttrbId 幂等更新时 id 可能与库里不同，故再按 fttrbID 排除一次）。库里已有的
// 脏地址（解析不了的）跳过，不因为存量数据挡住新配置。
func checkSrcNatAddrConflict(tx *sql.Tx, selfTable string, selfID uint32, selfFttrbID, addr string) error {
	_, want, err := net.ParseCIDR(addr)
	if err != nil {
		return fmt.Errorf("invalid cidr %q: %w", addr, err)
	}
	// check 返回非 nil 表示命中冲突；自己那一行返回 nil 表示跳过。
	check := func(label string, id uint32, fttrbID, cur string) error {
		if label == selfTable && id == selfID {
			return nil
		}
		if label == "fttrb" && selfFttrbID != "" && fttrbID == selfFttrbID {
			return nil
		}
		_, got, err := net.ParseCIDR(cur)
		if err != nil {
			return nil // 存量脏数据不参与比较
		}
		if !cidrOverlap(want, got) {
			return nil
		}
		return fmt.Errorf("srcNatAddr %s overlaps %s config %d (%s), "+
			"each nat address pool must be disjoint", want.String(), label, id, got.String())
	}

	rows, err := tx.Query(`SELECT id, src_nat_addr FROM vxlan_config ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uint32
		var cur string
		if err := rows.Scan(&id, &cur); err != nil {
			rows.Close()
			return err
		}
		if err := check("vxlan", id, "", cur); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rows, err = tx.Query(`SELECT id, fttrb_id, src_nat_addr FROM fttrb_config ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uint32
		var fttrbID, cur string
		if err := rows.Scan(&id, &fttrbID, &cur); err != nil {
			return err
		}
		if err := check("fttrb", id, fttrbID, cur); err != nil {
			return err
		}
	}
	return rows.Err()
}

// cidrOverlap 判断两个网段是否有交集。
//
// ParseCIDR 给出的 IP 已按掩码对齐，CIDR 块之间的关系只有两种：完全不相交，
// 或者一个包含另一个，所以只需检查任一方的网络地址是否落在对方网段内。
// 地址族不同（v4 vs v6）时 Contains 恒为 false，正好符合预期。
func cidrOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// requireFttrbExists 确认 fttrbId 已存在。
//
// 绑定表对 fttrb_id 有外键，但不做前置检查的话，OAM 传错 fttrbId 只能收到
// SQLite 原文 “FOREIGN KEY constraint failed”，看不出是哪条配置错了。
func requireFttrbExists(tx *sql.Tx, fttrbID string) error {
	var cnt int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM fttrb_config WHERE fttrb_id = ?`, fttrbID).Scan(&cnt); err != nil {
		return err
	}
	if cnt == 0 {
		return fmt.Errorf("fttrb %s not found, add_fttrb it first", fttrbID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 写入：每个方法 = 一个 SQLite 写事务（BEGIN IMMEDIATE → 校验 → 写 → COMMIT）。
// 事务提交成功后，调用方负责刷新内存 Snapshot 并发布配置变更事件。
// ---------------------------------------------------------------------------

// AddVxlan 新增 VXLAN 范围配置（OAM 全量下发）。
//
// 只做新增：id 已存在直接报错，不覆盖已有配置（修改必须走 ModifyVxlan）；
// VNI 范围需与库中其他配置不重叠。
func (s *Repository) AddVxlan(c VxlanConfig) error {
	if err := validateVxlanConfig(c); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cnt int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM vxlan_config WHERE id = ?`, c.ID).Scan(&cnt); err != nil {
		return err
	}
	if cnt > 0 {
		return fmt.Errorf("vxlan %d already exists, use mod_vxlan to modify it", c.ID)
	}
	if err := checkVxlanOverlap(tx, c.ID, c.VNIBegin, c.VNIEnd); err != nil {
		return err
	}
	if err := checkSrcNatAddrConflict(tx, "vxlan", 0, "", c.SrcNatAddr); err != nil {
		return err
	}

	now := utils.NowMilli()
	_, err = tx.Exec(`INSERT INTO vxlan_config(id,vni_begin,vni_end,local_vtep_ip,remote_vtep_ip,
		local_overlay_mac,remote_overlay_mac,src_nat_addr,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.VNIBegin, c.VNIEnd, c.LocalVtepIP, c.RemoteVtepIP,
		c.LocalOverlayMAC, c.RemoteOverlayMAC, c.SrcNatAddr, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ModifyVxlan 修改 VXLAN 范围配置（其余字段由 OAM 全量下发）。
//
// 只做修改：id 不存在直接报错，不隐式新建；VNI 范围不可变，下发范围与库中
// 不一致时报错——换范围需先 del_vxlan 再 add_vxlan，并重新下发该范围内的绑定。
//
// OAM 的 mod 报文不携带 vniBegin/vniEnd，此时沿用库中已有范围（范围本就不可变，
// 缺省即等价于“不变”）。只传其中一个视为报文不完整，直接报错，不去猜另一半。
//
// 返回生效后的配置（含补齐的 vni 范围），调用方据此定位受影响的绑定与会话。
func (s *Repository) ModifyVxlan(c VxlanConfig) (VxlanConfig, error) {
	if c.ID == 0 {
		return VxlanConfig{}, fmt.Errorf("vxlan id is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return VxlanConfig{}, err
	}
	defer tx.Rollback()

	var curBegin, curEnd uint32
	err = tx.QueryRow(`SELECT vni_begin, vni_end FROM vxlan_config WHERE id = ?`, c.ID).
		Scan(&curBegin, &curEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return VxlanConfig{}, fmt.Errorf("vxlan %d not found, use add_vxlan to create it", c.ID)
	}
	if err != nil {
		return VxlanConfig{}, err
	}

	// vni 范围不可变且 OAM 不再下发：两个都没带就沿用库中值；
	// 只带一个说明报文不完整，不能猜另一半，直接报错让人发现。
	switch {
	case c.VNIBegin == 0 && c.VNIEnd == 0:
		c.VNIBegin, c.VNIEnd = curBegin, curEnd
	case c.VNIBegin == 0 || c.VNIEnd == 0:
		return VxlanConfig{}, fmt.Errorf("vniBegin and vniEnd must be provided together, "+
			"or omit both to keep the existing range %d-%d", curBegin, curEnd)
	}

	if err := validateVxlanConfig(c); err != nil {
		return VxlanConfig{}, err
	}
	if curBegin != c.VNIBegin || curEnd != c.VNIEnd {
		return VxlanConfig{}, fmt.Errorf("vni range of vxlan %d is immutable: existing %d-%d, got %d-%d; "+
			"delete it and add a new config instead", c.ID, curBegin, curEnd, c.VNIBegin, c.VNIEnd)
	}
	// 改 srcNatAddr 也要查重：否则两条配置可以指向同一个 NAT 地址池。
	if err := checkSrcNatAddrConflict(tx, "vxlan", c.ID, "", c.SrcNatAddr); err != nil {
		return VxlanConfig{}, err
	}

	now := utils.NowMilli()
	// 范围未变，无需再查重叠；WHERE 只按 id 定位，不会越权改动别的配置。
	_, err = tx.Exec(`UPDATE vxlan_config SET local_vtep_ip=?, remote_vtep_ip=?,
		local_overlay_mac=?, remote_overlay_mac=?, src_nat_addr=?, updated_at=? WHERE id=?`,
		c.LocalVtepIP, c.RemoteVtepIP, c.LocalOverlayMAC, c.RemoteOverlayMAC,
		c.SrcNatAddr, now, c.ID)
	if err != nil {
		return VxlanConfig{}, err
	}
	if err := tx.Commit(); err != nil {
		return VxlanConfig{}, err
	}
	return c, nil
}

// DeleteVxlan 按 OAM 配置 ID 删除 VXLAN 范围配置。
//
// 删除前先查该范围（vni_begin~vni_end）内是否仍有 ToH 绑定：有则拒绝删除并
// 在错误信息里带上数量与一条示例，提示先删绑定；没有才真正删除。
// 返回被删配置，供调用方定位受影响的会话（对齐 FTTR-B 的外键 RESTRICT 语义，
// 但给出明确原因，不把 SQLite 原始错误抛给 OAM）。
func (s *Repository) DeleteVxlan(id uint32) (VxlanConfig, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return VxlanConfig{}, err
	}
	defer tx.Rollback()

	var c VxlanConfig
	err = tx.QueryRow(`SELECT id,vni_begin,vni_end,local_vtep_ip,remote_vtep_ip,
		local_overlay_mac,remote_overlay_mac,src_nat_addr,created_at,updated_at
		FROM vxlan_config WHERE id = ?`, id).
		Scan(&c.ID, &c.VNIBegin, &c.VNIEnd, &c.LocalVtepIP, &c.RemoteVtepIP,
			&c.LocalOverlayMAC, &c.RemoteOverlayMAC, &c.SrcNatAddr, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return VxlanConfig{}, fmt.Errorf("vxlan %d not found", id)
	}
	if err != nil {
		return VxlanConfig{}, err
	}

	// 再删前检查：范围内还有绑定就不让删，避免留下引用不存在范围的悬空绑定。
	var cnt int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM vxlan_imsi_binding
		WHERE vni BETWEEN ? AND ?`, c.VNIBegin, c.VNIEnd).Scan(&cnt); err != nil {
		return VxlanConfig{}, err
	}
	if cnt > 0 {
		var imsi, dnn string
		if err := tx.QueryRow(`SELECT imsi, dnn FROM vxlan_imsi_binding
			WHERE vni BETWEEN ? AND ? ORDER BY imsi LIMIT 1`, c.VNIBegin, c.VNIEnd).
			Scan(&imsi, &dnn); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return VxlanConfig{}, err
		}
		return VxlanConfig{}, fmt.Errorf("vxlan %d cannot be deleted: %d imsi binding(s) still in vni range %d-%d "+
			"(e.g. imsi %s dnn %q), delete the bindings first", id, cnt, c.VNIBegin, c.VNIEnd, imsi, dnn)
	}

	if _, err := tx.Exec(`DELETE FROM vxlan_config WHERE id = ?`, id); err != nil {
		return VxlanConfig{}, err
	}
	return c, tx.Commit()
}

// UpsertVxlanBinding 新增 ToH IMSI+DNN→VNI 绑定（add_vxlan_imsi）。
//
// 业务键是 (imsi,dnn)：重复下发同一对 (imsi,dnn) 视为幂等重发，只刷新 vni，
// 保留首次写入的 id（改 id 会与主键冲突，OAM 侧也已按首次的 id 对齐）。
//
// 修改已有绑定请走 ModifyVxlanBinding——它按 id 定位，允许改 imsi/dnn；
// 这里如果 OAM 拿着旧 id 传了新的 (imsi,dnn)，ON CONFLICT(imsi,dnn) 命不中
// 旧行，插入会直接撞主键（UNIQUE constraint failed: ...id），故先显式拒绝。
func (s *Repository) UpsertVxlanBinding(b VxlanImsiBinding) error {
	// id 必填：删除、事件定位都按 id 走，缺 id 无法回指这条绑定。
	if b.ID == 0 {
		return fmt.Errorf("binding id is required")
	}
	if b.IMSI == "" {
		return fmt.Errorf("imsi is required")
	}
	if b.VNI < 1 || b.VNI > 16777215 {
		return fmt.Errorf("invalid vni %d", b.VNI)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkBindingIDFree(tx, "vxlan_imsi_binding", b.ID, b.IMSI, b.DNN, "mod_vxlan_imsi"); err != nil {
		return err
	}
	// vni 必须落在某个 vxlan 配置的区间内，否则生成不了完整规则。
	if err := checkVniInVxlanRange(tx, b.VNI); err != nil {
		return err
	}
	// 同一 IMSI+DNN 不允许同时挂在 ToH / ToB 两张表上（说明书 10.8）。
	if err := checkCrossBindingConflict(tx, "fttrb_imsi_binding", b.IMSI, b.DNN); err != nil {
		return err
	}
	now := utils.NowMilli()
	_, err = tx.Exec(`INSERT INTO vxlan_imsi_binding(id,imsi,dnn,vni,created_at,updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(imsi,dnn) DO UPDATE SET vni=excluded.vni, updated_at=excluded.updated_at`,
		b.ID, b.IMSI, b.DNN, b.VNI, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ModifyVxlanBinding 按绑定 id 修改 ToH 绑定（mod_vxlan_imsi），返回修改前的记录。
//
// 与 add 分开的原因见 UpsertVxlanBinding：OAM 在编辑页会把 imsi/dnn 一起改，
// 而 add 的 ON CONFLICT(imsi,dnn) 只在(imsi,dnn) 命中时生效，(imsi,dnn) 一变
// 就退化成普通 INSERT，同一个 id 再插一次必然撞主键。
// 返回旧记录是为了让调用方把旧 (imsi,dnn) 的规则也重算掉，否则改绑后旧规则会
// 一直留到该 UE 下线。
func (s *Repository) ModifyVxlanBinding(b VxlanImsiBinding) (VxlanImsiBinding, error) {
	if b.ID == 0 {
		return VxlanImsiBinding{}, fmt.Errorf("binding id is required")
	}
	if b.IMSI == "" {
		return VxlanImsiBinding{}, fmt.Errorf("imsi is required")
	}
	if b.VNI < 1 || b.VNI > 16777215 {
		return VxlanImsiBinding{}, fmt.Errorf("invalid vni %d", b.VNI)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return VxlanImsiBinding{}, err
	}
	defer tx.Rollback()

	var old VxlanImsiBinding
	err = tx.QueryRow(`SELECT id,imsi,dnn,vni,created_at,updated_at FROM vxlan_imsi_binding WHERE id = ?`, b.ID).
		Scan(&old.ID, &old.IMSI, &old.DNN, &old.VNI, &old.CreatedAt, &old.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return VxlanImsiBinding{}, fmt.Errorf("vxlan imsi binding %d not found, use add_vxlan_imsi to create it", b.ID)
	}
	if err != nil {
		return VxlanImsiBinding{}, err
	}
	// (imsi,dnn) 被别人占着就不能改过去，否则 UNIQUE(imsi,dnn) 抛 SQLite 原文。
	var otherID uint32
	err = tx.QueryRow(`SELECT id FROM vxlan_imsi_binding WHERE imsi = ? AND dnn = ? AND id != ?`,
		b.IMSI, b.DNN, b.ID).Scan(&otherID)
	if err == nil {
		return VxlanImsiBinding{}, fmt.Errorf("imsi %s dnn %q is already bound to vxlan binding %d",
			b.IMSI, b.DNN, otherID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return VxlanImsiBinding{}, err
	}
	// 改出的新 (imsi,dnn) 也不允许落在另一业务（ToB）的绑定表里。
	if err := checkCrossBindingConflict(tx, "fttrb_imsi_binding", b.IMSI, b.DNN); err != nil {
		return VxlanImsiBinding{}, err
	}
	// 改出的新 vni 同样必须落在某个 vxlan 配置的区间内。
	if err := checkVniInVxlanRange(tx, b.VNI); err != nil {
		return VxlanImsiBinding{}, err
	}

	now := utils.NowMilli()
	if _, err := tx.Exec(`UPDATE vxlan_imsi_binding SET imsi=?, dnn=?, vni=?, updated_at=? WHERE id=?`,
		b.IMSI, b.DNN, b.VNI, now, b.ID); err != nil {
		return VxlanImsiBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return VxlanImsiBinding{}, err
	}
	return old, nil
}

// checkBindingIDFree 检查 id 是否已被另一条 (imsi,dnn) 的绑定占用。
//
// 占用了就说明调用方本该走 mod_*（按 id 改），走 add_* 只会拿到 SQLite 的
// “UNIQUE constraint failed: ...id”，看不出是谁占了。
func checkBindingIDFree(tx *sql.Tx, table string, id uint32, imsi, dnn, modCommand string) error {
	var curIMSI, curDNN string
	err := tx.QueryRow(fmt.Sprintf(`SELECT imsi, dnn FROM %s WHERE id = ?`, table), id).
		Scan(&curIMSI, &curDNN)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if curIMSI == imsi && curDNN == dnn {
		return nil
	}
	return fmt.Errorf("binding %d already belongs to imsi %s dnn %q, use %s to change it",
		id, curIMSI, curDNN, modCommand)
}

// BatchAddVxlanBindings 批量新增 ToH 绑定（同一事务；同业务重复键忽略，
// 跨业务冲突则整个批次失败回滚）。
func (s *Repository) BatchAddVxlanBindings(list []VxlanImsiBinding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := utils.NowMilli()
	for _, b := range list {
		if b.ID == 0 || b.IMSI == "" || b.VNI < 1 || b.VNI > 16777215 {
			return fmt.Errorf("invalid vxlan binding %+v: id, imsi and vni are required", b)
		}
		if err := checkVniInVxlanRange(tx, b.VNI); err != nil {
			return err
		}
		if err := checkCrossBindingConflict(tx, "fttrb_imsi_binding", b.IMSI, b.DNN); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO vxlan_imsi_binding(id,imsi,dnn,vni,created_at,updated_at)
			VALUES(?,?,?,?,?,?)`, b.ID, b.IMSI, b.DNN, b.VNI, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteVxlanBinding 按 IMSI+DNN 删除 ToH 绑定。
func (s *Repository) DeleteVxlanBinding(imsi, dnn string) error {
	_, err := s.db.Exec(`DELETE FROM vxlan_imsi_binding WHERE imsi = ? AND dnn = ?`, imsi, dnn)
	return err
}

// DeleteVxlanBindingByID 按绑定自身的 id 删除 ToH 绑定，返回被删记录。
// 之所以返回整条记录：OAM 的 del 命令只传 id，而事件需要 imsi+dnn
// 才能定位受影响会话，删除前先把它读出来。
func (s *Repository) DeleteVxlanBindingByID(id uint32) (VxlanImsiBinding, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return VxlanImsiBinding{}, err
	}
	defer tx.Rollback()

	var b VxlanImsiBinding
	err = tx.QueryRow(`SELECT id,imsi,dnn,vni,created_at,updated_at
		FROM vxlan_imsi_binding WHERE id = ?`, id).
		Scan(&b.ID, &b.IMSI, &b.DNN, &b.VNI, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return VxlanImsiBinding{}, fmt.Errorf("vxlan imsi binding %d not found", id)
	}
	if err != nil {
		return VxlanImsiBinding{}, err
	}
	if _, err := tx.Exec(`DELETE FROM vxlan_imsi_binding WHERE id = ?`, id); err != nil {
		return VxlanImsiBinding{}, err
	}
	return b, tx.Commit()
}

// UpsertFttrb 新增或修改 FTTR-B 配置（add_fttrb / mod_fttrb 共用）。
//
// 按 id 定位写入；fttrb_id 是业务唯一键（绑定表外键也指向它），被别的 id 占用
// 时先拦下来给出可读原因，否则 update 会撞 UNIQUE(fttrb_id) 抛 SQLite 原文。
// srcNatAddr 是 NAT 地址池，跨 VXLAN / FTTR-B 全局查重。
func (s *Repository) UpsertFttrb(c FttrbConfig) error {
	if c.ID == 0 || c.FttrbID == "" {
		return fmt.Errorf("fttrb id and fttrbId are required")
	}
	if err := validateCIDR(c.SrcNatAddr); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var otherID uint32
	err = tx.QueryRow(`SELECT id FROM fttrb_config WHERE fttrb_id = ? AND id != ?`, c.FttrbID, c.ID).Scan(&otherID)
	if err == nil {
		return fmt.Errorf("fttrbId %s is already used by fttrb config %d", c.FttrbID, otherID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := checkSrcNatAddrConflict(tx, "fttrb", c.ID, c.FttrbID, c.SrcNatAddr); err != nil {
		return err
	}

	now := utils.NowMilli()
	_, err = tx.Exec(`INSERT INTO fttrb_config(id,fttrb_id,src_nat_addr,created_at,updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET fttrb_id=excluded.fttrb_id,
		src_nat_addr=excluded.src_nat_addr, updated_at=excluded.updated_at`,
		c.ID, c.FttrbID, c.SrcNatAddr, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// BatchAddFttrbConfigs 批量新增 FTTR-B 配置（单事务，全成功或全回滚）。
//
// 冲突目标必须是 fttrb_id 而不是 id：fttrb_id 才是业务唯一键（有 UNIQUE 约束，
// 也是绑定表外键的指向），同一 fttrbId 换个 id 再下发按 id 做 upsert 会撞
// fttrb_id 的 UNIQUE 直接报错。命中 fttrb_id 时只刷新 src_nat_addr/updated_at，
// 保留首次写入的 id 与 created_at，所以重复下发同一批是幂等的。
//
// id 必填，与单条 add_fttrb 对齐——删除、修改都按 id 定位，批量里隐式生成
// 一个 id 会让 OAM 侧对不上账。
func (s *Repository) BatchAddFttrbConfigs(list []FttrbConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := utils.NowMilli()
	seen := make(map[string]bool, len(list))
	for _, c := range list {
		if c.ID == 0 || c.FttrbID == "" {
			return fmt.Errorf("invalid fttrb config %+v: id and fttrbId are required", c)
		}
		if err := validateCIDR(c.SrcNatAddr); err != nil {
			return fmt.Errorf("fttrb %d: %w", c.ID, err)
		}
		// 批内同一个 fttrbId 出现两次一定是上游生成有误，让整批失败比静默取
		// 最后一条更容易发现问题。
		if seen[c.FttrbID] {
			return fmt.Errorf("duplicate fttrbId %q in batch", c.FttrbID)
		}
		seen[c.FttrbID] = true

		// 批内已写入的行在同一事务里可见，所以这条检查同时覆盖批内重复与库中重复。
		if err := checkSrcNatAddrConflict(tx, "fttrb", c.ID, c.FttrbID, c.SrcNatAddr); err != nil {
			return fmt.Errorf("fttrb %d: %w", c.ID, err)
		}

		if _, err := tx.Exec(`INSERT INTO fttrb_config(id,fttrb_id,src_nat_addr,created_at,updated_at)
			VALUES(?,?,?,?,?)
			ON CONFLICT(fttrb_id) DO UPDATE SET src_nat_addr=excluded.src_nat_addr,
			updated_at=excluded.updated_at`,
			c.ID, c.FttrbID, c.SrcNatAddr, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteFttrb 按 OAM 配置 ID 删除 FTTR-B 配置，返回被删配置。
//
// fttrb_imsi_binding 对 fttrb_id 有 ON DELETE RESTRICT 外键：仍有绑定引用时
// 直接删只会拿到 SQLite 原文 “FOREIGN KEY constraint failed”，看不出是哪些
// 绑定挡着，所以这里先查出引用者，给出可读提示（对齐 DeleteVxlan 的做法）。
// 配置不存在也显式报错，不再假成功。
func (s *Repository) DeleteFttrb(id uint32) (FttrbConfig, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return FttrbConfig{}, err
	}
	defer tx.Rollback()

	var c FttrbConfig
	err = tx.QueryRow(`SELECT id, fttrb_id, src_nat_addr, created_at, updated_at
		FROM fttrb_config WHERE id = ?`, id).
		Scan(&c.ID, &c.FttrbID, &c.SrcNatAddr, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FttrbConfig{}, fmt.Errorf("fttrb config %d not found", id)
	}
	if err != nil {
		return FttrbConfig{}, err
	}

	// 删除前检查：还有绑定引用这个 fttrbId 就不让删，避免留下悬空绑定。
	var cnt int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM fttrb_imsi_binding
		WHERE fttrb_id = ?`, c.FttrbID).Scan(&cnt); err != nil {
		return FttrbConfig{}, err
	}
	if cnt > 0 {
		// 取一条样例放进错误信息，OAM 侧能直接定位要删的绑定。
		var imsi, dnn string
		if err := tx.QueryRow(`SELECT imsi, dnn FROM fttrb_imsi_binding
			WHERE fttrb_id = ? ORDER BY id LIMIT 1`, c.FttrbID).Scan(&imsi, &dnn); err != nil {
			return FttrbConfig{}, err
		}
		return FttrbConfig{}, fmt.Errorf(
			"fttrb %s is still referenced by %d imsi binding(s), e.g. imsi %s dnn %q; "+
				"delete or rebind the bindings first", c.FttrbID, cnt, imsi, dnn)
	}

	if _, err := tx.Exec(`DELETE FROM fttrb_config WHERE id = ?`, id); err != nil {
		return FttrbConfig{}, err
	}
	if err := tx.Commit(); err != nil {
		return FttrbConfig{}, err
	}
	return c, nil
}

// UpsertFttrbBinding 新增 ToB IMSI+DNN→FTTRB ID 绑定（add_fttrb_imsi）。
//
// 业务键是 (imsi,dnn)：重复下发同一对 (imsi,dnn) 视为幂等重发，只刷新 fttrb_id，
// 保留首次写入的 id。要改已有的 imsi/dnn 请走 ModifyFttrbBinding——那条按 id
// 定位；拿旧 id 传新 (imsi,dnn) 在这里只会撞主键。
func (s *Repository) UpsertFttrbBinding(b FttrbImsiBinding) error {
	if b.ID == 0 {
		return fmt.Errorf("binding id is required")
	}
	if b.IMSI == "" || b.FttrbID == "" {
		return fmt.Errorf("imsi and fttrbId are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkBindingIDFree(tx, "fttrb_imsi_binding", b.ID, b.IMSI, b.DNN, "mod_fttrb_imsi"); err != nil {
		return err
	}
	// 同一 IMSI+DNN 不允许同时挂在 ToB / ToH 两张表上（说明书 10.8）。
	if err := checkCrossBindingConflict(tx, "vxlan_imsi_binding", b.IMSI, b.DNN); err != nil {
		return err
	}
	if err := requireFttrbExists(tx, b.FttrbID); err != nil {
		return err
	}
	now := utils.NowMilli()
	_, err = tx.Exec(`INSERT INTO fttrb_imsi_binding(id,imsi,dnn,fttrb_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(imsi,dnn) DO UPDATE SET fttrb_id=excluded.fttrb_id, updated_at=excluded.updated_at`,
		b.ID, b.IMSI, b.DNN, b.FttrbID, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ModifyFttrbBinding 按绑定 id 修改 ToB 绑定（mod_fttrb_imsi），返回修改前的记录。
//
// OAM 的编辑页会把 imsi / dnn / fttrbId 一起提交，imsi 或 dnn 一变，
// add 走的 ON CONFLICT(imsi,dnn) 就命不中旧行，退化成普通 INSERT，同一个 id
// 再插一次必然撞主键（UNIQUE constraint failed: fttrb_imsi_binding.id）。
// 所以修改必须按 id 定位、就地 UPDATE。
// 返回旧记录供调用方把旧 (imsi,dnn) 的规则一并重算，避免改绑后旧规则残留。
func (s *Repository) ModifyFttrbBinding(b FttrbImsiBinding) (FttrbImsiBinding, error) {
	if b.ID == 0 {
		return FttrbImsiBinding{}, fmt.Errorf("binding id is required")
	}
	if b.IMSI == "" || b.FttrbID == "" {
		return FttrbImsiBinding{}, fmt.Errorf("imsi and fttrbId are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return FttrbImsiBinding{}, err
	}
	defer tx.Rollback()

	var old FttrbImsiBinding
	err = tx.QueryRow(`SELECT id,imsi,dnn,fttrb_id,created_at,updated_at FROM fttrb_imsi_binding WHERE id = ?`, b.ID).
		Scan(&old.ID, &old.IMSI, &old.DNN, &old.FttrbID, &old.CreatedAt, &old.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FttrbImsiBinding{}, fmt.Errorf("fttrb imsi binding %d not found, use add_fttrb_imsi to create it", b.ID)
	}
	if err != nil {
		return FttrbImsiBinding{}, err
	}
	var otherID uint32
	err = tx.QueryRow(`SELECT id FROM fttrb_imsi_binding WHERE imsi = ? AND dnn = ? AND id != ?`,
		b.IMSI, b.DNN, b.ID).Scan(&otherID)
	if err == nil {
		return FttrbImsiBinding{}, fmt.Errorf("imsi %s dnn %q is already bound to fttrb binding %d",
			b.IMSI, b.DNN, otherID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return FttrbImsiBinding{}, err
	}
	// 改出的新 (imsi,dnn) 也不允许落在另一业务（ToH）的绑定表里。
	if err := checkCrossBindingConflict(tx, "vxlan_imsi_binding", b.IMSI, b.DNN); err != nil {
		return FttrbImsiBinding{}, err
	}
	if err := requireFttrbExists(tx, b.FttrbID); err != nil {
		return FttrbImsiBinding{}, err
	}

	now := utils.NowMilli()
	if _, err := tx.Exec(`UPDATE fttrb_imsi_binding SET imsi=?, dnn=?, fttrb_id=?, updated_at=? WHERE id=?`,
		b.IMSI, b.DNN, b.FttrbID, now, b.ID); err != nil {
		return FttrbImsiBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return FttrbImsiBinding{}, err
	}
	return old, nil
}

// BatchAddFttrbBindings 批量新增 ToB 绑定（同一事务；跨业务冲突则整个批次失败回滚）。
func (s *Repository) BatchAddFttrbBindings(list []FttrbImsiBinding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := utils.NowMilli()
	for _, b := range list {
		if b.ID == 0 || b.IMSI == "" || b.FttrbID == "" {
			return fmt.Errorf("invalid fttrb binding %+v: id, imsi and fttrbId are required", b)
		}
		if err := checkCrossBindingConflict(tx, "vxlan_imsi_binding", b.IMSI, b.DNN); err != nil {
			return err
		}
		if err := requireFttrbExists(tx, b.FttrbID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO fttrb_imsi_binding(id,imsi,dnn,fttrb_id,created_at,updated_at)
			VALUES(?,?,?,?,?,?)`, b.ID, b.IMSI, b.DNN, b.FttrbID, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteFttrbBinding 按 IMSI+DNN 删除 ToB 绑定。
func (s *Repository) DeleteFttrbBinding(imsi, dnn string) error {
	_, err := s.db.Exec(`DELETE FROM fttrb_imsi_binding WHERE imsi = ? AND dnn = ?`, imsi, dnn)
	return err
}

// DeleteFttrbBindingByID 按绑定自身的 id 删除 ToB 绑定，返回被删记录。
// 与 DeleteVxlanBindingByID 同理：事件需要 imsi+dnn，删前先读。
func (s *Repository) DeleteFttrbBindingByID(id uint32) (FttrbImsiBinding, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return FttrbImsiBinding{}, err
	}
	defer tx.Rollback()

	var b FttrbImsiBinding
	err = tx.QueryRow(`SELECT id,imsi,dnn,fttrb_id,created_at,updated_at
		FROM fttrb_imsi_binding WHERE id = ?`, id).
		Scan(&b.ID, &b.IMSI, &b.DNN, &b.FttrbID, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FttrbImsiBinding{}, fmt.Errorf("fttrb imsi binding %d not found", id)
	}
	if err != nil {
		return FttrbImsiBinding{}, err
	}
	if _, err := tx.Exec(`DELETE FROM fttrb_imsi_binding WHERE id = ?`, id); err != nil {
		return FttrbImsiBinding{}, err
	}
	return b, tx.Commit()
}
