package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store 是 SQLite 索引的唯一入口。
// Phase 1 并发模型：单连接（只有 Detector 串行访问；WAL + 短事务，见 design §4.8）。
type Store struct {
	db     *sql.DB
	path   string
	pairID int64

	commitCount int
	commitDur   time.Duration
}

// ChangeKind -- CommitBatch 的操作类型。
type ChangeKind uint8

const (
	ChangeInsert ChangeKind = iota
	ChangeUpdate
	ChangeRemove
)

// Change -- 一条待写库的观测结果。
type Change struct {
	Kind    ChangeKind
	RelPath string
	IsDir   bool
	Size    int64
	MtimeNs int64
	State   int    // insert/update 的目标状态；remove 忽略（固定 REMOVED_LOCAL）
	Err     string // 拦截原因（如 netdiskname 非法文件名）；正常变更为 ""
}

// SnapRec -- LoadSnapshot 返回的索引行紧凑副本。
type SnapRec struct {
	IsDir     bool
	Size      int64
	MtimeNs   int64
	State     int
	HasRemote bool
	RSize     int64
}

// Counts -- status 用的汇总。
type Counts struct {
	ByState map[int]int64
	Files   int64 // 本地现存文件数（state != REMOVED_LOCAL）
	Dirs    int64 // 本地现存目录数
	Bytes   int64 // 本地现存文件总字节
}

// ErrNoStats -- kv 里没有上次扫描记录。
var ErrNoStats = errors.New("尚无扫描记录")

// Open 打开（或创建）索引库，并校验/建立 sync_pair 绑定。
func Open(path, localRoot string) (*Store, error) {
	s, err := open(path)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*Store, error) { s.Close(); return nil, e }

	if err := s.ensureSchema(); err != nil {
		return fail(fmt.Errorf("初始化表结构失败: %w", err))
	}
	if err := s.ensurePair(localRoot); err != nil {
		return fail(err)
	}
	return s, nil
}

// OpenExisting 打开已存在的索引库（status 用）：不建表、不建 pair。
func OpenExisting(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("数据库不存在: %s", path)
	}
	s, err := open(path)
	if err != nil {
		return nil, err
	}
	if err := s.loadPairID(); err != nil {
		s.Close()
		return nil, fmt.Errorf("数据库缺少 sync_pair 记录: %s", path)
	}
	return s, nil
}

func open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(abs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库目录失败: %w", err)
		}
	}
	db, err := sql.Open("sqlite", abs)
	if err != nil {
		return nil, err
	}
	// 单连接：保证 PRAGMA 恒定作用于该连接，也避免检测/发送互相抢写锁（Phase 1 只有检测端）。
	db.SetMaxOpenConns(1)
	if err := applyPragmas(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: abs}, nil
}

func applyPragmas(db *sql.DB) error {
	// journal_mode 返回一行结果，必须 Query。
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("启用 WAL 失败: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("无法启用 WAL（当前 journal_mode=%s）", mode)
	}
	for _, pr := range []string{
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA cache_size=-8000`, // 8 MB 页缓存（design §4.7）
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=OFF`,
	} {
		if _, err := db.Exec(pr); err != nil {
			return fmt.Errorf("%s 失败: %w", pr, err)
		}
	}
	return nil
}

func (s *Store) ensureSchema() error {
	for _, stmt := range ddl {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	// 迁移：entry.renamed_from（镜像模式改名管道，M2）
	if err := s.addMissingColumn("entry", "renamed_from", "TEXT"); err != nil {
		return err
	}
	return nil
}

// addMissingColumn -- 幂等迁移（PRAGMA table_info 检查后 ALTER）。
func (s *Store) addMissingColumn(table, col, decl string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			return rows.Err()
		}
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + decl)
	return err
}

// ensurePair 确保 sync_pair 存在，且绑定的 local_root 与本次一致。
func (s *Store) ensurePair(localRoot string) error {
	abs, err := filepath.Abs(localRoot)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)

	var id int64
	var stored string
	err = s.db.QueryRow(`SELECT id, local_root FROM sync_pair ORDER BY id LIMIT 1`).Scan(&id, &stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := s.db.Exec(
			`INSERT INTO sync_pair(local_root, remote_root, account, detector_mode, schema_ver) VALUES(?,?,?,?,?)`,
			abs, "", "", "poll", SchemaVersion)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}
		s.pairID = id
		return nil
	case err != nil:
		return err
	}
	if !samePath(stored, abs) {
		return fmt.Errorf("数据库 %s 已绑定根目录:\n    %s\n  与本次 --root %s 不符。\n  换根目录请换一个 --db 路径（本工具不会删除已有数据库）。",
			s.path, stored, abs)
	}
	s.pairID = id
	return nil
}

