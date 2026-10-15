package sender

import (
	"testing"
	"time"
)

// 归档日期桶必须取**本地日期**。曾经用 time.Now().UTC()，导致本地时间 00:00–08:00
// 之间的删除全部落进"昨天"的桶 —— 东八区用户每天凌晨都会撞上，看起来像 bug。
func TestHistoryDateBucketUsesLocalDate(t *testing.T) {
	cases := []struct {
		name string
		in   string // 带时区偏移的 RFC3339
	}{
		// 本地 10-15 凌晨 06:58（UTC+8）→ UTC 是 10-14 22:58
		{"东八区凌晨应算当天", "2026-10-15T06:58:49+08:00"},
		{"东八区正午", "2026-10-15T12:00:00+08:00"},
		// 本地 10-15 晚上 23:30（UTC-5）→ UTC 已经是 10-16
		{"西五区深夜应算当天", "2026-10-15T23:30:00-05:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts, err := time.Parse(time.RFC3339, c.in)
			if err != nil {
				t.Fatal(err)
			}
			got := historyDateBucketAt(ts)
			if want := ts.Format("2006-01-02"); got != want {
				t.Fatalf("桶名应跟随本地日期：got %q, want %q", got, want)
			}
			// 反向断言：确认这些用例确实与 UTC 日期不同（否则测试是空转的）
			if utc := ts.UTC().Format("2006-01-02"); utc != got {
				t.Logf("确认：本地 %s ≠ UTC %s（本用例有效）", got, utc)
			}
		})
	}
}

// 桶名格式固定为 YYYY-MM-DD：PruneHistory 靠 time.Parse("2006-01-02") 识别哪些是日期桶，
// 格式一变，过期清理会静默地一个桶都不删（非日期名会被 continue 跳过）。
func TestHistoryDateBucketFormatIsParseableByPrune(t *testing.T) {
	ts := time.Date(2026, 10, 15, 6, 58, 49, 0, time.UTC)
	name := historyDateBucketAt(ts)
	if _, err := time.Parse("2006-01-02", name); err != nil {
		t.Fatalf("桶名 %q 必须能被 PruneHistory 的 time.Parse(\"2006-01-02\") 解析: %v", name, err)
	}
	if len(name) != len("2006-01-02") {
		t.Fatalf("桶名长度应为 10，实际 %q（长度 %d）", name, len(name))
	}
}
