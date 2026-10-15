package sender

import (
	"context"
	"os"
	"strings"
	"testing"

	"bdsync/internal/baidu"
	"bdsync/internal/index"
)

// TestReconcileAndPrune_Live -- M3 真机验收：远端清单对账（清单外 → 归档）
// 与历史过期清理。环境变量门控（同 rename_live_test）。
func TestReconcileAndPrune_Live(t *testing.T) {
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
		Concurrency: 4, Logf: t.Logf,
	})

	// ── 1. 远端制造"清单外"内容：一个未知目录 + 一个未知文件 ──
	if err := bc.Mkdir(remote + "/unknown_dir"); err != nil {
		t.Fatalf("制造未知目录失败: %v", err)
	}
	body := strings.NewReader("unknown content, should be archived")
	if err := bc.UploadSingle(remote+"/unknown_dir/unknown.txt", baidu.PolicyOverwrite, body, int64(body.Len())); err != nil {
		t.Fatalf("制造未知文件失败: %v", err)
	}

	// ── 2. dry-run：只识别不动作 ──
	dres, err := snd.ReconcileRemote(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dry-run: %+v", *dres)
	if dres.ArchivedFiles != 1 || dres.ArchivedDirs != 1 {
		t.Errorf("dry-run 应识别 1 文件 + 1 目录待归档，得到 %d + %d", dres.ArchivedFiles, dres.ArchivedDirs)
	}
	if _, err := bc.Meta(remote + "/unknown_dir/unknown.txt"); err != nil {
		t.Error("dry-run 不应改动远端")
	}

	// ── 3. 真执行：未知文件先归档，未知目录整棵归档 ──
	res, err := snd.ReconcileRemote(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live: %+v", *res)
	if res.ArchivedFiles != 1 || res.ArchivedDirs != 1 || res.Errors != 0 {
		t.Errorf("应归档 1 文件 + 1 目录且无错误: %+v", *res)
	}
	if res.Kept == 0 {
		t.Error("清单内文件应被计入 Kept")
	}
	if _, err := bc.Meta(remote + "/unknown_dir/unknown.txt"); !baidu.RemoteMissing(err) {
		t.Errorf("未知文件应已离开主树: %v", err)
	}
	if _, err := bc.Meta(remote + "/unknown_dir"); !baidu.RemoteMissing(err) {
		t.Errorf("未知目录应已离开主树: %v", err)
	}
	// 清单内不受影响
	if _, err := bc.Meta(remote + "/p4ren.bin"); err != nil {
		t.Errorf("清单内文件不应被动: %v", err)
	}

	// ── 4. 历史过期清理：造一个 2019 年的桶 ──
	oldBucket := remote + "/.history/2019-01-01"
	if err := bc.Mkdir(oldBucket); err != nil {
		t.Fatalf("造过期桶失败: %v", err)
	}
	ob := strings.NewReader("very old")
	if err := bc.UploadSingle(oldBucket+"/old.txt", baidu.PolicyOverwrite, ob, int64(ob.Len())); err != nil {
		t.Fatalf("过期桶内容失败: %v", err)
	}
	// dry-run 不删
	dremoved, err := snd.PruneHistory(ctx, 30, true)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(dremoved, "2019-01-01") {
		t.Errorf("dry-run 应列出过期桶: %v", dremoved)
	}
	if _, err := bc.Meta(oldBucket); err != nil {
		t.Error("prune dry-run 不应删除")
	}
	// 真删
	removed, err := snd.PruneHistory(ctx, 30, false)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(removed, "2019-01-01") {
		t.Errorf("应删除过期桶: %v", removed)
	}
	if _, err := bc.Meta(oldBucket); !baidu.RemoteMissing(err) {
		t.Errorf("过期桶应已删除: %v", err)
	}
	// 未过期桶保留（今天的归档桶来自 M1/M2）
	if removed2, err := snd.PruneHistory(ctx, 90, true); err == nil {
		for _, name := range removed2 {
			if name == historyDateBucket() {
				t.Error("当天桶不应被列为可清理")
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
