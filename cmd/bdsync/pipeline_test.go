//go:build windows

package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// 本文件全部围绕一件事：**本进程写到 stdout/stderr 的东西，必须真的出现在控制台上。**
//
// 这里有两次真实事故：
//
//  1. transform.Chain 写成单元素 → 循环上界为 0 → 输出恒为空
//  2. transcodeConsole 返回管道的**读端**当 os.Stdout → fmt 往读端写必然失败，
//     而 fmt 丢弃 Write 的错误 → 「进程正常跑、日志文件正常、控制台一个字都没有」
//
// 两次的共同教训：**光断言「不报错」不够**，必须断言"内容真的落地了"。

// bannerSample -- 典型输出样本：启动横幅 + 状态行，覆盖中英混排、全角标点、数字。
const bannerSample = "bdsync 备份服务已启动\n" +
	"服务地址： http://127.0.0.1:8787/\n" +
	"停止服务： 关闭本窗口 / 按 Ctrl+C\n" +
	"[状态] QUIET（空闲；CPU 上限 5%，IDLE 优先级）\n"

// ── 根因验证 ──

// TestPipeWriteEndIsTheOnlyWritableOne -- 验证事故 2 的根因假设本身成立。
//
// 如果哪天管道语义变了、往读端写居然能成功，那后面的测试就失去了意义
// （它们是在"读端不可写"这个前提上建立的）。所以先把前提钉死。
func TestPipeWriteEndIsTheOnlyWritableOne(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	if _, err := w.Write([]byte("x")); err != nil {
		t.Errorf("写端应可写: %v", err)
	}
	if _, err := r.Write([]byte("x")); err == nil {
		t.Fatal("写管道读端居然成功了 —— 环境异常，后续测试的根因假设不成立")
	}
}

// ── 核心回归测试 ──

// TestInstallTranscoder_返回的必须是写端 -- 对应事故 2。
//
// transcodeConsole 里返回了 os.Pipe 的读端，而 os.Stdout 的用途是**写**。
// 于是一切 fmt.Print* 都写在一个只读句柄上，错误被静默丢弃。
//
// 为什么单测能覆盖：go test 的 stdout 本身是管道，isConsole() 为 false，
// transcodeConsole 走"直通"分支返回原文件，根本进不了建管道那段逻辑。
// 所以这里直接调 installTranscoder，并用另一根管道冒充"真实控制台"。
func TestInstallTranscoder_返回的必须是写端(t *testing.T) {
	sinkR, sinkW, err := os.Pipe() // sinkW 冒充控制台；sinkR 收集落地内容
	if err != nil {
		t.Fatal(err)
	}
	defer sinkR.Close()
	defer sinkW.Close()

	collected := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(sinkR)
		collected <- b
	}()

	out := installTranscoder(sinkW, newConsoleTransformer(936))

	// 这一行就是事故现场：拿到读端时它会直接报错。
	if _, werr := io.WriteString(out, bannerSample); werr != nil {
		flushConsole()
		t.Fatalf("返回的文件不可写 —— 拿到的是管道**读端**而不是写端: %v\n"+
			"这正是「控制台一片空白」的根因：fmt 丢弃 Write 的错误，"+
			"于是失败完全无声无息", werr)
	}

	// 收尾顺序不能错：先等转码协程把数据落到 sinkW，再关 sinkW。
	// 反过来会在协程还在写的时候把它关掉，数据静默丢失。
	// flushConsole 正是进程退出时走的同一条路径，顺带一起覆盖。
	flushConsole() // 关闭管道写端（触发 EOF）并等转码协程收尾
	sinkW.Close()  // 转码已结束，此刻关闭落地端是安全的

	select {
	case raw := <-collected:
		if len(raw) == 0 {
			t.Fatal("转码协程一个字节都没落地")
		}
		got, derr := simplifiedchinese.GBK.NewDecoder().String(string(raw))
		if derr != nil {
			t.Fatalf("落地内容不是合法 GBK: %v\n原始字节 %x", derr, raw)
		}
		if got != bannerSample {
			t.Errorf("落地内容与写入的不一致\n 写 =%q\n 落 =%q", bannerSample, got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内转码协程未收尾")
	}
}

// TestPrintBannerReachesConsole -- 端到端：printBanner 的输出必须真的落到"控制台"。
//
// 走的是真实链路：printBanner → fmt.Println → os.Stdout → 管道写端
// → transform → 落地端。上面那个测试验证的是管道两端没错接，
// 这个验证的是**整条链路接上了**，两者缺一不可。
func TestPrintBannerReachesConsole(t *testing.T) {
	sinkR, sinkW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer sinkR.Close()
	defer sinkW.Close()

	collected := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(sinkR)
		collected <- b
	}()

	old := os.Stdout
	os.Stdout = installTranscoder(sinkW, newConsoleTransformer(936))
	printBanner("http://127.0.0.1:8787/", true, nil)
	os.Stdout = old // 先还原，别让后面的测试拿到管道

	flushConsole()
	sinkW.Close()

	var raw []byte
	select {
	case raw = <-collected:
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内转码协程未收尾")
	}

	got, derr := simplifiedchinese.GBK.NewDecoder().String(string(raw))
	if derr != nil {
		t.Fatalf("落地内容不是合法 GBK: %v\n原始字节 %x", derr, raw)
	}
	for _, want := range []string{
		"bdsync 备份服务已启动",
		"http://127.0.0.1:8787/",
		"Ctrl+C",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("启动横幅缺少 %q；实际落地内容：\n%s", want, got)
		}
	}
}