func (s *Store) loadPairID() error {
	return s.db.QueryRow(`SELECT id FROM sync_pair ORDER BY id LIMIT 1`).Scan(&s.pairID)
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NextRunID -- 递增并持久化扫描轮次计数（kv.scan_run）。
func (s *Store) NextRunID(ctx context.Context) (int64, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k='scan_run'`).Scan(&raw)
	var n int64
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, err
	default:
		n, _ = strconv.ParseInt(string(raw), 10, 64)
	}
	n++
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO kv(k,v) VALUES('scan_run',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		[]byte(strconv.FormatInt(n, 10))); err != nil {
		return 0, err
	}
	return n, nil
}

// LoadSnapshot -- 一次性读出全部索引行，供内存比对（poll 模式的全量索引，design §4.7）。
func (s *Store) LoadSnapshot(ctx context.Context) (map[string]SnapRec, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rel_path, is_dir, COALESCE(l_size,0), COALESCE(l_mtime_ns,0), state,
		       CASE WHEN r_fsid IS NULL THEN 0 ELSE 1 END, COALESCE(r_size,0)
		FROM entry WHERE pair_id=?`, s.pairID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]SnapRec, 49152)
	for rows.Next() {
		var rel string
		var r SnapRec
		var isDir, hasRemote int
		if err := rows.Scan(&rel, &isDir, &r.Size, &r.MtimeNs, &r.State, &hasRemote, &r.RSize); err != nil {
			return nil, err
		}
		r.IsDir = isDir != 0
		r.HasRemote = hasRemote != 0
		m[rel] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// 写库语句。Insert 带 ON CONFLICT：兼容「路径类型变化（文件<->目录）」的重建。
// Update 顺带清空 upload_id/done_blocks：内容变了，旧的断点续传进度作废（design §9.3 偏向重传）。
const (
	sqlInsert = `INSERT INTO entry(pair_id, rel_path, is_dir, scan_run, l_size, l_mtime_ns, l_settled_ns, state, attempts, last_err)
		VALUES(?,?,?,?,?,?,?,?,0,?)
		ON CONFLICT(pair_id, rel_path) DO UPDATE SET
			is_dir=excluded.is_dir, scan_run=excluded.scan_run, l_size=excluded.l_size,
			l_mtime_ns=excluded.l_mtime_ns, l_settled_ns=excluded.l_settled_ns, state=excluded.state,
			upload_id=NULL, done_blocks=NULL, attempts=0, last_err=excluded.last_err`
	sqlUpdate = `UPDATE entry SET is_dir=?, scan_run=?, l_size=?, l_mtime_ns=?, l_settled_ns=?, state=?,
			upload_id=NULL, done_blocks=NULL, attempts=0, last_err=?
		WHERE pair_id=? AND rel_path=?`
	sqlRemove = `UPDATE entry SET state=?, scan_run=? WHERE pair_id=? AND rel_path=?`
)

// CommitBatch -- 一个事务写入一批变更，返回该批耗时。
func (s *Store) CommitBatch(ctx context.Context, runID int64, changes []Change) (time.Duration, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	t0 := time.Now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // commit 后为 no-op

	ins, err := tx.PrepareContext(ctx, sqlInsert)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	upd, err := tx.PrepareContext(ctx, sqlUpdate)
	if err != nil {
		return 0, err
	}
	defer upd.Close()
	rmv, err := tx.PrepareContext(ctx, sqlRemove)
	if err != nil {
		return 0, err
	}
	defer rmv.Close()

	now := time.Now().UnixNano()
	for _, c := range changes {
		switch c.Kind {
		case ChangeInsert:
			_, err = ins.ExecContext(ctx, s.pairID, c.RelPath, boolInt(c.IsDir), runID,
				c.Size, c.MtimeNs, now, c.State, c.Err)
		case ChangeUpdate:
			_, err = upd.ExecContext(ctx, boolInt(c.IsDir), runID, c.Size, c.MtimeNs, now, c.State,
				c.Err, s.pairID, c.RelPath)
		case ChangeRemove:
			_, err = rmv.ExecContext(ctx, StateRemovedLocal, runID, s.pairID, c.RelPath)
		}
		if err != nil {
			return 0, fmt.Errorf("写入 %q 失败: %w", c.RelPath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	d := time.Since(t0)
	s.commitCount++
	s.commitDur += d
	return d, nil
}

// MarkMissing -- 本轮未见的路径标记为本地已删（远端保留，design §9.2）。
func (s *Store) MarkMissing(ctx context.Context, runID int64, paths []string) (time.Duration, error) {
	changes := make([]Change, len(paths))
	for i, p := range paths {
		changes[i] = Change{Kind: ChangeRemove, RelPath: p}
	}
	return s.CommitBatch(ctx, runID, changes)
}

// Checkpoint -- wal_checkpoint(TRUNCATE)，防 WAL 膨胀（design §4.8）。
func (s *Store) Checkpoint(ctx context.Context) error {
	var b1, b2, b3 int
	return s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&b1, &b2, &b3)
}

// PutKV / GetKV -- 杂项状态（design §5 kv 表）。GetKV 未命中返回 (nil, nil)。
func (s *Store) PutKV(ctx context.Context, k string, v []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) GetKV(ctx context.Context, k string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// SaveScanStats / LoadScanStats -- 上次扫描结果（status 用）。
func (s *Store) SaveScanStats(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.PutKV(ctx, "last_scan", b)
}

func (s *Store) LoadScanStats(ctx context.Context, v any) error {
	b, err := s.GetKV(ctx, "last_scan")
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return ErrNoStats
	}
	return json.Unmarshal(b, v)
}

// Counts -- 状态分布 + 本地现存规模。
func (s *Store) Counts(ctx context.Context) (*Counts, error) {
	c := &Counts{ByState: map[int]int64{}}
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM entry WHERE pair_id=? GROUP BY state`, s.pairID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st int
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, err
		}
		c.ByState[st] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN is_dir=0 AND state!=9 THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN is_dir=1 AND state!=9 THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN is_dir=0 AND state!=9 THEN COALESCE(l_size,0) ELSE 0 END),0)
		FROM entry WHERE pair_id=?`, s.pairID).Scan(&c.Files, &c.Dirs, &c.Bytes)
	return c, err
}

// PairInfo -- status 用。
func (s *Store) PairInfo() (localRoot, remoteRoot, mode string, schemaVer int, pairID int64, err error) {
	err = s.db.QueryRow(`SELECT local_root, remote_root, detector_mode, schema_ver, id FROM sync_pair WHERE id=?`, s.pairID).
		Scan(&localRoot, &remoteRoot, &mode, &schemaVer, &pairID)
	return
}

// SetRemoteRoot -- 首次 send 时绑定远端根目录（之后不可改）。
func (s *Store) SetRemoteRoot(ctx context.Context, remoteRoot string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sync_pair SET remote_root=? WHERE id=?`, remoteRoot, s.pairID)
	return err
}

// PendingEntry -- 已领取/待上传的文件。
type PendingEntry struct {
	RelPath    string
	Size       int64
	MtimeNs    int64
	MD5        string
	SettledNs  int64
	Attempts   int
	UploadID   string // 上次中断的上传会话（断点续传）
	DoneBlocks string // 已完成分片（JSON 数组）
	HasRemote  bool   // 远端同路径有副本（镜像模式：覆盖前先归档）
	RMtime     int64  // 远端副本 mtime（归档目标名用）
}

// pendingSelect -- 待传文件选择（最早 settle 优先；attempts 上限防死循环；
// 非法文件名拦截行永不出队 ---- 见 internal/netdiskname）。
const pendingSelect = `
	SELECT rel_path, COALESCE(l_size,0), COALESCE(l_mtime_ns,0), COALESCE(l_md5,''),
	       COALESCE(l_settled_ns,0), attempts, COALESCE(upload_id,''), COALESCE(done_blocks,''),
	       CASE WHEN r_fsid IS NULL THEN 0 ELSE 1 END, COALESCE(r_mtime,0)
	FROM entry
	WHERE pair_id=? AND is_dir=0 AND state IN (2,4,6) AND attempts < 20
	  AND COALESCE(last_err,'') NOT LIKE '非法文件名:%'
	ORDER BY l_settled_ns ASC, rel_path ASC LIMIT ?`

func scanPending(rows *sql.Rows) ([]PendingEntry, error) {
	var out []PendingEntry
	for rows.Next() {
		var e PendingEntry
		var hasRemote int
		if err := rows.Scan(&e.RelPath, &e.Size, &e.MtimeNs, &e.MD5, &e.SettledNs,
			&e.Attempts, &e.UploadID, &e.DoneBlocks, &hasRemote, &e.RMtime); err != nil {
			return nil, err
		}
		e.HasRemote = hasRemote != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClaimBatch -- 原子领取一批待传文件（state → IN_PROGRESS，design §7）。
// 先复位崩溃遗留的 IN_PROGRESS（design §9.3：宁可多传，不漏传）。
func (s *Store) ClaimBatch(ctx context.Context, limit int) ([]PendingEntry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		UPDATE entry SET state = CASE WHEN r_fsid IS NULL THEN 2 ELSE 4 END
		WHERE pair_id=? AND state=7`, s.pairID); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, pendingSelect, s.pairID, limit)
	if err != nil {
		return nil, err
	}
	out, err := scanPending(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, tx.Commit()
	}

	stmt, err := tx.PrepareContext(ctx, `UPDATE entry SET state=7 WHERE pair_id=? AND rel_path=?`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, e := range out {
		if _, err := stmt.ExecContext(ctx, s.pairID, e.RelPath); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// LookupEntry -- 单条点查（usn 模式：查询次数 ∝ 变更数，与总文件数无关，design §4.7）。
func (s *Store) LookupEntry(ctx context.Context, relPath string) (SnapRec, bool, error) {
	var r SnapRec
	var isDir, hasRemote int
	err := s.db.QueryRowContext(ctx, `
		SELECT is_dir, COALESCE(l_size,0), COALESCE(l_mtime_ns,0), state,
		       CASE WHEN r_fsid IS NULL THEN 0 ELSE 1 END, COALESCE(r_size,0)
		FROM entry WHERE pair_id=? AND rel_path=?`, s.pairID, relPath).
		Scan(&isDir, &r.Size, &r.MtimeNs, &r.State, &hasRemote, &r.RSize)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapRec{}, false, nil
	}
	if err != nil {
		return SnapRec{}, false, err
	}
	r.IsDir = isDir != 0
	r.HasRemote = hasRemote != 0
	return r, true, nil
}

// ── dir_ref（NTFS FileReferenceNumber → 相对路径，usn 模式路径解析，design §4.2c）──

// DirRef -- 一条目录引用映射。
type DirRef struct {
	Ref     uint64
	RelPath string
}

// ResetDirRefs -- 全量重建前清空（poll 全量扫描结束时调用，自愈陈旧引用）。
func (s *Store) ResetDirRefs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dir_ref WHERE pair_id=?`, s.pairID)
	return err
}

// AddDirRefs -- 批量 upsert 目录引用。
func (s *Store) AddDirRefs(ctx context.Context, refs []DirRef) error {
	if len(refs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO dir_ref(pair_id, ref, rel_path) VALUES(?,?,?)
		 ON CONFLICT(pair_id, ref) DO UPDATE SET rel_path=excluded.rel_path`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range refs {
		if _, err := stmt.ExecContext(ctx, s.pairID, int64(r.Ref), r.RelPath); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DirRefCount -- 引用表行数（0 = 从未全量引导过，usn 首轮必须先全量扫描）。
func (s *Store) DirRefCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dir_ref WHERE pair_id=?`, s.pairID).Scan(&n)
	return n, err
}

// ResolveRef -- 引用号 → 相对路径（root 本身为 ""）。
func (s *Store) ResolveRef(ctx context.Context, ref uint64) (string, bool, error) {
	var p string
	err := s.db.QueryRowContext(ctx,
		`SELECT rel_path FROM dir_ref WHERE pair_id=? AND ref=?`, s.pairID, int64(ref)).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return p, true, nil
}

// UpdateDirRefPath -- 目录改名：更新该引用自身的路径。
func (s *Store) UpdateDirRefPath(ctx context.Context, ref uint64, newPath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dir_ref SET rel_path=? WHERE pair_id=? AND ref=?`, newPath, s.pairID, int64(ref))
	return err
}

// RewriteSubtree -- 目录改名：整体重写子树前缀（dir_ref 与 entry 两表）。
// 用 substr 精确匹配而非 LIKE（SQLite LIKE 对 ASCII 大小写不敏感，会误伤）。
//
// 同时把子树里已 IDENTICAL 的**文件**重新入队（state → LOCAL_NEW/LOCAL_MOD）：
// 改名后远端新路径并不存在（r_* 指向旧路径），若不重新入队，新路径永远不会被
// 备份。重新入队后内容已在服务端 → 秒传近乎零成本补上新路径；远端旧路径照旧
// 保留（不删除语义，design §9.2）。这也与 poll 后端的「旧路径消失 + 新路径新增」
// 观测结果保持一致。
func (s *Store) RewriteSubtree(ctx context.Context, oldPrefix, newPrefix string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, table := range []string{"dir_ref", "entry"} {
		_, err := tx.ExecContext(ctx, `
			UPDATE `+table+` SET rel_path = ?3 || substr(rel_path, ?1 + 1)
			WHERE pair_id = ?2
			  AND substr(rel_path, 1, ?1) = ?4
			  AND (length(rel_path) = ?1 OR substr(rel_path, ?1 + 1, 1) = '/')`,
			len(oldPrefix), s.pairID, newPrefix, oldPrefix)
		if err != nil {
			return err
		}
	}
	// 重新入队（见函数注释）：只动 IDENTICAL 的文件行，其余状态不干扰
	if _, err := tx.ExecContext(ctx, `
		UPDATE entry SET state = CASE WHEN r_fsid IS NULL THEN 2 ELSE 4 END
		WHERE pair_id=?1 AND is_dir=0 AND state=1
		  AND (rel_path = ?2 OR (substr(rel_path, 1, ?3) = ?2
		       AND substr(rel_path, ?3 + 1, 1) = '/'))`,
		s.pairID, newPrefix, len(newPrefix)); err != nil {
		return err
	}
	// 登记改名来源（镜像模式 R 阶段远端 mv 用）：
	//  1) 目录行记旧前缀（远端整棵移动的目标基准）
	//  2) 已删待归档行记改名前的旧路径（归档源必须指向旧位置）
	if _, err := tx.ExecContext(ctx, `
		UPDATE entry SET renamed_from=?3
		WHERE pair_id=?1 AND is_dir=1 AND rel_path=?2`,
		s.pairID, newPrefix, oldPrefix); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE entry SET renamed_from = ?3 || substr(rel_path, ?4 + 1)
		WHERE pair_id=?1 AND state=9
		  AND (rel_path = ?2 OR (substr(rel_path, 1, ?4) = ?2
		       AND substr(rel_path, ?4 + 1, 1) = '/'))`,
		s.pairID, newPrefix, oldPrefix, len(newPrefix)); err != nil {
		return err
	}
	return tx.Commit()
}

// MoveEntry -- 文件改名/移动：整行迁移到新路径（目标存在则覆盖）。
// 同时登记 renamed_from（镜像模式 R 阶段据此做远端 mv，见 sender/mirror.go）。
// 迁移后若原为 IDENTICAL 则重新入队 ---- 远端只有旧路径的副本，新路径需要
// 补传（内容已在服务端 → 秒传命中）；远端旧路径照旧保留（不删除语义）。
// mv 成功后由 MarkRenamedDone 撤回到 IDENTICAL。
func (s *Store) MoveEntry(ctx context.Context, oldPath, newPath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entry WHERE pair_id=? AND rel_path=?`, s.pairID, newPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entry(pair_id, rel_path, is_dir, scan_run, l_size, l_mtime_ns, l_md5, l_settled_ns,
			r_fsid, r_size, r_mtime, r_md5, state, upload_id, done_blocks, attempts, last_err, last_sync)
		SELECT pair_id, ?3, is_dir, scan_run, l_size, l_mtime_ns, l_md5, l_settled_ns,
			r_fsid, r_size, r_mtime, r_md5, state, upload_id, done_blocks, attempts, last_err, last_sync
		FROM entry WHERE pair_id=?1 AND rel_path=?2`,
		s.pairID, oldPath, newPath); err != nil {
		return err
	}
	// 重新入队（只动 IDENTICAL；pending/error 状态保持原样）
	if _, err := tx.ExecContext(ctx, `
		UPDATE entry SET state = CASE WHEN r_fsid IS NULL THEN 2 ELSE 4 END
		WHERE pair_id=? AND rel_path=? AND is_dir=0 AND state=1`,
		s.pairID, newPath); err != nil {
		return err
	}
	// 登记改名来源（R 阶段远端 mv 用；与 state 无关，任何行都记）
	if _, err := tx.ExecContext(ctx,
		`UPDATE entry SET renamed_from=?2 WHERE pair_id=?1 AND rel_path=?3`,
		s.pairID, oldPath, newPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entry WHERE pair_id=? AND rel_path=?`, s.pairID, oldPath); err != nil {
		return err
	}
	return tx.Commit()
}

