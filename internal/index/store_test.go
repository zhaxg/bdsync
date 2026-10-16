package index

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "idx.db"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func insertIdentical(t *testing.T, st *Store, rel string, size, mtime int64, fsid, rmtime int64) {
	t.Helper()
	ctx := context.Background()
	// 先落成 IN_PROGRESS（等价于"已被领取"）再走 FinishUpload：
	// FinishUpload 现带乐观锁，只认 state=IN_PROGRESS 且 mtime/size 与快照一致的行。
	if _, err := st.CommitBatch(ctx, 1, []Change{{
		Kind: ChangeInsert, RelPath: rel, Size: size, MtimeNs: mtime, State: StateInProgress,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishUpload(ctx, PendingEntry{RelPath: rel, Size: size, MtimeNs: mtime},
		fsid, size, rmtime, "md5-"+rel); err != nil {
		t.Fatal(err)
	}
}

// TestFinishUpload_SnapshotGuard -- 上传期间本地被再次修改时，旧快照的回写必须被
// 拒绝：否则那行会被标成 IDENTICAL，新版本永不重传（静默漏备份）。
func TestFinishUpload_SnapshotGuard(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if _, err := st.CommitBatch(ctx, 1, []Change{{
		Kind: ChangeInsert, RelPath: "g.txt", Size: 10, MtimeNs: 100, State: StateInProgress,
	}}); err != nil {
		t.Fatal(err)
	}
	// 模拟上传期间文件被改：Detector 写回"待传"并更新快照
	if _, err := st.CommitBatch(ctx, 2, []Change{{
		Kind: ChangeUpdate, RelPath: "g.txt", Size: 20, MtimeNs: 200, State: StateLocalMod,
	}}); err != nil {
		t.Fatal(err)
	}
	applied, err := st.FinishUpload(ctx, PendingEntry{RelPath: "g.txt", Size: 10, MtimeNs: 100},
		900, 10, 300, "md5-old")
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("快照不匹配时 FinishUpload 不应回写")
	}
	// 新版本必须仍能被领取重传
	pend, err := st.PendingList(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range pend {
		if e.RelPath == "g.txt" && e.Size == 20 {
			found = true
		}
	}
	if !found {
		t.Fatal("新版本应仍在待传队列中")
	}
}

func TestMoveEntry_SetsRenamedFromAndRequeues(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertIdentical(t, st, "a/b.txt", 10, 100, 500, 111)

	if err := st.MoveEntry(ctx, "a/b.txt", "a/c.txt"); err != nil {
		t.Fatal(err)
	}

	rec, ok, err := st.LookupEntry(ctx, "a/c.txt")
	if err != nil || !ok {
		t.Fatalf("新路径应存在: %v %v", ok, err)
	}
	if rec.State != StateLocalMod {
		t.Errorf("改名后应重新入队为 LOCAL_MOD，得到 state=%d", rec.State)
	}
	if !rec.HasRemote || rec.RSize != 10 {
		t.Errorf("r_* 应保留: %+v", rec)
	}
	if _, ok, _ := st.LookupEntry(ctx, "a/b.txt"); ok {
		t.Error("旧路径行应消失")
	}

	items, err := st.ClaimRenamed(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("ClaimRenamed: %v %d", err, len(items))
	}
	it := items[0]
	if it.RelPath != "a/c.txt" || it.OldPath != "a/b.txt" || !it.HasRemote ||
		it.RMtime != 111 || it.Size != 10 || it.MtimeNs != 100 {
		t.Errorf("改名条目错误: %+v", it)
	}

	// mv 成功且内容一致 → 撤回 IDENTICAL
	if err := st.MarkRenamedDone(ctx, "a/c.txt", true); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = st.LookupEntry(ctx, "a/c.txt")
	if rec.State != StateIdentical {
		t.Errorf("mv 成功应撤回 IDENTICAL，得到 %d", rec.State)
	}
	if items, _ := st.ClaimRenamed(ctx, 10); len(items) != 0 {
		t.Error("记账后不应再被领取")
	}
}

func TestRewriteSubtree_RenameRegistration(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// 目录行（真实全量扫描总会为目录建行）+ IDENTICAL 子文件 + 已删子文件
	if _, err := st.CommitBatch(ctx, 1, []Change{{
		Kind: ChangeInsert, RelPath: "d", IsDir: true, State: StateLocalNew,
	}}); err != nil {
		t.Fatal(err)
	}
	insertIdentical(t, st, "d/f.txt", 20, 200, 600, 222)
	if _, err := st.CommitBatch(ctx, 1, []Change{{
		Kind: ChangeInsert, RelPath: "d/g.txt", Size: 5, MtimeNs: 250, State: StateInProgress,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishUpload(ctx, PendingEntry{RelPath: "d/g.txt", Size: 5, MtimeNs: 250},
		700, 5, 233, "md5-g"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkMissing(ctx, 1, []string{"d/g.txt"}); err != nil {
		t.Fatal(err)
	}

	if err := st.RewriteSubtree(ctx, "d", "e"); err != nil {
		t.Fatal(err)
	}

	// 路径已重写
	if _, ok, _ := st.LookupEntry(ctx, "e/f.txt"); !ok {
		t.Error("子文件路径应重写为 e/f.txt")
	}
	// 目录行登记旧前缀
	items, err := st.ClaimRenamed(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].RelPath != "e" || items[0].OldPath != "d" || !items[0].IsDir {
		t.Fatalf("目录改名条目错误: %+v", items)
	}
	// IDENTICAL 子文件被重新入队（非镜像模式兼容；mv 成功后撤回）
	frec, _, _ := st.LookupEntry(ctx, "e/f.txt")
	if frec.State != StateLocalMod {
		t.Errorf("子文件应重新入队 LOCAL_MOD，得到 %d", frec.State)
	}
	// 撤回集合 = 该子文件
	rows, err := st.RenamedSubtreeRows(ctx, "e")
	if err != nil || len(rows) != 1 || rows[0].RelPath != "e/f.txt" {
		t.Fatalf("RenamedSubtreeRows: %v %+v", err, rows)
	}
	// 整棵清标记
	if err := st.ClearRenamedSubtree(ctx, "e"); err != nil {
		t.Fatal(err)
	}
	if items, _ := st.ClaimRenamed(ctx, 10); len(items) != 0 {
		t.Errorf("子树标记应清空，仍剩 %+v", items)
	}

	// 已删行：改名后归档源指向旧路径
	hq, err := st.HistoryQueue(ctx, 10)
	if err != nil || len(hq) != 1 {
		t.Fatalf("HistoryQueue: %v %d", err, len(hq))
	}
	if hq[0].RelPath != "e/g.txt" || hq[0].OldPath != "d/g.txt" || hq[0].RMtime != 233 {
		t.Errorf("历史条目应带旧路径: %+v", hq[0])
	}
	// state=9 行不进改名领取（归历史阶段处理）
	for _, it := range items {
		if it.RelPath == "e/g.txt" {
			t.Error("state=9 行不应出现在 ClaimRenamed")
		}
	}
}

func TestLocalManifest(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertIdentical(t, st, "keep.txt", 1, 1, 1, 1)
	if _, err := st.CommitBatch(ctx, 1, []Change{{
		Kind: ChangeInsert, RelPath: "d", IsDir: true, State: StateLocalNew,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitBatch(ctx, 1, []Change{{
		Kind: ChangeInsert, RelPath: "gone.txt", Size: 1, MtimeNs: 1, State: StateLocalNew,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkMissing(ctx, 1, []string{"gone.txt"}); err != nil {
		t.Fatal(err)
	}

	m, err := st.LocalManifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["keep.txt"]; !ok {
		t.Error("清单应含 keep.txt")
	}
	if !m["d"] {
		t.Error("清单应含目录 d 且 isDir=true")
	}
	if m["keep.txt"] {
		t.Error("keep.txt 是文件，isDir 应为 false")
	}
	if _, ok := m["gone.txt"]; ok {
		t.Error("REMOVED_LOCAL 行不应在清单中（否则远端副本不会被对账归档）")
	}
}
