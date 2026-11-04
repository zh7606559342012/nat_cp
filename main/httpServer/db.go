// Package httpServer 负责数据库连接初始化（对应 jxtx httpServer 的
// DbConnInit，CP 只用 SQLite3，不用 Redis）。
//
// 约定：DbConnInit 成功后全局 DB 可用，modules 用它构造 models.Repository。
package httpServer

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// DB 是 CP 全局 SQLite 连接，由 DbConnInit 建立。
var DB *sql.DB

// DbConnInit 打开数据库并执行 PRAGMA 与建表（说明书 10.2）。
func DbConnInit(path string) error {
	dsn := fmt.Sprintf("file:%s?cache=shared&mode=rwc&_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=5000", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return err
	}
	DB = db
	return nil
}

// Close 关闭数据库。
func Close() error {
	if DB != nil {
		return DB.Close()
	}
	return nil
}

// migrate 建 5 张表与索引（说明书 10.4~10.8、10.21）。
// 第一阶段只持久化静态表；会话/期望规则/安装状态只放内存。
func migrate(db *sql.DB) error {
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
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
}