// SetDetectorMode -- 记录实际使用的检测后端（status 展示）。
func (s *Store) SetDetectorMode(mode string) error {
	_, err := s.db.Exec(`UPDATE sync_pair SET detector_mode=? WHERE id=?`, mode, s.pairID)
	return err
}

// RequeueSettled -- UNSTABLE 且已过 settle 期的复位为待传。返回复位数量。
func (s *Store) RequeueSettled(ctx context.Context, settle time.Duration) (int64, error) {
	cutoff := time.Now().Add(-settle).UnixNano()
	res, err := s.db.ExecContext(ctx, `
		UPDATE entry SET state = CASE WHEN r_fsid IS NULL THEN 2 ELSE 4 END
		WHERE pair_id=? AND state=8 AND COALESCE(l_settled_ns,0) <= ?`, s.pairID, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeferEntry -- 稳定性闸门/临时失败：转 UNSTABLE，settle 期满后由 RequeueSettled 复位。
func (s *Store) DeferEntry(ctx context.Context, relPath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE entry SET state=8 WHERE pair_id=? AND rel_path=?`, s.pairID, relPath)
	return err
}

// FailUpload -- 记录失败（state=ERROR，attempts+1）。
func (s *Store) FailUpload(ctx context.Context, relPath, errMsg string) error {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE entry SET state=6, attempts=attempts+1, last_err=? WHERE pair_id=? AND rel_path=?`,
		errMsg, s.pairID, relPath)
	return err
}

// FinishUpload -- 上传成功：state=IDENTICAL + 远端信息回写（design §7 步骤6）。
func (s *Store) FinishUpload(ctx context.Context, relPath string, fsid, rSize, rMtime int64, rMd5 string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE entry SET state=1, r_fsid=?, r_size=?, r_mtime=?, r_md5=?, last_sync=?,
			upload_id=NULL, done_blocks=NULL, attempts=0, last_err=NULL
		WHERE pair_id=? AND rel_path=?`,
		fsid, rSize, rMtime, rMd5, time.Now().Unix(), s.pairID, relPath)
	return err
}

// SetLocalMD5 -- 哈希结果持久化。
func (s *Store) SetLocalMD5(ctx context.Context, relPath, md5sum string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE entry SET l_md5=? WHERE pair_id=? AND rel_path=?`, md5sum, s.pairID, relPath)
	return err
}

