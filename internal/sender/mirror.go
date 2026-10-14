package sender

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"syncer/internal/baidu"
	"syncer/internal/index"
)

// 镜像模式（config.MirrorConfig）：folderRA 主树路径清单永远等于本地清单；
// 所有会被镜像操作销毁的远端旧版本先归档进 .history（服务器端 Move：零带宽，
// 一次调用同时完成"历史写入 + 主树删除"），到期整目录清理。
//
// 幂等设计（崩溃可重入）：归档目标名确定性 —— <history>/<UTC日期>/<rel>@<r_mtime>
//   1. Meta(目标) 已存在 ⇒ 上次移动已成功、只是记账没写 → 直接补账
//   2. Move(源 → 目标)
//   3. 源 31066 不存在 ⇒ 已移动过/从未存在 → 视为完成

// archiveRemote — 归档远端主树上的旧版本。
//
//	targetBaseRel：归档目标的相对路径基准（通常= 行的 rel_path）
//	srcRel：远端源路径（改名后又删除的行，源还在改名前的位置）
//
// 返回是否确实发生了移动。
func (s *Sender) archiveRemote(ctx context.Context, targetBaseRel, srcRel string, rMtime int64) (bool, error) {
	if !s.mirrorOn || s.historyDir == "" {
		return false, nil
	}
	src := path.Join(s.remoteRoot, srcRel)
	date := time.Now().UTC().Format("2006-01-02")
	target := path.Join(s.remoteRoot, s.historyDir, date,
		targetBaseRel+"@"+strconv.FormatInt(rMtime, 10))

	// 1) 幂等检查
	fi, merr := s.bc.Meta(target)
	if merr == nil && fi != nil {
		return true, nil // 已归档（上次崩溃在记账前）
	}
	if merr != nil && !baidu.RemoteMissing(merr) {
		return false, fmt.Errorf("归档目标检查失败: %w", merr)
	}

	// 2) 目标父目录链（.history/<日期>/…）
	if err := s.ensureRemoteChain(target); err != nil {
		return false, err
	}

	// 3) 服务器端移动
	if err := s.bc.Move(src, target); err != nil {
		if baidu.RemoteMissing(err) {
			return false, nil // 源已不在（并发/重复处理）→ 无需归档
		}
		return false, fmt.Errorf("归档移动失败: %w", err)
	}
	return true, nil
}

// ensureRemoteChain — 确保 full 的父目录链存在（含 remoteRoot，不含网盘根 /）。
// 并发安全（uploadOne 的归档前置与批量阶段都会调用）。
func (s *Sender) ensureRemoteChain(full string) error {
	s.remoteDirsMu.Lock()
	defer s.remoteDirsMu.Unlock()

	var chain []string
	for d := path.Dir(full); d != "/" && d != "." && d != ""; d = path.Dir(d) {
		chain = append(chain, d)
		if d == s.remoteRoot {
			break
		}
	}
	sort.Slice(chain, func(i, j int) bool {
		return strings.Count(chain[i], "/") < strings.Count(chain[j], "/")
	})
	for _, d := range chain {
		if s.remoteDirs[d] {
			continue
		}
		if err := s.bc.Mkdir(d); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
		s.remoteDirs[d] = true
	}
	return nil
}

// historyDateBucket — 归档日期桶（UTC，展示/日志用）。
func historyDateBucket() string { return time.Now().UTC().Format("2006-01-02") }

// kvPruneAt — 历史清理时间戳（最多每日一次）。
const kvPruneAt = "hist_prune_at"

// pruneIfDue — 历史过期清理（kv 守卫：最多每日一次；成功才记账，失败下轮重试）。
func (s *Sender) pruneIfDue(ctx context.Context) {
	raw, _ := s.st.GetKV(ctx, kvPruneAt)
	var last int64
	if len(raw) > 0 {
		last, _ = strconv.ParseInt(string(raw), 10, 64)
	}
	now := time.Now().Unix()
	if now-last < 24*3600 {
		return
	}
	removed, err := s.PruneHistory(ctx, s.historyDays, false)
	if err != nil {
		s.logf("[!] 历史清理失败（下轮重试）: %v", err)
		return
	}
	if err := s.st.PutKV(ctx, kvPruneAt, []byte(strconv.FormatInt(now, 10))); err != nil {
		return
	}
	if len(removed) > 0 {
		s.logf("[历史] 清理过期归档 %d 个日期桶（保留 %d 天）: %s",
			len(removed), s.historyDays, strings.Join(removed, ", "))
	}
}

