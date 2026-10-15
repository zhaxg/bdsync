//go:build windows

package main

import (
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// encodeForTest -- 用给定代码页的转码器处理一段 UTF-8 文本。
func encodeForTest(t *testing.T, cp uintptr, s string) (string, error) {
	t.Helper()
	tr := newConsoleTransformer(cp)
	if tr == nil {
		t.Fatalf("cp=%d 不该返回 nil 转码器", cp)
	}
	out, _, err := transform.String(tr, s)
	return out, err
}

// decodeGBK -- 把转码产物解回 UTF-8，验证「控制台上真正显示的字符」。
func decodeGBK(t *testing.T, s string) string {
	t.Helper()
	back, _, err := transform.String(simplifiedchinese.GBK.NewDecoder(), s)
	if err != nil {
		t.Fatalf("产物不是合法 GBK（%v）: %q", err, s)
	}
	return back
}

// TestConsoleTransformer_CP936RoundTrip ---- GBK 往返必须**逐字一致**。
//
// 早期版本这里期望的是"符号被换成等价字符"（/x/）。现在不做任何替换，
// 所以期望值就等于输入：源码里只用 GBK 装得下的字符，往返自然无损。
// 这同时是一道防线 —— 有人往输出里塞了个 GBK 装不下的符号，这里会立刻红。
func TestConsoleTransformer_CP936RoundTrip(t *testing.T) {
	cases := []string{
		"[OK] usn 自检通过：写入的探测文件在 10s 内产生了 journal 事件",
		"[X] 配置读取失败: bdsync.yaml 不存在",
		"[!] 扫描未完整（部分结果，未做删除判定）",
		"[~] a.txt -> b.txt（history 策略：归档+重传）",
		"[状态] -> QUIET（空闲；CPU 上限 5%，IDLE 优先级）",
		"[drain] 开始排空：1234 文件 / 2.0 GB（最老等待 3h12m）",
	}
	for _, in := range cases {
		enc, err := encodeForTest(t, 936, in)
		if err != nil {
			t.Errorf("GBK 编码报错（说明输出里混进了 GBK 装不下的字符）%q: %v", in, err)
			continue
		}
		if got := decodeGBK(t, enc); got != in {
			t.Errorf("CP936 往返不符（不该有任何替换）\n in =%q\n got=%q", in, got)
		}
	}
}

// TestConsoleTransformer_装不下也不能哑掉 ---- 回归测试：
//
// 转码器遇到装不下的 rune 会返回错误，而 transform.Reader 出错后就再也不吐字节，
// 表现为「程序跑完了但屏幕后半段一行都没有」。这里逐个字符扫全 BMP，
// 确认任何输入都不会把流打断。
func TestConsoleTransformer_装不下也不能哑掉(t *testing.T) {
	tr := newConsoleTransformer(936)
	if tr == nil {
		t.Fatal("cp=936 应返回转码器")
	}
	// 非法 UTF-8 + emoji + 生僻字 + 未映射符号，混在一段里
	var sb strings.Builder
	sb.WriteString("正常中文 ASCII ok\n")
	sb.WriteString("\U0001f389\U0001d11e\u26ff\u2b1b\ufe0f") // 装不下的各种
	sb.WriteString("非法字节后仍要继续输出\n")

	src := sb.String()
	enc, _, err := transform.String(tr, src)
	if err != nil {
		t.Fatalf("转码报错（流会就此哑掉）: %v", err)
	}
	// 产物必须整体是合法 GBK：非法序列会让控制台显示成「?」或直接吞字。
	// 只打印 hex，避免失败信息本身再被控制台代码页搅成一团乱码。
	if !gbkValid(t, enc) {
		t.Errorf("产物不是合法 GBK: %s", hex.EncodeToString([]byte(enc)))
	}
	if !strings.Contains(decodeGBK(t, enc), "非法字节后仍要继续输出") {
		t.Errorf("后半段输出丢了: %s", hex.EncodeToString([]byte(enc)))
	}
	// U+FFFD 绝不能原样漏进控制台：装不下的 rune 必须换成可见字符。
	if strings.Contains(enc, "�") {
		t.Errorf("产物里漏出了 U+FFFD: %s", hex.EncodeToString([]byte(enc)))
	}
}

// gbkValid -- 严格校验字节串是否为合法 GBK 序列（单字节 ASCII / 双字节首尾字节）。
func gbkValid(t *testing.T, b string) bool {
	t.Helper()
	s := []byte(b)
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c < 0x80:
			i++
		case c >= 0x81 && c <= 0xfe:
			if i+1 >= len(s) {
				return false
			}
			n := s[i+1]
			if n < 0x40 || n == 0x7f || n > 0xfe {
				return false
			}
			i += 2
		default:
			return false // 0x80/0xff 不是合法的 GBK 序列首字节
		}
	}
	return true
}

// TestConsoleTransformer_不碰控制台 ---- 整条铁律：
// 转码只发生在字节层，绝不允许再调 SetConsoleOutputCP。
// 那个 API 改的是整个控制台窗口的状态、且退出后不还原，
// 会让同窗口里已经开着的 PowerShell（缓存了 GBK）往 UTF-8 控制台吐 GBK 字节 → 全窗乱码。
func TestConsoleTransformer_不碰控制台(t *testing.T) {
	before := consoleOutputCP()
	if _, err := encodeForTest(t, 936, "中文 [OK]"); err != nil {
		t.Fatal(err)
	}
	if _, err := encodeForTest(t, 950, "中文 [OK]"); err != nil {
		t.Fatal(err)
	}
	if after := consoleOutputCP(); after != before {
		t.Errorf("转码路径改了控制台代码页：%d → %d", before, after)
	}
}

