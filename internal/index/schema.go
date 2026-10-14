// Package index — SQLite 持久索引与队列（design.md §5）。
// 崩溃安全边界：Detector 写完即持久化（P3）。
package index

// State 枚举（design.md §5）。
const (
	StateUnknown      = 0
	StateIdentical    = 1
	StateLocalNew     = 2
	StateRemoteNew    = 3
	StateLocalMod     = 4
	StateConflict     = 5
	StateError        = 6
	StateInProgress   = 7
	StateUnstable     = 8
	StateRemovedLocal = 9
)

// StateName 状态名（status 输出用）。
func StateName(s int) string {
	switch s {
	case StateUnknown:
		return "UNKNOWN"
	case StateIdentical:
		return "IDENTICAL"
	case StateLocalNew:
		return "LOCAL_NEW"
	case StateRemoteNew:
		return "REMOTE_NEW"
	case StateLocalMod:
		return "LOCAL_MOD"
	case StateConflict:
		return "CONFLICT"
	case StateError:
		return "ERROR"
	case StateInProgress:
		return "IN_PROGRESS"
	case StateUnstable:
		return "UNSTABLE"
	case StateRemovedLocal:
		return "REMOVED_LOCAL"
	default:
		return "?"
	}
}

// SchemaVersion — sync_pair.schema_ver。
const SchemaVersion = 1

// ddl 逐条执行（IF NOT EXISTS，可重复初始化）。
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS sync_pair (
		id            INTEGER PRIMARY KEY,
		local_root    TEXT    NOT NULL,
		remote_root   TEXT    NOT NULL,
		account       TEXT    NOT NULL,
		detector_mode TEXT    NOT NULL,
		last_flush    INTEGER,
		last_verify   INTEGER,
		schema_ver    INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS entry (
		pair_id       INTEGER NOT NULL,
		rel_path      TEXT    NOT NULL,
		is_dir        INTEGER NOT NULL,
		scan_run      INTEGER NOT NULL,
		l_size        INTEGER,
		l_mtime_ns    INTEGER,
		l_md5         TEXT,
		l_settled_ns  INTEGER,
		r_fsid        INTEGER,
		r_size        INTEGER,
		r_mtime       INTEGER,
		r_md5         TEXT,
		state         INTEGER NOT NULL,
		upload_id     TEXT,
		done_blocks   TEXT,
		attempts      INTEGER NOT NULL DEFAULT 0,
		last_err      TEXT,
		last_sync     INTEGER,
		PRIMARY KEY (pair_id, rel_path)
	) WITHOUT ROWID`,

	// 部分索引：4 万条 IDENTICAL/REMOVED_LOCAL 不进索引（design §5）
	`CREATE INDEX IF NOT EXISTS idx_pending ON entry(pair_id, state, l_mtime_ns DESC)
		WHERE state NOT IN (1, 9)`,

	// usn 模式的 ref→path 映射（Phase 2 用，先建表）
	`CREATE TABLE IF NOT EXISTS dir_ref (
		pair_id  INTEGER NOT NULL,
		ref      INTEGER NOT NULL,
		rel_path TEXT    NOT NULL,
		PRIMARY KEY (pair_id, ref)
	) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS kv (
		k TEXT PRIMARY KEY,
		v BLOB
	)`,
}
