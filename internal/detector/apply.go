package detector

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"syncer/internal/index"
	"syncer/internal/netdiskname"
)

// ApplyChanges — 把变更路径集合落到索引（usn 模式专用）：
// 逐路径 stat + 点查比对 + 分批原子写。
//
// 硬规矩②：判定逻辑永远读索引，不读事件流 —— 任何后端只是性能优化，
// 不可能引入正确性 bug。
//
// exclude 与 poll 共用同一套语义：被排除的路径归入 vanished（→ REMOVED_LOCAL，
// 停止跟踪）；非法文件名标 CONFLICT 拦截。
func ApplyChanges(ctx context.Context, st *index.Store, root string,
	changed, deleted []string, runID int64, exclude []string) (added, modified, resurrected, gone, conflicted int, err error) {

	walkRoot, err := normalizeWalkRoot(root)
	if err != nil {
		return
	}
	matcher := NewMatcher(exclude, runtime.GOOS == "windows")

	batch := make([]index.Change, 0, 2000)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, cerr := st.CommitBatch(ctx, runID, batch); cerr != nil {
			return cerr
		}
		batch = batch[:0]
		return nil
	}

	vanished := []string{} // 声明后立刻消失/被排除的路径
	for i := 0; i < len(changed); i++ {
		rel := changed[i]
		if matcher.Match(rel) {
			vanished = append(vanished, rel)
			continue
		}

		full := filepath.Join(walkRoot, filepath.FromSlash(rel))
		info, lerr := os.Lstat(full)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				vanished = append(vanished, rel)
			}
			continue // 其他读取错误：跳过，等对账扫描兜底
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue // 不备份链接本身（--follow-links 以后再说）
		}
		isDir := info.IsDir()
		size, mtime := info.Size(), info.ModTime().UnixNano()

		// 网盘文件名校验（design §10 扫描阶段拦截）
		if verr := netdiskname.Validate(rel); verr != nil {
			conflicted++
			rec, ok, serr := st.LookupEntry(ctx, rel)
			if serr != nil {
				err = serr
				return
			}
			if ok && rec.State == index.StateConflict {
				continue // 已记录 → 零写入
			}
			kind := index.ChangeInsert
			if ok {
				kind = index.ChangeUpdate
			}
			batch = append(batch, index.Change{Kind: kind, RelPath: rel, IsDir: isDir,
				Size: size, MtimeNs: mtime, State: index.StateConflict,
				Err: netdiskname.ErrPrefix + verr.Error()})
			continue
		}

		rec, ok, serr := st.LookupEntry(ctx, rel)
		if serr != nil {
			err = serr
			return
		}

		switch {
		case !ok:
			added++
			batch = append(batch, index.Change{
				Kind: index.ChangeInsert, RelPath: rel, IsDir: isDir,
				Size: size, MtimeNs: mtime, State: index.StateLocalNew,
			})
		case rec.IsDir != isDir:
			// 类型变化（文件↔目录）：按新形态重建
			modified++
			batch = append(batch, index.Change{
				Kind: index.ChangeUpdate, RelPath: rel, IsDir: isDir,
				Size: size, MtimeNs: mtime, State: index.StateLocalNew,
			})
		case isDir:
			// 目录：内容变化不代表远端需要动作（子文件各自上报）
		case size != rec.Size || mtime != rec.MtimeNs:
			modified++
			batch = append(batch, index.Change{
				Kind: index.ChangeUpdate, RelPath: rel,
				Size: size, MtimeNs: mtime, State: queueState(rec),
			})
		case rec.State == index.StateRemovedLocal:
			// 删除后又出现：偏向重传而非跳过（design §9.3）
			resurrected++
			batch = append(batch, index.Change{
				Kind: index.ChangeUpdate, RelPath: rel,
				Size: size, MtimeNs: mtime, State: queueState(rec),
			})
		default:
			// journal 的信息级事件（如 CLOSE），内容未变 → 零写库
		}
		if len(batch) >= 2000 {
			if err = flush(); err != nil {
				return
			}
		}
	}
	if err = flush(); err != nil {
		return
	}

	deleted = append(deleted, vanished...)
	for i := 0; i < len(deleted); i += 2000 {
		end := i + 2000
		if end > len(deleted) {
			end = len(deleted)
		}
		if _, merr := st.MarkMissing(ctx, runID, deleted[i:end]); merr != nil {
			err = merr
			return
		}
		gone += end - i
	}
	return
}
