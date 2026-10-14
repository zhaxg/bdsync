package sender

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"syncer/internal/baidu"
	"syncer/internal/index"
)

// TestRenameE2E_Live — 改名管道真机端到端（M2 验收）。
// 沙箱内无法读 USN journal，故用 DAO 调用精确模拟 RunUsnCycle 的改名处理：
// 本地真实改名 + st.MoveEntry / st.RewriteSubtree（usn 路径同款调用）→ Drain → 远端核对。
//
// 环境变量：SYNCER_COOKIE（必需）、SYNCER_TEST_DB、SYNCER_TEST_ROOT、SYNCER_TEST_REMOTE。
// 未设置时跳过（不进 CI）。
func TestRenameE2E_Live(t *testing.T) {
	cookie := os.Getenv("SYNCER_COOKIE")
	dbPath := os.Getenv("SYNCER_TEST_DB")
	root := os.Getenv("SYNCER_TEST_ROOT")
	remote := os.Getenv("SYNCER_TEST_REMOTE")
	if cookie == "" || dbPath == "" || root == "" || remote == "" {
		t.Skip("live test：需设置 SYNCER_COOKIE / SYNCER_TEST_DB / SYNCER_TEST_ROOT / SYNCER_TEST_REMOTE")
	}
	ctx := context.Background()

	st, err := index.OpenExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bc, err := baidu.New(baidu.DefaultAppID, cookie)
	if err != nil {
		t.Fatal(err)
	}
	snd := New(st, bc, Options{
		LocalRoot:   root,
		RemoteRoot:  remote,
		MirrorOn:    true,
		HistoryDir:  ".history",
		OnRename:    "mv",
		Concurrency: 4,
		Logf:        t.Logf,
	})

	// ── 0. 幂等基线：还原上次演练的改名（本地改回 + 索引登记 → Drain 用 rename 管道把远端搬回）──
	if _, err := os.Stat(filepath.Join(root, "p4.bin")); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(root, "p4ren.bin")); err == nil {
			if err := os.Rename(filepath.Join(root, "p4ren.bin"), filepath.Join(root, "p4.bin")); err != nil {
				t.Fatal(err)
			}
			if err := st.MoveEntry(ctx, "p4ren.bin", "p4.bin"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "sub")); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(root, "sub2")); err == nil {
			if err := os.Rename(filepath.Join(root, "sub2"), filepath.Join(root, "sub")); err != nil {
				t.Fatal(err)
			}
			if err := st.RewriteSubtree(ctx, "sub2", "sub"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := snd.Drain(ctx, 0); err != nil {
		t.Fatalf("基线还原失败: %v", err)
	}

	// 取改名前的远端信息（用于断言 mv 是"移动"而非"重传"）
	preMeta, err := bc.Meta(remote + "/p4.bin")
	if err != nil {
		t.Fatalf("远端 p4.bin 应存在: %v", err)
	}

	// ── 1. 文件改名：本地真实改名 + 模拟 usn 的 MoveEntry ──
	if err := os.Rename(filepath.Join(root, "p4.bin"), filepath.Join(root, "p4ren.bin")); err != nil {
		t.Fatal(err)
	}
	if err := st.MoveEntry(ctx, "p4.bin", "p4ren.bin"); err != nil {
		t.Fatal(err)
	}

	// ── 2. 目录改名：sub → sub2 ──
	if _, err := os.Stat(filepath.Join(root, "sub")); err != nil {
		t.Fatalf("前置缺失（先跑 M1 基线）: %v", err)
	}
	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub2")); err != nil {
		t.Fatal(err)
	}
	if err := st.RewriteSubtree(ctx, "sub", "sub2"); err != nil {
		t.Fatal(err)
	}

	stats, err := snd.Drain(ctx, 0)
	if err != nil {
		t.Fatalf("Drain 失败: %v", err)
	}
	t.Logf("stats: %+v", stats.Snapshot())
	if stats.Renamed < 2 {
		t.Errorf("期望至少 2 次远端改名同步，得到 %d", stats.Renamed)
	}
	if stats.Uploaded != 0 {
		t.Errorf("纯改名不应产生上传（mv 即完成），Uploaded=%d", stats.Uploaded)
	}
	if stats.Archived != 0 {
		t.Errorf("纯改名不应产生历史归档（mv 零污染），Archived=%d", stats.Archived)
	}

	// ── 3. 远端核对 ──
	fi, err := bc.Meta(remote + "/p4ren.bin")
	if err != nil {
		t.Fatalf("远端新路径应存在: %v", err)
	}
	if fi.MD5 != preMeta.MD5 || fi.Size != preMeta.Size {
		t.Errorf("mv 应保持内容: 旧 %s/%d → 新 %s/%d", preMeta.MD5, preMeta.Size, fi.MD5, fi.Size)
	}
	if _, err := bc.Meta(remote + "/p4.bin"); !baidu.RemoteMissing(err) {
		t.Errorf("远端旧路径应消失，得到 err=%v", err)
	}
	if _, err := bc.Meta(remote + "/sub2/b.txt"); err != nil {
		t.Errorf("目录 mv 后子文件应在新位置: %v", err)
	}
	if _, err := bc.Meta(remote + "/sub/b.txt"); !baidu.RemoteMissing(err) {
		t.Errorf("目录旧路径应消失，得到 err=%v", err)
	}

	// ── 4. 索引核对 ──
	rec, ok, err := st.LookupEntry(ctx, "p4ren.bin")
	if err != nil || !ok {
		t.Fatalf("p4ren.bin 应在索引: %v %v", ok, err)
	}
	if rec.State != index.StateIdentical {
		t.Errorf("mv 完成后应撤回 IDENTICAL，得到 %d", rec.State)
	}
	items, err := st.ClaimRenamed(ctx, 10)
	if err != nil || len(items) != 0 {
		t.Errorf("改名队列应清空: %v %+v", err, items)
	}
}