// SaveProgress -- 断点续传进度（upload_id + done_blocks，design §7 步骤4）。
func (s *Store) SaveProgress(ctx context.Context, relPath, uploadID, doneBlocks string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE entry SET upload_id=?, done_blocks=? WHERE pair_id=? AND rel_path=?`,
		uploadID, doneBlocks, s.pairID, relPath)
	return err
}

// Pending -- 待传统计（含镜像归档积压）。
type Pending struct {
	Count    int64 // 可传 + 推迟中（state 2,4,6,8）
	Bytes    int64
	OldestNs int64
	Deferred int64 // 其中推迟中（UNSTABLE）
	History  int64 // 镜像归档积压：本地已删、远端旧版待归档（state=9 & r_fsid!=空）
}

// PendingStats -- 待传统计。daemon 的 drain 触发用全量口径：推迟中与归档积压
// 都必须能触发下一轮 drain，否则会卡死。
func (s *Store) PendingStats(ctx context.Context) (*Pending, error) {
	p := &Pending{}
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN state IN (2,4,6,8) THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN state IN (2,4,6,8) THEN COALESCE(l_size,0) ELSE 0 END),0),
		       COALESCE(MIN(CASE WHEN state IN (2,4,6,8) THEN COALESCE(l_settled_ns,0) END),0),
		       COALESCE(SUM(CASE WHEN state = 8 THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN state = 9 AND r_fsid IS NOT NULL THEN 1 ELSE 0 END),0)
		FROM entry WHERE pair_id=? AND is_dir=0`, s.pairID).
		Scan(&p.Count, &p.Bytes, &p.OldestNs, &p.Deferred, &p.History)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// PendingList -- 待传列表预览（dry-run 用）。
