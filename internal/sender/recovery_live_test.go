package sender

import (
	"context"
	"os"
	"testing"
	"time"

	"bdsync/internal/baidu"
	"bdsync/internal/index"
)

// TestRecoverArchiveIncident_Live — 误归档事故恢复演练（也是真实恢复预案）。
//
// 背景：M3 开发中曾因清单判断 bug 把主树全部误归档（对账把"文件清单值=false"
// 误判为不存在）。恢复思路：内容仍在服务端（.history）→ 全部重新入队 →
// 秒传命中，零带宽重建主树。此测试固化该预案。
func TestRecoverArchiveIncident_Live(t *testing.T) {
	cookie := os.Getenv("BDSYNC_COOKIE")
	dbPath := os.Getenv("BDSYNC_TEST_DB")
	root := os.Getenv("BDSYNC_TEST_ROOT")
	remote := os.Getenv("BDSYNC_TEST_REMOTE")
	if cookie == "" || dbPath == "" || root == "" || remote == "" {
		t.Skip("live test：需设置 BDSYNC_COOKIE / BDSYNC_TEST_DB / BDSYNC_TEST_ROOT / BDSYNC_TEST_REMOTE")
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
		LocalRoot: root, RemoteRoot: remote,
		MirrorOn: true, HistoryDir: ".history", HistoryDays: 90, OnRename: "mv",
		Concurrency: 6, Settle: time.Millisecond, // 恢复演练不需要稳定性等待
		Logf: t.Logf,
	})

	// 清单内文件全部清远端记账 + 重新入队（模拟"远端副本丢失"）
	m, err := st.LocalManifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var changes []index.Change
	for rel, isDir := range m {
		if isDir {
			continue
		}
		if err := st.ClearRemote(ctx, rel); err != nil {
			t.Fatal(err)
		}
		rec, ok, lerr := st.LookupEntry(ctx, rel)
		if lerr != nil || !ok {
			t.Fatalf("lookup %s: %v %v", rel, ok, lerr)
		}
		changes = append(changes, index.Change{
			Kind: index.ChangeUpdate, RelPath: rel,
			Size: rec.Size, MtimeNs: rec.MtimeNs, State: index.StateLocalNew,
		})
	}
	if len(changes) == 0 {
		t.Skip("清单为空，无需恢复")
	}
	if _, err := st.CommitBatch(ctx, 999, changes); err != nil {
		t.Fatal(err)
	}
	t.Logf("重新入队 %d 个文件（内容仍在服务端 → 应全部秒传命中）", len(changes))

	stats, err := snd.Drain(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	snap := stats.Result()
	t.Logf("stats: %+v", snap)
	// 0 字节文件走单请求直传（无法秒传），其余应全部秒传命中
	if snap.RapidOK+snap.Uploaded != len(changes) || snap.Failed != 0 {
		t.Errorf("应全部恢复（秒传 %d + 直传/上传 %d = %d），实际 秒传 %d 上传 %d 失败 %d",
			snap.RapidOK, snap.Uploaded, len(changes), snap.RapidOK, snap.Uploaded, snap.Failed)
	}
	if snap.Archived != 0 {
		t.Errorf("恢复不应产生归档: %+v", snap)
	}

	// 主树回归核对
	for _, rel := range []string{"a.txt", "p4ren.bin", "sub2/b.txt", "中文文件.txt"} {
		if _, err := bc.Meta(remote + "/" + rel); err != nil {
			t.Errorf("主树应已重建 %s: %v", rel, err)
		}
	}
}