// PruneHistory — 清理过期历史日期桶（服务器端递归删除；design镜像 history_days）。
// 只删形如 YYYY-MM-DD 的桶；人工放进去的非日期目录永不动。
func (s *Sender) PruneHistory(ctx context.Context, days int, dryRun bool) ([]string, error) {
	if !s.mirrorOn || s.historyDir == "" || days <= 0 {
		return nil, nil
	}
	root := path.Join(s.remoteRoot, s.historyDir)
	items, err := s.bc.List(root)
	if err != nil {
		if baidu.RemoteMissing(err) {
			return nil, nil // .history 尚不存在
		}
		return nil, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	var removed []string
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !it.Isdir {
			continue
		}
		name := path.Base(it.Path)
		d, perr := time.Parse("2006-01-02", name)
		if perr != nil {
			continue // 非日期桶：不动
		}
		if !d.Before(cutoff) {
			continue // 未过期
		}
		if !dryRun {
			if rerr := s.bc.Remove(it.Path); rerr != nil && !baidu.RemoteMissing(rerr) {
				s.logf("[!] 历史桶删除失败 %s: %v", name, rerr)
				continue
			}
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// ── 改名远端同步（R 阶段，design镜像 Q1：mv 优先，零历史污染零带宽）──

// drainRenames — 改名队列排空（串行；目录优先，一次 mv 整棵子树）。
func (s *Sender) drainRenames(ctx context.Context, stats *Stats) error {
	tried := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		items, err := s.st.ClaimRenamed(ctx, 100)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		newAny := false
		for _, it := range items {
			if tried[it.RelPath] {
				continue
			}
			newAny = true
			tried[it.RelPath] = true
			s.renameOne(ctx, it, stats)
		}
		if !newAny {
			return nil
		}
	}
}

// renameOne — 单个改名的远端同步。
func (s *Sender) renameOne(ctx context.Context, it index.PendingRenamed, stats *Stats) {
	// history 策略（配置降级）：归档旧版 + 正常流程重传
	if s.onRename == "history" {
		if it.HasRemote || it.IsDir {
			archived, aerr := s.archiveRemote(ctx, it.RelPath, it.OldPath, it.RMtime)
			if aerr != nil {
				s.logf("[!] 改名归档失败（下轮重试）%s: %v", it.RelPath, aerr)
				return
			}
			if archived {
				if err := s.st.ClearRemote(ctx, it.RelPath); err != nil {
					s.logf("[!] %s 归档记账失败: %v", it.RelPath, err)
				}
				stats.apply(func(st *Stats) { st.Archived++ })
			}
		}
		if err := s.st.MarkRenamedDone(ctx, it.RelPath, false); err != nil {
			s.logf("[!] %s 改名记账失败: %v", it.RelPath, err)
		}
		stats.apply(func(st *Stats) { st.Renamed++ })
		s.logf("[↻] %s → %s（history 策略：归档+重传）", it.OldPath, it.RelPath)
		return
	}

	// mv 策略（默认）
	if !it.IsDir && !it.HasRemote {
		// 远端无副本可移动：清标记，行状态不变（LOCAL_NEW → 正常上传）
		if err := s.st.MarkRenamedDone(ctx, it.RelPath, false); err != nil {
			s.logf("[!] %s 改名记账失败: %v", it.RelPath, err)
		}
		return
	}

	dst := path.Join(s.remoteRoot, it.RelPath)

	// 幂等：目标已存在 = 上次 mv 已成功（崩溃在记账前）
	if fi, merr := s.bc.Meta(dst); merr == nil && fi != nil {
		s.finishRenameMv(ctx, it, stats)
		return
	} else if merr != nil && !baidu.RemoteMissing(merr) {
		s.logf("[!] 改名检查失败（下轮重试）%s: %v", it.RelPath, merr)
		return
	}

	if err := s.ensureRemoteChain(dst); err != nil {
		s.logf("[!] 改名建目录失败（下轮重试）%s: %v", it.RelPath, err)
		return
	}
	src := path.Join(s.remoteRoot, it.OldPath)
	if err := s.bc.Move(src, dst); err != nil {
		if baidu.RemoteMissing(err) {
			// 旧路径远端已不存在：无从移动 → 交给上传流程补新路径
			if err := s.st.MarkRenamedDone(ctx, it.RelPath, false); err != nil {
				s.logf("[!] %s 改名记账失败: %v", it.RelPath, err)
			}
			return
		}
		s.logf("[!] 改名移动失败（下轮重试）%s → %s: %v", it.OldPath, it.RelPath, err)
		return
	}
	s.finishRenameMv(ctx, it, stats)
}

// finishRenameMv — mv 成功后的记账：
//
//	文件：本地未变 → 撤回到 IDENTICAL（远端内容已随 mv 到位）；
//	     本地已变 → 保持待传（上传前置归档会先处理刚移过来的旧版）
//	目录：清整棵子树标记 + 把"改名引发的重新入队"按本地实况撤回
func (s *Sender) finishRenameMv(ctx context.Context, it index.PendingRenamed, stats *Stats) {
	if it.IsDir {
		if err := s.st.ClearRenamedSubtree(ctx, it.RelPath); err != nil {
			s.logf("[!] %s 子树改名标记清理失败: %v", it.RelPath, err)
			return
		}
		rows, err := s.st.RenamedSubtreeRows(ctx, it.RelPath)
		if err != nil {
			s.logf("[!] %s 子树行读取失败: %v", it.RelPath, err)
			return
		}
		settled := 0
		for _, r := range rows {
			full := filepath.Join(s.localRoot, filepath.FromSlash(r.RelPath))
			if info, serr := os.Stat(full); serr == nil &&
				info.Size() == r.Size && info.ModTime().UnixNano() == r.MtimeNs {
				if err := s.st.MarkRenamedDone(ctx, r.RelPath, true); err == nil {
					settled++
				}
			}
		}
		stats.apply(func(st *Stats) { st.Renamed++ })
		s.logf("[↻] %s → %s（远端整棵移动，%d 个子文件确认一致）", it.OldPath, it.RelPath, settled)
		return
	}

	identical := false
	full := filepath.Join(s.localRoot, filepath.FromSlash(it.RelPath))
	if info, err := os.Stat(full); err == nil &&
		info.Size() == it.Size && info.ModTime().UnixNano() == it.MtimeNs {
		identical = true
	}
	if err := s.st.MarkRenamedDone(ctx, it.RelPath, identical); err != nil {
		s.logf("[!] %s 改名记账失败: %v", it.RelPath, err)
	}
	stats.apply(func(st *Stats) { st.Renamed++ })
	s.logf("[↻] %s → %s（远端已移动%s）", it.OldPath, it.RelPath,
		map[bool]string{true: "，内容一致", false: "，本地内容另有变更待传"}[identical])
}

// ── 远端清单对账（design镜像 Q2：RA 主树路径清单 ≡ 本地清单，清单外 → 归档）──

// ReconcileResult — 对账结果。
type ReconcileResult struct {
	DryRun        bool
	Dirs          int // 远端目录数（不含 .history）
	Files         int // 远端文件数
	Kept          int // 与本地清单一致的文件
	MissingDirs   int // 本地有、远端缺的目录（将补建，含空目录）
	ArchivedFiles int // 清单外文件 → 归档
	ArchivedDirs  int // 清单外目录 → 整棵一次归档
	Errors        int
	Samples       []string // 前 50 项（dry-run 预览 / 实际动作记录）
	Elapsed       time.Duration
}

func (r *ReconcileResult) Summary() string {
	return fmt.Sprintf("远端 %d 目录 / %d 文件，与清单一致 %d | 补建目录 %d | 归档 %d 文件 + %d 目录（整棵）| 错误 %d（%s）",
		r.Dirs, r.Files, r.Kept, r.MissingDirs, r.ArchivedFiles, r.ArchivedDirs, r.Errors,
		r.Elapsed.Round(time.Millisecond))
}

// ReconcileRemote — 全量列举 RA 并与本地清单对账：
//   - 并发 BFS 列举（跳过 .history；即 design Phase3.12 的"并发 BFS"落地）
//   - 清单外文件 → 逐个归档；清单外目录 → 浅者先整棵一次归档（深者届时源已不存在，自动跳过）
//   - 本地有、远端缺的目录（含空目录）→ 补建（保证"完全映射"）
//   - 列举失败的子树本轮跳过（少归档不误归档；下轮对账自愈）
func (s *Sender) ReconcileRemote(ctx context.Context, dryRun bool) (*ReconcileResult, error) {
	if !s.mirrorOn || s.historyDir == "" {
		return nil, fmt.Errorf("远端清单对账需要开启镜像模式（mirror.enabled）")
	}
	t0 := time.Now()
	manifest, err := s.st.LocalManifest(ctx)
	if err != nil {
		return nil, err
	}
	res := &ReconcileResult{DryRun: dryRun}

	type unknownItem struct {
		rel   string
		mtime int64
	}
	var (
		mu           sync.Mutex
		unknownFiles []unknownItem
		unknownDirs  []unknownItem
	)
	knownDirs := map[string]bool{s.remoteRoot: true}

	// ── 并发 BFS 列举（.history 永不参与）──
	const workers = 8
	level := []string{s.remoteRoot}
	for len(level) > 0 {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var (
			wg   sync.WaitGroup
			sem  = make(chan struct{}, workers)
			next []string
		)
		for _, dir := range level {
			wg.Add(1)
			sem <- struct{}{}
			go func(dir string) {
				defer func() { <-sem; wg.Done() }()
				items, lerr := s.bc.List(dir)
				mu.Lock()
				defer mu.Unlock()
				if lerr != nil {
					res.Errors++
					s.logf("[对账] 列举失败 %s: %v（该子树本轮跳过，下轮自愈）", dir, lerr)
					return
				}
				for _, it := range items {
					rel := strings.TrimPrefix(it.Path, s.remoteRoot+"/")
					if rel == "" {
						continue
					}
					if rel == s.historyDir || strings.HasPrefix(rel, s.historyDir+"/") {
						continue
					}
					_, inManifest := manifest[rel] // 值=isDir，判断存在必须显式取第二个返回值！
					if it.Isdir {
						res.Dirs++
						knownDirs[rel] = true
						next = append(next, it.Path)
						if !inManifest {
							unknownDirs = append(unknownDirs, unknownItem{rel, it.Mtime})
						}
					} else {
						res.Files++
						if inManifest {
							res.Kept++
						} else {
							unknownFiles = append(unknownFiles, unknownItem{rel, it.Mtime})
						}
					}
				}
			}(dir)
		}
		wg.Wait()
		level = next
	}

	// ── 本地有、远端缺的目录 → 补建（含空目录，保证完全映射）──
	var missingDirs []string
	for rel, isDir := range manifest {
		if isDir && !knownDirs[rel] {
			missingDirs = append(missingDirs, rel)
		}
	}
	sort.Slice(missingDirs, func(i, j int) bool {
		return strings.Count(missingDirs[i], "/") < strings.Count(missingDirs[j], "/")
	})
	res.MissingDirs = len(missingDirs)
	if !dryRun {
		for _, rel := range missingDirs {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			full := path.Join(s.remoteRoot, rel)
			if err := s.ensureRemoteChain(full); err != nil {
				res.Errors++
				s.logf("[对账] 补建目录失败 %s: %v", rel, err)
				continue
			}
			if err := s.bc.Mkdir(full); err != nil {
				res.Errors++
				s.logf("[对账] 补建目录失败 %s: %v", rel, err)
				continue
			}
			s.logf("[对账·补建] %s/", rel)
		}
	}

	// ── 归档清单外目录：浅者先（整棵一次归档；深者届时自动跳过）──
	sort.Slice(unknownDirs, func(i, j int) bool {
		return strings.Count(unknownDirs[i].rel, "/") < strings.Count(unknownDirs[j].rel, "/")
	})
	collapsed := make([]unknownItem, 0, len(unknownDirs))
	for _, d := range unknownDirs {
		covered := false
		for _, c := range collapsed {
			if strings.HasPrefix(d.rel, c.rel+"/") {
				covered = true
				break
			}
		}
		if !covered {
			collapsed = append(collapsed, d)
		}
	}

	// ── 归档：清单外文件先、目录后 ──
	archiveOne := func(u unknownItem, isDir bool) {
		if err := ctx.Err(); err != nil {
			return
		}
		suffix := ""
		if isDir {
			suffix = "/（整棵）"
		}
		if dryRun {
			if isDir {
				res.ArchivedDirs++
			} else {
				res.ArchivedFiles++
			}
			if len(res.Samples) < 50 {
				res.Samples = append(res.Samples, u.rel+suffix)
			}
			return
		}
		archived, aerr := s.archiveRemote(ctx, u.rel, u.rel, u.mtime)
		if aerr != nil {
			if isRateLimited(aerr) {
				s.lim.onRateLimit()
			}
			res.Errors++
			s.logf("[对账] 归档失败 %s: %v", u.rel, aerr)
			return
		}
		if !archived {
			return // 源已不存在（如被父目录整棵归档带走）
		}
		if isDir {
			res.ArchivedDirs++
		} else {
			res.ArchivedFiles++
		}
		if len(res.Samples) < 50 {
			res.Samples = append(res.Samples, u.rel+suffix)
		}
		s.logf("[对账·归档] %s%s", u.rel, suffix)
	}
	sort.Slice(unknownFiles, func(i, j int) bool { return unknownFiles[i].rel < unknownFiles[j].rel })
	for _, u := range unknownFiles {
		archiveOne(u, false)
	}
	for _, u := range collapsed {
		archiveOne(u, true)
	}

	res.Elapsed = time.Since(t0)
	return res, nil
}
