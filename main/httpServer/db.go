// Package httpServer 负责数据库连接初始化（对应 jxtx httpServer 的
// DbConnInit，CP 只用 SQLite3，不用 Redis）。
//
// 约定：DbConnInit 成功后全局 DB 可用，modules 用它构造 models.Repository。
package httpServer

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sirupsen/logrus"
)

// DB 是 CP 全局 SQLite 连接，由 DbConnInit 建立。
var DB *sql.DB

// DbConnInit 打开数据库并执行 PRAGMA 与建表（说明书 10.2）。
func DbConnInit(path string) error {
	// Windows 下路径是反斜杠，统一成正斜杠再拼 DSN：
	// file: 形式按 URL 解析，反斜杠会被当成转义或截断。
	dsn := fmt.Sprintf(
		"file:%s?cache=shared&mode=rwc&_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=5000",
		strings.ReplaceAll(path, "\\", "/"),
	)
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
	// acb91f99 起主键 id 改为 uint32：旧库里的字符串 id（如 vx1/b1）读表
	// 时会报 Scan 错误、导致启动建快照失败，这里先把它们重编号。
	if err := migrateNumericIDs(db); err != nil {
		return err
	}
	return nil
}

// migrateNumericIDs 把四张静态表里旧的字符串主键重编号为数字主键。
//
// 数字串（如 '999101'）本身能被 ParseUint，不动；只有解析失败或为 0 的
// 行才重新编号（取该表已有最大数字 id +1 起），其余字段不动。绑定表的
// 外键只认 fttrb_id / VNI，不认数字 id，所以改 id 不影响关联。
// OAM 侧若还存着旧字符串 id，迁移后需按 list_* 返回的新数字 id 对齐。
func migrateNumericIDs(db *sql.DB) error {
	for _, table := range []string{
		"vxlan_config", "vxlan_imsi_binding", "fttrb_config", "fttrb_imsi_binding",
	} {
		if err := migrateTableIDs(db, table); err != nil {
			return fmt.Errorf("migrate %s ids: %w", table, err)
		}
	}
	return nil
}

func migrateTableIDs(db *sql.DB, table string) error {
	rows, err := db.Query(fmt.Sprintf("SELECT id FROM %s", table))
	if err != nil {
		return err
	}
	var bad []string
	var max uint64
	for rows.Next() {
		var idStr string
		if err := rows.Scan(&idStr); err != nil {
			rows.Close()
			return err
		}
		n, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil || n == 0 {
			bad = append(bad, idStr)
			continue
		}
		if uint64(n) > max {
			max = uint64(n)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(bad) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	next := max + 1
	for _, old := range bad {
		if _, err := tx.Exec(fmt.Sprintf("UPDATE %s SET id = ? WHERE id = ?", table), next, old); err != nil {
			return err
		}
		logrus.WithField("table", table).WithField("old_id", old).
			WithField("new_id", next).Warn("migrated non-numeric config id to uint32")
		next++
	}
	return tx.Commit()
}
