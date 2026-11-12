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

// checkCrossBindingConflict 检查同一 IMSI+DNN 是否已属于另一业务（说明书 10.8）。
func checkCrossBindingConflict(tx *sql.Tx, otherTable, imsi, dnn string) error {
	var cnt int
	err := tx.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE imsi = ? AND dnn = ?`, otherTable), imsi, dnn).Scan(&cnt)
	if err != nil {
		return err
	}
	if cnt > 0 {
		return fmt.Errorf("imsi %s dnn %s already bound to the other business type", imsi, dnn)
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

// UpsertVxlanBinding 新增或修改 ToH IMSI+DNN→VNI 绑定。
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
	if err := checkCrossBindingConflict(tx, "fttrb_imsi_binding", b.IMSI, b.DNN); err != nil {
		return err
	}
	now := utils.NowMilli()
	_, err = tx.Exec(`INSERT INTO vxlan_imsi_binding(id,imsi,dnn,vni,created_at,updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(imsi,dnn) DO UPDATE SET id=excluded.id, vni=excluded.vni, updated_at=excluded.updated_at`,
		b.ID, b.IMSI, b.DNN, b.VNI, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
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

// UpsertFttrb 新增或修改 FTTR-B 配置。
func (s *Repository) UpsertFttrb(c FttrbConfig) error {
	if c.ID == 0 || c.FttrbID == "" {
		return fmt.Errorf("fttrb id and fttrbId are required")
	}
	if err := validateCIDR(c.SrcNatAddr); err != nil {
		return err
	}
	now := utils.NowMilli()
	_, err := s.db.Exec(`INSERT INTO fttrb_config(id,fttrb_id,src_nat_addr,created_at,updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET fttrb_id=excluded.fttrb_id,
		src_nat_addr=excluded.src_nat_addr, updated_at=excluded.updated_at`,
		c.ID, c.FttrbID, c.SrcNatAddr, now, now)
	return err
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

// DeleteFttrb 按 OAM 配置 ID 删除 FTTR-B 配置。
// 注意：fttrb_imsi_binding 对 fttrb_id 有 ON DELETE RESTRICT 外键，
// 仍有绑定引用时删除会失败，调用方需先删除绑定。
func (s *Repository) DeleteFttrb(id uint32) error {
	_, err := s.db.Exec(`DELETE FROM fttrb_config WHERE id = ?`, id)
	return err
}

// UpsertFttrbBinding 新增或修改 ToB IMSI+DNN→FTTRB ID 绑定。
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
	if err := checkCrossBindingConflict(tx, "vxlan_imsi_binding", b.IMSI, b.DNN); err != nil {
		return err
	}
	now := utils.NowMilli()
	_, err = tx.Exec(`INSERT INTO fttrb_imsi_binding(id,imsi,dnn,fttrb_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(imsi,dnn) DO UPDATE SET id=excluded.id, fttrb_id=excluded.fttrb_id, updated_at=excluded.updated_at`,
		b.ID, b.IMSI, b.DNN, b.FttrbID, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// BatchAddFttrbBindings 批量新增 ToB 绑定（同一事务）。
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
