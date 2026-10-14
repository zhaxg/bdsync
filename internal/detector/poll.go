package detector

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"bdsync/internal/index"
	"bdsync/internal/netdiskname"
)

// Options — poll 后端参数。
type Options struct {
	BatchSize int      // 每批写库条数（默认 2000，design §6.2：分批落库避免超长事务）
	WalkOnly  bool     // 只遍历计数不写库（性能基线对照）
	Exclude   []string // 排除规则（design §14，见 detector/exclude.go）
}

// ScanResult — 一次全量扫描的结果与计时。
type ScanResult struct {
	Root     string    `json:"root"`
	Backend  string    `json:"backend"` // poll | usn
	RunID    int64     `json:"run_id"`
	WalkOnly bool      `json:"walk_only"`
	At       time.Time `json:"at"`      // 扫描完成时刻
	WalkOK   bool      `json:"walk_ok"` // 遍历完整（只有完整遍历才允许做删除判定）

	Files    int64 `json:"files"`
	Dirs     int64 `json:"dirs"`
	Symlinks int64 `json:"symlinks"`
	Skipped  int64 `json:"skipped"`
	Bytes    int64 `json:"bytes"`

	AddedFiles  int64 `json:"added_files"`
	AddedDirs   int64 `json:"added_dirs"`
	Modified    int64 `json:"modified"`
	Resurrected int64 `json:"resurrected"`
	Removed     int64 `json:"removed"`
	Unchanged   int64 `json:"unchanged"`
	Conflicted  int64 `json:"conflicted"` // 网盘不接受的文件名（design §10 拦截）

	LoadMs       int64 `json:"load_ms"`       // 索引快照加载
	CommitMs     int64 `json:"commit_ms"`     // 写库累计
	CommitCount  int64 `json:"commit_count"`  // 写库批次数
	CheckpointMs int64 `json:"checkpoint_ms"` // WAL checkpoint

	WallMs int64 `json:"wall_ms"` // 总墙钟
	CpuMs  int64 `json:"cpu_ms"`  // 进程 CPU（main 填充）

	RssBytes     int64 `json:"rss_bytes"`           // 扫描结束时进程工作集
	PeakRssBytes int64 `json:"peak_rss_bytes"`      // 进程峰值工作集
	HeapBytes    int64 `json:"heap_bytes"`          // GC 后存活堆（main 填充）
	GoSysBytes   int64 `json:"go_sys_bytes"`        // Go 运行时从 OS 申请的总量
	HeapSysBytes int64 `json:"heap_sys_bytes"`      // Go 堆从 OS 申请的总量
	HeapRelBytes int64 `json:"heap_released_bytes"` // 已归还 OS 的堆

	ErrSamples []string `json:"errors,omitempty"`
}

func (r *ScanResult) noteErr(err error) {
	if err == nil || len(r.ErrSamples) >= 5 {
		return
	}
	s := err.Error()
	for _, e := range r.ErrSamples {
		if e == s {
			return
		}
	}
	r.ErrSamples = append(r.ErrSamples, s)
}

// NoteErr — 供外部（如 main）追加一条错误样本（最多 5 条）。
func (r *ScanResult) NoteErr(err error) { r.noteErr(err) }

// Poll — WalkDir 全量扫描后端（portable：Windows/Linux 共用，design §4.1）。
// 天然完整：全量遍历不存在「丢事件」这回事。也是 usn 后端的正确性对照组（硬规矩③）。
type Poll struct {
	root    string
	index   *index.Store // nil = walk-only
	opts    Options
	matcher *Matcher // 排除规则
}

var _ Backend = (*Poll)(nil)

// NewPoll — index 传 nil 表示 walk-only（不落库）。
func NewPoll(root string, st *index.Store, opts Options) *Poll {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 2000
	}
	return &Poll{
		root:    root,
		index:   st,
		opts:    opts,
		matcher: NewMatcher(opts.Exclude, runtime.GOOS == "windows"),
	}
}

func (p *Poll) Name() string { return "poll" }

func (p *Poll) Start(ctx context.Context, root string) error {
	p.root = root
	return nil
}

// Events — poll 由定时器驱动（design §14 poll_interval），没有事件流。
func (p *Poll) Events() <-chan Event { return nil }

// SelfTest 见 selftest.go（写探测文件 → 扫描路径确认 → 删除）。

func (p *Poll) Stats() Stats { return Stats{Backend: "poll"} }

func (p *Poll) Close() error { return nil }

func ms(d time.Duration) int64 { return int64(d / time.Millisecond) }