func (s *Store) PendingList(ctx context.Context, limit int) ([]PendingEntry, error) {
	rows, err := s.db.QueryContext(ctx, pendingSelect, s.pairID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPending(rows)
}

// HistoryItem -- 镜像归档队列条目（本地已删、远端还有副本）。
type HistoryItem struct {
	RelPath string
	OldPath string // 改名后又删除：远端副本还在改名前的位置（为空 = 同 rel_path）
	RMtime  int64
}

// HistoryQueue -- 镜像模式历史归档队列：本地已删但远端主树仍有副本的条目。
func (s *Store) HistoryQueue(ctx context.Context, limit int) ([]HistoryItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rel_path, COALESCE(renamed_from,''), COALESCE(r_mtime,0) FROM entry
		WHERE pair_id=? AND state=9 AND r_fsid IS NOT NULL
		ORDER BY rel_path ASC LIMIT ?`, s.pairID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryItem
	for rows.Next() {
		var e HistoryItem
		if err := rows.Scan(&e.RelPath, &e.OldPath, &e.RMtime); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClearRemote -- 归档/处理完成后清除主树远端副本记账（r_fsid 置空后
// HistoryQueue 不再领取；若本地文件再现，queueState 会按"远端无副本"入队重传）。
// 同时清 renamed_from：归档已完成，改名标记不得残留（防止复活后被误当成改名 mv）。
func (s *Store) ClearRemote(ctx context.Context, relPath string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE entry SET r_fsid=NULL, r_size=NULL, r_mtime=NULL, r_md5=NULL,
			renamed_from=NULL, last_sync=?
		WHERE pair_id=? AND rel_path=?`,
		time.Now().Unix(), s.pairID, relPath)
	return err
}

