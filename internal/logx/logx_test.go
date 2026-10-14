package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	// maxMB=1 → 1MB 上限，keep=2
	lg, err := New(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	line := make([]byte, 64*1024) // 64KB/行
	for i := range line {
		line[i] = 'x'
	}
	line = append(line, '\n')
	for i := 0; i < 40; i++ { // 2.5MB 总量 → 应发生轮转
		if _, err := lg.Write(line); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i, err)
		}
	}
	lg.Close()

	// 轮转后：log.1 存在；log.2 存在（keep=2）；log.3 不存在（最旧被删）
	for _, f := range []string{path, path + ".1", path + ".2"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("期望存在 %s: %v", filepath.Base(f), err)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("log.3 不应存在（keep=2，最旧应被删除）")
	}
	// 主文件不超过上限（+一行余量）
	st, _ := os.Stat(path)
	if st.Size() > (1<<20)+64*1024 {
		t.Errorf("主日志 %d 字节，超过上限", st.Size())
	}
}

func TestNilLoggerSafe(t *testing.T) {
	lg, err := New("", 10, 7)
	if err != nil || lg != nil {
		t.Fatalf("空路径应返回 nil logger, got %v %v", lg, err)
	}
	// nil 上调用 Write/Close 不 panic
	var n *Logger
	if _, err := n.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	n.Close()
	_ = fmt.Sprintf("ok")
}
