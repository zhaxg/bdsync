// Package logx -- 按大小轮转的文件日志（design §4.8：日志大小上限 + 轮转，
// 保留 N 份 x 最大 MB，循环删除最旧）。
//
// 守护进程常驻数周，日志撑爆磁盘是 §4.8 明确列出的风险之一。
// 只追加、不采样、不做任何网络上报（低开销约束）。
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger -- 带轮转的文件写入器。path 为 "" 时是空操作（stdout 由调用方处理）。
type Logger struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	maxBytes int64
	keep     int
	written  int64
}

// New -- path 为空返回 nil（合法，调用方按无文件日志处理）。
func New(path string, maxMB, keep int) (*Logger, error) {
	if path == "" {
		return nil, nil
	}
	if maxMB <= 0 {
		maxMB = 10
	}
	if keep <= 0 {
		keep = 7
	}
	l := &Logger{path: path, maxBytes: int64(maxMB) << 20, keep: keep}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) open() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f = f
	l.written = st.Size()
	return nil
}

// Write -- 追加一行；超限时轮转（log → log.1 → ... → 删除最旧）。
// 注意：Write 面向整行写入（调用方一次 Write 一条日志），跨行大块写不做保护。
func (l *Logger) Write(p []byte) (int, error) {
	if l == nil {
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(p), nil
	}
	if l.written+int64(len(p)) > l.maxBytes {
		if err := l.rotate(); err != nil {
			return 0, err // 轮转失败：停写文件（进程照常运行，不因日志问题崩溃）
		}
	}
	n, err := l.f.Write(p)
	l.written += int64(n)
	return n, err
}

func (l *Logger) rotate() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	// log.(keep-1) → 丢弃；...；log → log.1
	oldest := fmt.Sprintf("%s.%d", l.path, l.keep)
	_ = os.Remove(oldest)
	for i := l.keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", l.path, i)
		to := fmt.Sprintf("%s.%d", l.path, i+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return l.open()
}

// Close -- 关闭文件。
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// Logf -- 统一的日志格式：带时间戳写文件；echo 为 true 时同步打到 stdout。
// log 为 nil 时只打 stdout（scan/send 等短命令的默认行为）。
func Logf(log *Logger, echo bool, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if log != nil {
		fmt.Fprintf(log, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
	}
	if echo || log == nil {
		fmt.Println(line)
	}
}