// ── 转码器本身 ──

// TestTranscoderMustNotSwallowOutput -- 回归测试：转码器**必须真的输出内容**。
//
// 对应事故 1。transform.Chain 写成单元素后（删掉符号映射层后只剩
// ReplaceUnsupported），控制台整片空白。根因是 x/text 的 Chain 实现：
//
//	c := &chain{link: make([]link, len(t)+1)}   // 传 1 个 → link 长度 2
//	for low, i, high := ..., ..., len(c.link)-2  // → high = 0
//	    ; low <= i && i <= high; {                // 循环条件恒为 false
//
// 循环一次都不跑，没有任何字节被写进 dst。
//
// 旧版这里只断言 len(out) > 0 却仍然通过 —— 断言写得太松。
// 现在对**每个支持的代码页**都断言"消费了全部输入"，单元素 Chain 下 n 恒为 0。
func TestTranscoderMustNotSwallowOutput(t *testing.T) {
	for _, cp := range []uintptr{936, 950, 932, 949} {
		tr := newConsoleTransformer(cp)
		if tr == nil {
			t.Errorf("cp=%d 应有转码器", cp)
			continue
		}
		out, n, err := transform.String(tr, bannerSample)
		if err != nil {
			t.Errorf("cp=%d 转码报错: %v", cp, err)
			continue
		}
		if n != len(bannerSample) {
			t.Errorf("cp=%d 消费字节数不符: 消费 %d，期望 %d\n"+
				"（单元素 transform.Chain 会恒为 0 —— 输出整条为空，控制台一片空白）",
				cp, n, len(bannerSample))
		}
		if len(out) == 0 {
			t.Errorf("cp=%d 转码后输出为空（整条 stdout 哑掉）", cp)
			continue
		}
		// 产物必须是该代码页的合法字节序列，不能是半截的多字节字符
		if _, derr := consoleEncoding(cp).NewDecoder().String(out); derr != nil {
			t.Errorf("cp=%d 产物不是合法字节序列: %v", cp, derr)
		}
	}
}

// TestTranscoder_CP936必须逐字无损 -- cp936 是本程序的目标平台（国内 Windows 默认），
// 简体中文明明都在 GBK 里，所以输出必须**一字不差**。
//
// 曾经把 936/950/932/949 一视同仁地要求往返一致，那是错的：
// Big5 / Shift-JIS / EUC-KR 结构上就装不下简体字，在这些代码页下看到 '?' 是
// 先天限制而非缺陷。把两种断言分开，"GBK 下掉了字"这种真问题才会第一时间暴露。
func TestTranscoder_CP936必须逐字无损(t *testing.T) {
	for _, in := range []string{
		bannerSample,
		"[OK] usn 自检通过：写入的探测文件在 10s 内产生了 journal 事件",
		"[X] 读取配置失败: open C:\\nonexistent\\zz.yaml",
		"[~] desktop/a.txt -> desktop/b.txt（history 策略：归档+重传）",
		"扫描完成：新增 120 / 修改 3 / 删除 0 / 未变 43634（723 ms）",
	} {
		tr := newConsoleTransformer(936)
		out, _, err := transform.String(tr, in)
		if err != nil {
			t.Errorf("GBK 编码报错（输出里混进了 GBK 装不下的字符）%q: %v", in, err)
			continue
		}
		back, derr := consoleEncoding(936).NewDecoder().String(out)
		if derr != nil {
			t.Errorf("GBK 产物非法 %q: %v", in, derr)
			continue
		}
		if back != in {
			t.Errorf("GBK 往返有损（不该有任何替换）\n in =%q\n got=%q", in, back)
		}
	}
}