// PreArchiveCount -- 镜像模式：待上传条目中"覆盖前需先归档旧版"的数量（dry-run 展示）。
func (s *Store) PreArchiveCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entry WHERE pair_id=? AND is_dir=0 AND state IN (2,4,6) AND r_fsid IS NOT NULL`,
		s.pairID).Scan(&n)
	return n, err
}

// HistoryCount -- 尚未归档的已删条目数（status 展示）。
func (s *Store) HistoryCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entry WHERE pair_id=? AND state=9 AND r_fsid IS NOT NULL`,
		s.pairID).Scan(&n)
	return n, err
}

// ── 改名管道（镜像模式 R 阶段：远端直接 mv，零污染零带宽）──

// PendingRenamed -- 待处理的改名条目。
type PendingRenamed struct {
	RelPath   string // 新路径
	OldPath   string // 改名前路径（renamed_from）
	IsDir     bool
	HasRemote bool // 文件行：远端同路径是否有副本（目录行无意义，恒尝试 mv）
	RMtime    int64
	Size      int64
	MtimeNs   int64
}

// renamedCols -- 改名查询的公共列（FROM entry，无 WHERE）。
// renamed_from 用 COALESCE 包裹：子文件行该列为 NULL（scanRenamed 直接扫 string）。
const renamedCols = `
	SELECT rel_path, COALESCE(renamed_from,''), is_dir,
	       CASE WHEN r_fsid IS NULL THEN 0 ELSE 1 END, COALESCE(r_mtime,0),
	       COALESCE(l_size,0), COALESCE(l_mtime_ns,0)
	FROM entry`

