//go:build windows

package main

import (
	"io"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

// ── 为什么是「转码」，不是「改代码页」 ──
//
// 旧实现是 init() 里 SetConsoleOutputCP(65001)。看着一行搞定，实际是**整个控制台
// 窗口级**的副作用，而且退出后不还原：
//
//	窗口 936 + 已开着的 pwsh（启动时把 [Console]::OutputEncoding 缓存成 gb2312）
//	  → 跑一次 bdsync → 控制台变 65001，pwsh 仍按 GBK 吐字节
//	  → 整窗乱码（go / git / 任何中文输出全废），关掉窗口才恢复
//
// 正确做法：**控制台一个字都不动**，只把本进程写到 stdout/stderr 的 UTF-8 字节
// 转成控制台当前代码页。窗口里其它程序完全不受影响，
// 重定向到文件时也仍然是 UTF-8（日志文件、看板 SSE 都依赖这一点）。

// 本文件只做一件事：把本进程写到 stdout/stderr 的 UTF-8 字节，按控制台当前代码页
// 转码输出。**不做任何字符替换/降级映射。**
//
// 早期版本带一张"符号 → 等价字符"的回退表（[OK]→sqrt、[X]→x、[!]→*...），现已删除。
// 删它的理由：那是在替源码里的用字不当打补丁 ---- 真正该做的是**源码里就别用
// GBK 装不下的符号**，让输出在任何 CJK 代码页下都原样可打。
//
// 剩下的 ReplaceUnsupported 是最后一道保险，不是"映射"：若输入里混进了
// 装不下的 rune（比如用户自建目录名带 emoji），它只把那个 rune 变成 '?'，
// 而 transform.Reader 一旦报错就**再也不吐出任何字节** ---- 后果是
// 「程序跑完了，但屏幕后半段一行输出都没有」。这层去掉不划算。

// init -- 给 stdout/stderr 套上转码层。
func init() {
	os.Stdout = transcodeConsole(os.Stdout)
	os.Stderr = transcodeConsole(os.Stderr)
}

// pipeWriters / pipeDones -- 转码管道的写端与「已收尾」信号，供 exitProcess 收尾。
var (
	pipeWriters []*os.File
	pipeDones   []chan struct{}
	pipeMu      sync.Mutex
)

// transcodeConsole -- 把 f 换成「写入即转码」的等价物。
//
// os.Stdout 是 *os.File，没法在外面包一层自定义 Write；唯一的办法是接一根
// os.Pipe 再由后台协程转码，于是 os.Stdout 变成管道的读端：
//
//	fmt.Printf → 管道 → 协程 transform → 真实控制台
//
// 好处是几百处 fmt.Printf / fmt.Fprintf(os.Stderr, ...) 一行都不用改。
// 代价：**退出前必须让转码协程收尾**，否则最后几行（通常正是错误信息）会随进程一起消失。
// 收尾点有两个，见 flushConsole 与 exitProcess。
func transcodeConsole(f *os.File) *os.File {
	t := consoleTransformer(f)
	if t == nil {
		return f // 非控制台 / 控制台已是 UTF-8 → 原样直通
	}
	r, w, err := os.Pipe()
	if err != nil {
		return f // 建不出管道就退回直通：宁可乱码，也别把输出全吞了
	}
	done := make(chan struct{})

	pipeMu.Lock()
	pipeWriters = append(pipeWriters, w)
	pipeDones = append(pipeDones, done)
	pipeMu.Unlock()

	go pumpConsole(f, r, t, done)
	return r
}

// pumpConsole -- 转码协程本体：读到管道 EOF 为止，把数据全部落到真实控制台。
//
// 刻意不加 bufio：管道本身同步，fmt 写完即到控制台，
// 不会因为「最后一笔还留在协程里」而丢掉错误信息。
// 代价是每笔输出多两次系统调用 ---- 对本程序可忽略（日志大头走文件与 SSE，不走这里）。
func pumpConsole(dst, src *os.File, t transform.Transformer, done chan struct{}) {
	defer close(done)
	_, _ = io.Copy(dst, transform.NewReader(src, t))
}

// flushConsole -- 收尾转码管道：关写端 → 等协程把剩余数据落到控制台 → 才算完。
//
// main 里必须 `defer flushConsole()`（正常 return 的路径，如 help）；
// os.Exit 的路径不跑 defer，由 exitProcess 代劳。两条路都得覆盖到。
func flushConsole() {
	pipeMu.Lock()
	ws, ds := pipeWriters, pipeDones
	pipeWriters, pipeDones = nil, nil
	pipeMu.Unlock()
	for _, w := range ws {
		_ = w.Close() // 触发读端 EOF，让协程吐完最后一段
	}
	for _, d := range ds {
		<-d // 等转码真正落到控制台，否则 os.Exit 会把它砍掉
	}
}

// exitProcess -- os.Exit 的替身：先 flushConsole 再退出。
//
// os.Exit 不跑 defer，所以 main 里的 os.Exit 必须换成它，
// 否则最后一行（通常正是错误信息）可能还留在转码协程里没落到屏幕上。
func exitProcess(code int) {
	flushConsole()
	os.Exit(code)
}

// consoleTransformer -- f 是真实控制台、且控制台代码页不是 UTF-8 时返回转码器；
// 其它情况返回 nil，调用方据此跳过套管道。
func consoleTransformer(f *os.File) transform.Transformer {
	if !isConsole(f) {
		return nil // 重定向到文件/管道：保持 UTF-8
	}
	return newConsoleTransformer(consoleOutputCP())
}

// newConsoleTransformer -- 给定控制台输出代码页造转码器。cp 不是 UTF-8 时返回 nil。
// 单独拎出来是为了能脱离控制台做单测。
func newConsoleTransformer(cp uintptr) transform.Transformer {
	enc := consoleEncoding(cp)
	if enc == nil {
		return nil // 65001：控制台本来就是 UTF-8，不动它
	}
	return transform.Chain(
		// 只保留最后一道保险：装不下的 rune 退化成 '?'，
		// 避免 transform.Reader 出错后整条 stdout 从此哑掉。
		encoding.ReplaceUnsupported(enc.NewEncoder()),
	)
}

// consoleEncoding -- 控制台输出代码页 → 编码器。nil = 无需转码。
func consoleEncoding(cp uintptr) encoding.Encoding {
	switch cp {
	case 936: // 简体中文 GBK（国内 Windows 默认）
		return simplifiedchinese.GBK
	case 950: // 繁体中文 Big5
		return traditionalchinese.Big5
	case 932: // 日文 Shift-JIS
		return japanese.ShiftJIS
	case 949: // 韩文 EUC-KR
		return korean.EUCKR
	}
	return nil // 65001 / 0（无控制台）/ 不认识 → 直通 UTF-8
}

func consoleOutputCP() uintptr {
	cp, _, _ := kernel32.NewProc("GetConsoleOutputCP").Call()
	return cp
}

// isConsole -- 句柄是不是真实控制台。GetConsoleMode 对文件/管道句柄会失败。
func isConsole(f *os.File) bool {
	var mode uint32
	r, _, _ := kernel32.NewProc("GetConsoleMode").Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}
