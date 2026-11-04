// Package store 负责 CP 的 SQLite 持久化。
//
// 第一阶段只持久化 5 张静态表（说明书 10.22）：schema_meta、vxlan_config、
// vxlan_imsi_binding、fttrb_config、fttrb_imsi_binding。
// UPF 会话、Desired Rule、Installed State 只放内存，不建表，
// 重启后由 UPF 全量会话 + UP 批量同步重建。
//
// 约定（说明书 10.13）：Repository 层只负责 CRUD/事务；业务校验、
// 关联计算与状态机在 Service/Engine 层；handler 不直接拼 SQL。
package store

import (
	"database/sql"
	"fmt"
	"net"
	"sync/atomic"

	_ "github.com/mattn/go-sqlite3"

	"natcp/internal/model"
	"natcp/internal/snapshot"
)

// Store 持有 SQLite 连接与快照版本号。
type Store struct {
	db      *sql.DB
	version uint64 // 每次成功写入后递增，作为 Snapshot 版本
}

// Open 打开数据库并执行 PRAGMA 与建表（说明书 10.2）。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?cache=shared&mode=rwc&_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=5000", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// NextVersion 在一次成功写入后递增并返回新版本。
func (s *Store) NextVersion() uint64 { return atomic.AddUint64(&s.version, 1) }