// normalizeWalkRoot — Windows 下加 \\?\ 前缀避免 MAX_PATH 260 限制。
// filepath 已把 \\?\C:\ 识别为设备前缀卷名，Clean/Join/Rel 均安全（Go internal/filepathlite）。
func normalizeWalkRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" &&
		!strings.HasPrefix(abs, `\\?\`) && len(abs) >= 2 && abs[1] == ':' {
		abs = `\\?\` + abs
	}
	return abs, nil
}

// queueState — 有远端副本 → 修改（LOCAL_MOD）；没有 → 新增（LOCAL_NEW）。
func queueState(rec index.SnapRec) int {
	if rec.HasRemote {
		return index.StateLocalMod
	}
	return index.StateLocalNew
}

// ScanAll — 一次全量扫描：WalkDir + 内存快照比对 + 分批写库（design §6.2）。
//
// 性能要点：稳态（无变更）下零写库 —— 未变路径不动数据库，
// 删除判定用本轮内存 seen 集合，而不是逐行 bump scan_run（省掉每轮 ~15MB 的 WAL 写放大）。
func (p *Poll) ScanAll(ctx context.Context, runID int64) (*ScanResult, error) {
	start := time.Now()
	res := &ScanResult{Root: p.root, Backend: "poll", RunID: runID, WalkOnly: p.index == nil}
	defer func() {
		res.WallMs = ms(time.Since(start))
		if res.WalkOK {
			res.At = time.Now()
		}
	}()

	walkRoot, err := normalizeWalkRoot(p.root)
	if err != nil {
		return nil, err
	}

	var snap map[string]index.SnapRec
	if p.index != nil {
		t0 := time.Now()
		snap, err = p.index.LoadSnapshot(ctx)
		if err != nil {
			return nil, fmt.Errorf("加载索引快照失败: %w", err)
		}
		res.LoadMs = ms(time.Since(t0))
	}

	seen := make(map[string]struct{}, len(snap)+1024)
	batch := make([]index.Change, 0, p.opts.BatchSize)
	var dirRefs []index.DirRef // Windows：采集目录引用号（usn 模式路径解析用）

	flush := func() error {
		if p.index == nil || len(batch) == 0 {
			batch = batch[:0]
			return nil
		}
		d, err := p.index.CommitBatch(ctx, runID, batch)
		res.CommitMs += ms(d)
		res.CommitCount++
		if err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	add := func(c index.Change) error {
		batch = append(batch, c)
		if len(batch) >= p.opts.BatchSize {
			return flush()
		}
		return nil
	}

	// markConflict — 网盘不接受的文件名：标 CONFLICT 拦下（design §10 扫描阶段拦截，
	// 绝不排队上传、绝不等传到一半才失败）。已是 CONFLICT 行 → 稳态零写入。
	markConflict := func(rel string, isDir bool, size, mtime int64, msg string) error {
		rec, ok, err := p.index.LookupEntry(ctx, rel)
		if err != nil {
			return err
		}
		if ok && rec.State == index.StateConflict {
			return nil
		}
		kind := index.ChangeInsert
		if ok {
			kind = index.ChangeUpdate
		}
		return add(index.Change{Kind: kind, RelPath: rel, IsDir: isDir,
			Size: size, MtimeNs: mtime, State: index.StateConflict, Err: msg})
	}

	walkErr := filepath.WalkDir(walkRoot, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			if path == walkRoot {
				return err // 根不可读 → 致命
			}
			res.Skipped++
			res.noteErr(err)
			if de != nil && de.IsDir() {
				return fs.SkipDir // 整个子树不可读（权限等）：跳过，不中断
			}
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}

		rel, rerr := filepath.Rel(walkRoot, path)
		if rerr != nil {
			res.Skipped++
			return nil
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// 排除规则（design §14）：不跟踪 → 全量对账时自然归入 REMOVED_LOCAL
		if p.matcher.Match(rel) {
			if de.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// 符号链接 / junction：不跟随、不备份链接本身（--follow-links 以后再说）
		if de.Type()&fs.ModeSymlink != 0 {
			res.Symlinks++
			if de.IsDir() {
				return fs.SkipDir // 目录联接：不进入
			}
			return nil
		}

		if de.IsDir() {
			res.Dirs++
			if ref, ok := dirFileRef(path); ok {
				dirRefs = append(dirRefs, index.DirRef{Ref: ref, RelPath: rel})
			}
			// 网盘文件名校验（design §10）：目录非法 → 拦下且不进入子树
			if verr := netdiskname.Validate(rel); verr != nil {
				res.Conflicted++
				seen[rel] = struct{}{}
				if p.index == nil {
					return fs.SkipDir
				}
				if err := markConflict(rel, true, 0, 0, netdiskname.ErrPrefix+verr.Error()); err != nil {
					return err
				}
				return fs.SkipDir
			}
			if p.index == nil {
				return nil // walk-only
			}
			rec, ok := snap[rel]
			if ok {
				seen[rel] = struct{}{}
				switch {
				case !rec.IsDir:
					// 类型变化（文件→目录）：按新形态重建
					info, e := de.Info()
					var size, mtime int64
					if e == nil {
						size, mtime = info.Size(), info.ModTime().UnixNano()
					} else {
						res.noteErr(e)
					}
					res.Modified++
					return add(index.Change{Kind: index.ChangeUpdate, RelPath: rel, IsDir: true,
						Size: size, MtimeNs: mtime, State: index.StateLocalNew})
				case rec.State == index.StateRemovedLocal:
					// 删除后重建的目录：复活
					res.Resurrected++
					return add(index.Change{Kind: index.ChangeUpdate, RelPath: rel, IsDir: true,
						Size: rec.Size, MtimeNs: rec.MtimeNs, State: index.StateLocalNew})
				default:
					// 目录 mtime 变化不代表远端需要动作（子文件会各自上报），不产生变更
					res.Unchanged++
					return nil
				}
			}
			info, e := de.Info()
			var size, mtime int64
			if e == nil {
				size, mtime = info.Size(), info.ModTime().UnixNano()
			} else {
				res.noteErr(e)
			}
			res.AddedDirs++
			return add(index.Change{Kind: index.ChangeInsert, RelPath: rel, IsDir: true,
				Size: size, MtimeNs: mtime, State: index.StateLocalNew})
		}

		// 普通文件（Windows 上 Info() 用的是 FindFirstFile 缓存数据，无额外 syscall）
		info, ierr := de.Info()
		if ierr != nil {
			res.Skipped++
			res.noteErr(ierr)
			return nil
		}
		size, mtime := info.Size(), info.ModTime().UnixNano()
		res.Files++
		res.Bytes += size

		// 网盘文件名校验（design §10）：扫描阶段拦截
		if verr := netdiskname.Validate(rel); verr != nil {
			res.Conflicted++
			seen[rel] = struct{}{}
			if p.index == nil {
				return nil
			}
			if err := markConflict(rel, false, size, mtime, netdiskname.ErrPrefix+verr.Error()); err != nil {
				return err
			}
			return nil
		}
		if p.index == nil {
			return nil // walk-only
		}

		rec, ok := snap[rel]
		if !ok {
			res.AddedFiles++
			return add(index.Change{Kind: index.ChangeInsert, RelPath: rel,
				Size: size, MtimeNs: mtime, State: index.StateLocalNew})
		}
		seen[rel] = struct{}{}
		switch {
		case rec.IsDir:
			// 类型变化（目录→文件）
			res.Modified++
			return add(index.Change{Kind: index.ChangeUpdate, RelPath: rel,
				Size: size, MtimeNs: mtime, State: queueState(rec)})
		case size != rec.Size || mtime != rec.MtimeNs:
			res.Modified++
			return add(index.Change{Kind: index.ChangeUpdate, RelPath: rel,
				Size: size, MtimeNs: mtime, State: queueState(rec)})
		case rec.State == index.StateRemovedLocal:
			// 本地删除后又出现：偏向重传而非跳过（design §9.3 宁可多传，不漏传）
			res.Resurrected++
			return add(index.Change{Kind: index.ChangeUpdate, RelPath: rel,
				Size: size, MtimeNs: mtime, State: queueState(rec)})
		default:
			res.Unchanged++
			return nil
		}
	})

	if walkErr != nil {
		return res, fmt.Errorf("遍历中断（本轮不做删除判定）: %w", walkErr)
	}
	if p.index == nil {
		res.WalkOK = true
		return res, nil
	}

	// 删除判定：本轮没见到的 = 本地已删（远端保留，design §9.2）。
	// 只有遍历完整（walkErr == nil）才允许做 —— 半途而废的扫描不能产生删除结论。
	var missing []string
	for rel, rec := range snap {
		if _, ok := seen[rel]; ok {
			continue
		}
		if rec.State == index.StateRemovedLocal {
			continue // 已经是删除态，不用重复写
		}
		// 注：CONFLICT 行在其仍存在时必被 seen（冲突处理里标记），走到这里
		// 说明文件已删除 → 正常降级 REMOVED_LOCAL，不留陈旧拦截记录。
		missing = append(missing, rel)
	}
	res.Removed = int64(len(missing))
	if len(missing) > 0 {
		d, err := p.index.MarkMissing(ctx, runID, missing)
		res.CommitMs += ms(d)
		res.CommitCount++
		if err != nil {
			return res, fmt.Errorf("标记删除失败: %w", err)
		}
	}
	// 收尾未满一批的
	if len(batch) > 0 {
		d, err := p.index.CommitBatch(ctx, runID, batch)
		res.CommitMs += ms(d)
		res.CommitCount++
		if err != nil {
			return res, fmt.Errorf("写库失败: %w", err)
		}
		batch = batch[:0]
	}

	// 目录引用号全量重建（usn 模式的路径解析表；全量扫描时重建可自愈陈旧引用）
	if len(dirRefs) > 0 {
		if ref, ok := dirFileRef(walkRoot); ok {
			dirRefs = append(dirRefs, index.DirRef{Ref: ref, RelPath: ""}) // root 本身 → ""
		}
		if err := p.index.ResetDirRefs(ctx); err != nil {
			res.noteErr(fmt.Errorf("dir_ref 重建失败: %w", err))
		} else if err := p.index.AddDirRefs(ctx, dirRefs); err != nil {
			res.noteErr(fmt.Errorf("dir_ref 写入失败: %w", err))
		}
	}

	// WAL checkpoint：每轮截断，防膨胀（design §4.8）
	t0 := time.Now()
	if err := p.index.Checkpoint(ctx); err != nil {
		res.noteErr(fmt.Errorf("checkpoint: %w", err))
	}
	res.CheckpointMs = ms(time.Since(t0))

	res.WalkOK = true
	return res, nil
}