// TestConsoleEncoding_UTF8直通 ---- 控制台本来就是 UTF-8 时不该套转码层。
func TestConsoleEncoding_UTF8直通(t *testing.T) {
	for _, cp := range []uintptr{0, 65001, 437} {
		if got := consoleEncoding(cp); got != nil {
			t.Errorf("cp=%d 应直通 UTF-8，实际返回了转码器", cp)
		}
		if got := newConsoleTransformer(cp); got != nil {
			t.Errorf("cp=%d 应返回 nil 转码器", cp)
		}
	}
	for _, cp := range []uintptr{936, 950, 932, 949} {
		if got := consoleEncoding(cp); got == nil {
			t.Errorf("cp=%d 应有对应编码器", cp)
		}
	}
}

// TestConsoleTransformer_不做字符替换 ---- 转码层只转码、不替换。
//
// 曾经的实现带一张"符号→等价字符"回退表（→ 之类），现已删除。删除后必须保证：
//   - GBK 装得下的字符**原样通过**（不做任何等价替换）
//   - GBK 装不下的字符退化成 '?'，且**只影响那一个字符**，后续输出照常
//
// 源码侧已改为只用 GBK 安全字符，这里是防止将来有人再加回映射层。
func TestConsoleTransformer_不做字符替换(t *testing.T) {
	// GBK 能表示的：必须逐字节一致，不许被"优化"成别的字符
	for _, s := range []string{
		"[OK] usn 自检通过",
		"[X] 配置读取失败",
		"-> QUIET（空闲）",
		"约 30 分钟", // 中文
	} {
		enc, err := encodeForTest(t, 936, s)
		if err != nil {
			t.Fatalf("GBK 应能编码 %q: %v", s, err)
		}
		if got := decodeGBK(t, enc); got != s {
			t.Errorf("GBK 往返被改动了: 期望 %q，得到 %q", s, got)
		}
	}

	// GBK 装不下的：退化成 '?'，但不能把后续输出一起吞掉
	enc, err := encodeForTest(t, 936, "前"+string(rune(0x1F389))+"后")
	if err != nil {
		t.Fatalf("含装不下字符时应仍能编码（退化为 '?'）: %v", err)
	}
	if got := decodeGBK(t, enc); !strings.Contains(got, "前") || !strings.Contains(got, "后") {
		t.Errorf("退化的字符不应吞掉前后文: %q", got)
	}
}

// TestPumpConsole_退出前必须冲干净 ---- 回归测试：
//
// os.Stdout 变成管道读端后，最后一笔 fmt 写完只保证「进了管道」，
// 不保证「已经落到控制台」。main 正常 return 走 defer flushConsole，
// os.Exit 走 exitProcess ---- 两条路任一漏掉，进程就带着最后几行（通常正是错误信息）消失。
//
// 这里模拟「写完立刻收尾」，确认收尾返回时数据确实全部到达终点。
func TestPumpConsole_退出前必须冲干净(t *testing.T) {
	const lastLine = "[X] 配置读取失败：这一行必须出现在控制台上\n"

	srcR, srcW, err := os.Pipe() // 进程内当控制台：读端
	if err != nil {
		t.Fatal(err)
	}
	dstR, dstW, err := os.Pipe() // 进程内当真实控制台：写端
	if err != nil {
		t.Fatal(err)
	}
	defer dstR.Close()

	done := make(chan struct{})
	go pumpConsole(dstW, srcR, newConsoleTransformer(936), done)

	if _, err := srcW.WriteString("前面几行都正常\n" + lastLine); err != nil {
		t.Fatal(err)
	}
	// 模拟 flushConsole：关写端 → 等 done
	_ = srcW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("收尾后转码协程仍未结束，会丢输出")
	}
	_ = dstW.Close()

	got, err := io.ReadAll(dstR)
	if err != nil {
		t.Fatal(err)
	}
	// 末尾这行是重点：[X] 会被降级成 x（GBK 装不下），正文必须一字不差。
	if !strings.Contains(decodeGBK(t, string(got)), "配置读取失败：这一行必须出现在控制台上") {
		t.Errorf("最后一行丢了: %s", hex.EncodeToString(got))
	}
}

// TestFlushConsole_幂等 ---- main 里 defer 一次、exitProcess 里一次，
// 两条路径不该重复等同一个已关闭的 done（会永久阻塞）。
func TestFlushConsole_幂等(t *testing.T) {
	// 隔离全局：init() 已为本测试进程的真实 stdout/stderr 装过管道，
	// 直接调 flushConsole 会去等那些还没关闭的管道。
	pipeMu.Lock()
	savedW, savedD := pipeWriters, pipeDones
	done := make(chan struct{})
	close(done)
	pipeWriters, pipeDones = nil, []chan struct{}{done}
	pipeMu.Unlock()
	defer func() {
		pipeMu.Lock()
		pipeWriters, pipeDones = savedW, savedD
		pipeMu.Unlock()
	}()

	finished := make(chan struct{})
	go func() {
		flushConsole()
		flushConsole() // 第二次必须立刻返回，不能卡在 <-done
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("flushConsole 第二次调用卡住了")
	}
}