// migrate 建 5 张表与索引（说明书 10.4~10.8、10.21）。
func (s *Store) migrate() error {
	ddl := `
	CREATE TABLE IF NOT EXISTS schema_meta (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	);
	INSERT OR IGNORE INTO schema_meta(key,value,updated_at) VALUES('schema_version','1',0);
	CREATE TABLE IF NOT EXISTS vxlan_config (
		id TEXT PRIMARY KEY,
		vni_begin INTEGER NOT NULL CHECK(vni_begin BETWEEN 1 AND 16777215),
		vni_end INTEGER NOT NULL CHECK(vni_end >= vni_begin AND vni_end <= 16777215),
		local_vtep_ip TEXT NOT NULL, remote_vtep_ip TEXT NOT NULL,
		local_overlay_mac TEXT NOT NULL, remote_overlay_mac TEXT NOT NULL,
		src_nat_addr TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_vxlan_range ON vxlan_config(vni_begin, vni_end);
	CREATE TABLE IF NOT EXISTS vxlan_imsi_binding (
		id TEXT PRIMARY KEY,
		imsi TEXT NOT NULL,
		dnn TEXT NOT NULL DEFAULT '',
		vni INTEGER NOT NULL CHECK(vni BETWEEN 1 AND 16777215),
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		UNIQUE(imsi, dnn)
	);
	CREATE INDEX IF NOT EXISTS idx_vxlan_binding_vni ON vxlan_imsi_binding(vni);
	CREATE TABLE IF NOT EXISTS fttrb_config (
		id TEXT PRIMARY KEY,
		fttrb_id TEXT NOT NULL UNIQUE,
		src_nat_addr TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS fttrb_imsi_binding (
		id TEXT PRIMARY KEY, imsi TEXT NOT NULL, dnn TEXT NOT NULL DEFAULT '',
		fttrb_id TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		UNIQUE(imsi, dnn),
		FOREIGN KEY(fttrb_id) REFERENCES fttrb_config(fttrb_id)
		ON UPDATE CASCADE ON DELETE RESTRICT
	);
	CREATE INDEX IF NOT EXISTS idx_fttrb_binding_fttrb ON fttrb_imsi_binding(fttrb_id);
	`
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 读取：构建 Snapshot 用
// ---------------------------------------------------------------------------

func (s *Store) ListVxlanConfigs() ([]model.VxlanConfig, error) {
	rows, err := s.db.Query(`SELECT id,vni_begin,vni_end,local_vtep_ip,remote_vtep_ip,
		local_overlay_mac,remote_overlay_mac,src_nat_addr,created_at,updated_at FROM vxlan_config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.VxlanConfig
	for rows.Next() {
		var c model.VxlanConfig
		if err := rows.Scan(&c.ID, &c.VNIBegin, &c.VNIEnd, &c.LocalVtepIP, &c.RemoteVtepIP,
			&c.LocalOverlayMAC, &c.RemoteOverlayMAC, &c.SrcNatAddr, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListVxlanBindings() ([]model.VxlanImsiBinding, error) {
	rows, err := s.db.Query(`SELECT id,imsi,dnn,vni,created_at,updated_at FROM vxlan_imsi_binding`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.VxlanImsiBinding
	for rows.Next() {
		var b model.VxlanImsiBinding
		if err := rows.Scan(&b.ID, &b.IMSI, &b.DNN, &b.VNI, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) ListFttrbConfigs() ([]model.FttrbConfig, error) {
	rows, err := s.db.Query(`SELECT id,fttrb_id,src_nat_addr,created_at,updated_at FROM fttrb_config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FttrbConfig
	for rows.Next() {
		var c model.FttrbConfig
		if err := rows.Scan(&c.ID, &c.FttrbID, &c.SrcNatAddr, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListFttrbBindings() ([]model.FttrbImsiBinding, error) {
	rows, err := s.db.Query(`SELECT id,imsi,dnn,fttrb_id,created_at,updated_at FROM fttrb_imsi_binding`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FttrbImsiBinding
	for rows.Next() {
		var b model.FttrbImsiBinding
		if err := rows.Scan(&b.ID, &b.IMSI, &b.DNN, &b.FttrbID, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BuildSnapshot 从四张表读取并构建新 Snapshot。
func (s *Store) BuildSnapshot(version uint64) (*snapshot.Snapshot, error) {
	vxlan, err := s.ListVxlanConfigs()
	if err != nil {
		return nil, err
	}
	vb, err := s.ListVxlanBindings()
	if err != nil {
		return nil, err
	}
	fttrb, err := s.ListFttrbConfigs()
	if err != nil {
		return nil, err
	}
	fb, err := s.ListFttrbBindings()
	if err != nil {
		return nil, err
	}
	return snapshot.Build(version, vxlan, vb, fttrb, fb), nil
}

// ---------------------------------------------------------------------------
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

func validateVxlanConfig(c model.VxlanConfig) error {
	if c.ID == "" {
		return fmt.Errorf("vxlan id is required")
	}
	if c.VNIBegin < 1 || c.VNIBegin > 16777215 || c.VNIEnd < c.VNIBegin || c.VNIEnd > 16777215 {
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
func checkVxlanOverlap(tx *sql.Tx, id string, begin, end uint32) error {
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

// UpsertVxlan 新增或修改 VXLAN 范围配置。
func (s *Store) UpsertVxlan(c model.VxlanConfig) error {
	if err := validateVxlanConfig(c); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkVxlanOverlap(tx, c.ID, c.VNIBegin, c.VNIEnd); err != nil {
		return err
	}
	now := model.NowMilli()
	_, err = tx.Exec(`INSERT INTO vxlan_config(id,vni_begin,vni_end,local_vtep_ip,remote_vtep_ip,
		local_overlay_mac,remote_overlay_mac,src_nat_addr,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET vni_begin=excluded.vni_begin, vni_end=excluded.vni_end,
		local_vtep_ip=excluded.local_vtep_ip, remote_vtep_ip=excluded.remote_vtep_ip,
		local_overlay_mac=excluded.local_overlay_mac, remote_overlay_mac=excluded.remote_overlay_mac,
		src_nat_addr=excluded.src_nat_addr, updated_at=excluded.updated_at`,
		c.ID, c.VNIBegin, c.VNIEnd, c.LocalVtepIP, c.RemoteVtepIP,
		c.LocalOverlayMAC, c.RemoteOverlayMAC, c.SrcNatAddr, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteVxlan 按 OAM 配置 ID 删除 VXLAN 范围配置。
func (s *Store) DeleteVxlan(id string) error {
	_, err := s.db.Exec(`DELETE FROM vxlan_config WHERE id = ?`, id)
	return err
}

// UpsertVxlanBinding 新增或修改 ToH IMSI+DNN→VNI 绑定。
func (s *Store) UpsertVxlanBinding(b model.VxlanImsiBinding) error {
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
	now := model.NowMilli()
	if b.ID == "" {
		b.ID = b.IMSI + "|" + b.DNN
	}
	_, err = tx.Exec(`INSERT INTO vxlan_imsi_binding(id,imsi,dnn,vni,created_at,updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(imsi,dnn) DO UPDATE SET vni=excluded.vni, updated_at=excluded.updated_at`,
		b.ID, b.IMSI, b.DNN, b.VNI, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// BatchAddVxlanBindings 批量新增 ToH 绑定（同一事务；同业务重复键忽略，
// 跨业务冲突则整个批次失败回滚）。
func (s *Store) BatchAddVxlanBindings(list []model.VxlanImsiBinding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := model.NowMilli()
	for _, b := range list {
		if b.IMSI == "" || b.VNI < 1 || b.VNI > 16777215 {
			return fmt.Errorf("invalid vxlan binding %+v", b)
		}
		if err := checkCrossBindingConflict(tx, "fttrb_imsi_binding", b.IMSI, b.DNN); err != nil {
			return err
		}
		id := b.ID
		if id == "" {
			id = b.IMSI + "|" + b.DNN
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO vxlan_imsi_binding(id,imsi,dnn,vni,created_at,updated_at)
			VALUES(?,?,?,?,?,?)`, id, b.IMSI, b.DNN, b.VNI, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteVxlanBinding 按 IMSI+DNN 删除 ToH 绑定。
func (s *Store) DeleteVxlanBinding(imsi, dnn string) error {
	_, err := s.db.Exec(`DELETE FROM vxlan_imsi_binding WHERE imsi = ? AND dnn = ?`, imsi, dnn)
	return err
}

// UpsertFttrb 新增或修改 FTTR-B 配置。
func (s *Store) UpsertFttrb(c model.FttrbConfig) error {
	if c.ID == "" || c.FttrbID == "" {
		return fmt.Errorf("fttrb id and fttrbId are required")
	}
	if err := validateCIDR(c.SrcNatAddr); err != nil {
		return err
	}
	now := model.NowMilli()
	_, err := s.db.Exec(`INSERT INTO fttrb_config(id,fttrb_id,src_nat_addr,created_at,updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET fttrb_id=excluded.fttrb_id,
		src_nat_addr=excluded.src_nat_addr, updated_at=excluded.updated_at`,
		c.ID, c.FttrbID, c.SrcNatAddr, now, now)
	return err
}

// DeleteFttrb 按 OAM 配置 ID 删除 FTTR-B 配置。
// 注意：fttrb_imsi_binding 对 fttrb_id 有 ON DELETE RESTRICT 外键，
// 仍有绑定引用时删除会失败，调用方需先删除绑定。
func (s *Store) DeleteFttrb(id string) error {
	_, err := s.db.Exec(`DELETE FROM fttrb_config WHERE id = ?`, id)
	return err
}

// UpsertFttrbBinding 新增或修改 ToB IMSI+DNN→FTTRB ID 绑定。
func (s *Store) UpsertFttrbBinding(b model.FttrbImsiBinding) error {
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
	now := model.NowMilli()
	if b.ID == "" {
		b.ID = b.IMSI + "|" + b.DNN
	}
	_, err = tx.Exec(`INSERT INTO fttrb_imsi_binding(id,imsi,dnn,fttrb_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(imsi,dnn) DO UPDATE SET fttrb_id=excluded.fttrb_id, updated_at=excluded.updated_at`,
		b.ID, b.IMSI, b.DNN, b.FttrbID, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// BatchAddFttrbBindings 批量新增 ToB 绑定（同一事务）。
func (s *Store) BatchAddFttrbBindings(list []model.FttrbImsiBinding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := model.NowMilli()
	for _, b := range list {
		if b.IMSI == "" || b.FttrbID == "" {
			return fmt.Errorf("invalid fttrb binding %+v", b)
		}
		if err := checkCrossBindingConflict(tx, "vxlan_imsi_binding", b.IMSI, b.DNN); err != nil {
			return err
		}
		id := b.ID
		if id == "" {
			id = b.IMSI + "|" + b.DNN
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO fttrb_imsi_binding(id,imsi,dnn,fttrb_id,created_at,updated_at)
			VALUES(?,?,?,?,?,?)`, id, b.IMSI, b.DNN, b.FttrbID, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteFttrbBinding 按 IMSI+DNN 删除 ToB 绑定。
func (s *Store) DeleteFttrbBinding(imsi, dnn string) error {
	_, err := s.db.Exec(`DELETE FROM fttrb_imsi_binding WHERE imsi = ? AND dnn = ?`, imsi, dnn)
	return err
}
