package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExample(t *testing.T) {
	cfg, err := Load("bdsync.example.yaml") // 示例配置与包同目录
	if err != nil {
		t.Fatalf("示例配置应能解析: %v", err)
	}
	if cfg.Detector.Mode != "auto" || cfg.Detector.ReconcileInterval.Seconds() != 86400 {
		t.Errorf("detector 解析错误: %+v", cfg.Detector)
	}
	if cfg.Sender.Concurrency != 12 || cfg.Sender.FlushBytes.Int64() != 2<<30 {
		t.Errorf("sender 解析错误: %+v", cfg.Sender)
	}
	if cfg.Policy.PropagateDelete {
		t.Error("propagate_delete 应为 false")
	}
	if cfg.Service.LogMaxMB != 10 || cfg.Service.LogKeep != 7 {
		t.Errorf("service 解析错误: %+v", cfg.Service)
	}
}

func TestValidateRejectsPropagation(t *testing.T) {
	cfg := Default()
	cfg.Policy.PropagateDelete = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("propagate_delete=true 必须被拒绝（design §9.2 备份语义）")
	}
	cfg.Policy.PropagateDelete = false
	cfg.Detector.Mode = "inotify"
	if err := cfg.Validate(); err == nil {
		t.Fatal("未知 detector.mode 必须被拒绝")
	}
}

func TestDefaults(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置必须合法: %v", err)
	}
	if cfg.Sender.SettleDuration.Seconds() != 10 || cfg.Quota.IdleThreshold.Seconds() != 300 {
		t.Errorf("默认值偏离 design §14: %+v %+v", cfg.Sender, cfg.Quota)
	}
	// 就地部署：索引库与二进制同目录，日志在 logs/ 下
	if cfg.Service.DBPath != "index.db" {
		t.Errorf("db_path 默认应为 index.db（与二进制同目录），得到 %q", cfg.Service.DBPath)
	}
	if cfg.Service.LogFile != "logs/bdsync.log" {
		t.Errorf("log_file 默认应为 logs/bdsync.log，得到 %q", cfg.Service.LogFile)
	}
}

// TestPairRequired — 备份对必须显式声明：缺任一项都拒绝运行（不猜、不兜底）。
func TestPairRequired(t *testing.T) {
	cfg := Default()
	if err := cfg.ValidatePair(); err == nil {
		t.Fatal("空 pair 必须被拒绝")
	}
	cfg.Pair.LocalRoot = `D:\YourData`
	if err := cfg.ValidatePair(); err == nil {
		t.Fatal("缺 pair.remote 必须被拒绝")
	}
	cfg.Pair.Remote = "bdsync" // 缺 / 前缀
	if err := cfg.ValidatePair(); err == nil {
		t.Fatal("pair.remote 缺 / 前缀必须被拒绝")
	}
	cfg.Pair.Remote = "/bdsync"
	if err := cfg.ValidatePair(); err != nil {
		t.Fatalf("完整 pair 必须通过: %v", err)
	}
}

// TestRelativePathsAnchoredToConfigDir — 相对路径以配置文件目录为基准
// （便携性保证：从任何目录调用，结果一致）。
func TestRelativePathsAnchoredToConfigDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bdsync.yaml")
	body := "pair:\n  local_root: \"data\"\n  remote: \"/bdsync\"\n" +
		"service:\n  db_path: \"index.db\"\n  log_file: \"logs/bdsync.log\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"db":   filepath.Join(dir, "index.db"),
		"log":  filepath.Join(dir, "logs", "bdsync.log"),
		"root": filepath.Join(dir, "data"),
	}
	if cfg.Service.DBPath != want["db"] {
		t.Errorf("db_path 应锚定到配置目录: %q", cfg.Service.DBPath)
	}
	if cfg.Service.LogFile != want["log"] {
		t.Errorf("log_file 应锚定到配置目录: %q", cfg.Service.LogFile)
	}
	if cfg.Pair.LocalRoot != want["root"] {
		t.Errorf("pair.local_root 应锚定到配置目录: %q", cfg.Pair.LocalRoot)
	}
}

func TestParseByteSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{{"2GB", 2 << 30}, {"512MB", 512 << 20}, {"1048576", 1048576}, {"1.5GB", 1610612736}} {
		got, err := ParseByteSize(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseByteSize(%q) = %d, %v；期望 %d", c.in, got, err, c.want)
		}
	}
}