// renamedWhere -- 领取条件：有改名标记；state=9 行归历史阶段处理。
const renamedWhere = `
	WHERE pair_id=? AND renamed_from IS NOT NULL AND renamed_from != '' AND state != 9`

const renamedTail = `
	ORDER BY is_dir DESC, rel_path ASC LIMIT ?`

func scanRenamed(rows *sql.Rows) ([]PendingRenamed, error) {
	var out []PendingRenamed
	for rows.Next() {
		var e PendingRenamed
		var hasRemote int
		if err := rows.Scan(&e.RelPath, &e.OldPath, &e.IsDir, &hasRemote,
			&e.RMtime, &e.Size, &e.MtimeNs); err != nil {
			return nil, err
		}
		e.HasRemote = hasRemote != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClaimRenamed -- 领取一批改名条目（只读领取，不改状态；记账由 MarkRenamedDone）。
// 目录优先（一次 mv 整棵子树）；state=9 行归历史阶段处理。
func (s *Store) ClaimRenamed(ctx context.Context, limit int) ([]PendingRenamed, error) {
	rows, err := s.db.QueryContext(ctx, renamedCols+renamedWhere+renamedTail, s.pairID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRenamed(rows)
}

// RenamedSubtreeRows -- 目录 mv 后需要"撤回重新入队"的子文件行。
// 注意：子文件行本身没有 renamed_from（只有目录行/已删行有），所以这里不能用
// renamedWhere ---- 筛选条件是"位于前缀下 + 状态被改名重排过 + 远端有副本"。
func (s *Store) RenamedSubtreeRows(ctx context.Context, prefix string) ([]PendingRenamed, error) {
	// 参数顺序：?1=pairID ?2=prefix ?3=len(prefix) ?4=prefix ?5=len ?6=limit
	const q = `
	WHERE pair_id=?1 AND is_dir=0 AND state IN (2,4) AND r_fsid IS NOT NULL
	  AND (rel_path = ?2 OR (substr(rel_path, 1, ?3) = ?2
	       AND substr(rel_path, ?3 + 1, 1) = '/'))
	ORDER BY rel_path ASC LIMIT ?4`
	rows, err := s.db.QueryContext(ctx, renamedCols+q, s.pairID, prefix, len(prefix), 1<<20)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRenamed(rows)
}

// MarkRenamedDone -- 改名处理完成：清 renamed_from；
// identical=true 表示远端内容已随 mv 到位且本地未变 → 撤回到 IDENTICAL。
func (s *Store) MarkRenamedDone(ctx context.Context, relPath string, identical bool) error {
	if identical {
		_, err := s.db.ExecContext(ctx, `
			UPDATE entry SET renamed_from=NULL, state=1, last_sync=?
			WHERE pair_id=? AND rel_path=?`,
			time.Now().Unix(), s.pairID, relPath)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE entry SET renamed_from=NULL WHERE pair_id=? AND rel_path=?`, s.pairID, relPath)
	return err
}

// ClearRenamedSubtree -- 目录 mv 成功后清整棵子树的改名标记。
// state=9 行除外：其 renamed_from 是历史归档的**源路径**，归档完成后
// 由 ClearRemote 清除（不能在这里误删）。
func (s *Store) ClearRenamedSubtree(ctx context.Context, prefix string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE entry SET renamed_from=NULL
		WHERE pair_id=?1 AND state != 9
		  AND (rel_path = ?2 OR (substr(rel_path, 1, ?3) = ?2
		       AND substr(rel_path, ?3 + 1, 1) = '/'))`,
		s.pairID, prefix, len(prefix))
	return err
}

// LocalManifest -- 本地清单：所有当前存在的路径（state != REMOVED_LOCAL），
// 值 = is_dir（远端清单对账用：本地有、远端缺的空目录也要补建）。
// 远端清单对账以此为准：RA 中不在清单里的路径 → 归档到历史（design镜像 Q2）。
// 注：被 exclude 的路径会收敛为 REMOVED_LOCAL，故它们的远端副本也会被判为
// "清单外"并归档（与"RA == 本地清单"语义一致）。
func (s *Store) LocalManifest(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rel_path, is_dir FROM entry WHERE pair_id=? AND state != 9`, s.pairID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]bool, 50000)
	for rows.Next() {
		var rel string
		var isDir int
		if err := rows.Scan(&rel, &isDir); err != nil {
			return nil, err
		}
		m[rel] = isDir != 0
	}
	return m, rows.Err()
}

// CheckEntry -- verify 校验目标。
type CheckEntry struct {
	RelPath string
	Size    int64
	LMD5    string // 本地计算的 md5
	RMD5    string // 索引记录的远端 md5（服务端元数据）
}

// CheckList -- IDENTICAL 条目中列出需校验的文件（only 为空则全部）。
func (s *Store) CheckList(ctx context.Context, only []string) ([]CheckEntry, error) {
	if len(only) > 0 {
		var out []CheckEntry
		for _, rel := range only {
			var e CheckEntry
			err := s.db.QueryRowContext(ctx, `
				SELECT rel_path, COALESCE(l_size,0), COALESCE(l_md5,''), COALESCE(r_md5,'')
				FROM entry WHERE pair_id=? AND rel_path=? AND is_dir=0 AND state=1`,
				s.pairID, rel).Scan(&e.RelPath, &e.Size, &e.LMD5, &e.RMD5)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%s 不在 IDENTICAL 状态（先 scan+send）", rel)
			}
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT rel_path, COALESCE(l_size,0), COALESCE(l_md5,''), COALESCE(r_md5,'')
		FROM entry WHERE pair_id=? AND is_dir=0 AND state=1 ORDER BY rel_path`, s.pairID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckEntry
	for rows.Next() {
		var e CheckEntry
		if err := rows.Scan(&e.RelPath, &e.Size, &e.LMD5, &e.RMD5); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CommitStats -- 本进程累计的写库批次数与耗时。
func (s *Store) CommitStats() (count int, total time.Duration) { return s.commitCount, s.commitDur }

func (s *Store) Path() string  { return s.path }
func (s *Store) PairID() int64 { return s.pairID }
func (s *Store) Close() error  { return s.db.Close() }
