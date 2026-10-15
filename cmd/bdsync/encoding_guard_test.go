//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件守的是**源码本身的编码完整性**，不是"输出要转成什么编码"。
//
// 背景（一次真实事故）：程序原先在 Windows 上把 stdout/stderr 按控制台代码页
// 转码，于是给源码立了一条红线——不许出现 GBK 装不下的符号（✓ ✗ ⚠ ↻ ★）。
// 后来查明那条红线的前提就是错的：
//
//	Go 的 os.File.Write 在控制台句柄上走 WriteConsoleW，
//	它把字节按 UTF-8 解码成 UTF-16，**不按控制台代码页解释**。
//
// 所以"先转 GBK 再写"必然被当 UTF-8 解码失败 -> 每字节变 U+FFFD -> 满屏菱形。
// 转码层已整个删除，os.Stdout 直接收 UTF-8，中文正常。
//
// 于是约束反过来了：**源码必须是干净的 UTF-8**，因为 Go 是把它当 UTF-8 解码的。
// 源码里混进 GBK 字节或已经损坏的文本，用户看到的就是真·乱码，而且无从排查。
func TestSourceIsValidUTF8(t *testing.T) {
	for _, p := range sourceFiles(t) {
		rel := relToRepo(t, p)
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if !utf8.Valid(raw) {
			t.Errorf("%s: 不是合法 UTF-8（多半是 GBK 字节被当成 UTF-8 提交进来了）\n"+
				"  程序按 UTF-8 解码它，控制台上会是货真价实的乱码", rel)
		}
	}
}

// TestSourceHasNoReplacementChar -- 源码里不该已经存在 U+FFFD（替换字符）。
//
// U+FFFD 出现在源码里只有一个可能：某个环节（编辑器、批量替换、粘贴）
// 已经把文本解码错了，而后续所有环节都会原样传递它 —— 编译照过、测试照过，
// 只是用户看到满屏乱码。这类问题最难靠肉眼 review 发现，所以固化成红线。
//
// 注意：本文件自己也必须过关，所以下面用反斜杠-u 转义，而不是直接写字面量 ——
// 否则这个测试会永远红，而且红得毫无意义。
func TestSourceHasNoReplacementChar(t *testing.T) {
	const bad = "\uFFFD"
	for _, p := range sourceFiles(t) {
		rel := relToRepo(t, p)
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if n := strings.Count(line, bad); n > 0 {
				t.Errorf("%s:%d 含 %d 个 U+FFFD（替换字符）—— 文本在某一步已被解码损坏\n  %s",
					rel, i+1, n, strings.TrimSpace(line))
			}
		}
	}
}

// TestSourceHasNoBOM -- UTF-8 BOM 会混进 Go 源码的第一个标识符前面，
// 表现为"文件开头那个诡异的字符"，必要时才可见。同样固化成红线。
func TestSourceHasNoBOM(t *testing.T) {
	var bom = []byte{0xEF, 0xBB, 0xBF}
	for _, p := range sourceFiles(t) {
		rel := relToRepo(t, p)
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) < 3 {
			continue
		}
		if raw[0] == bom[0] && raw[1] == bom[1] && raw[2] == bom[2] {
			t.Errorf("%s: 文件开头有 UTF-8 BOM，会粘到第一个标识符上", rel)
		}
	}
}

// sourceFiles -- 扫仓库里所有会影响用户可见文本的源文件。
func sourceFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 读不到就跳过，不让检查本身成为故障源
		}
		if info.IsDir() {
			switch filepath.Base(p) {
			case ".git", "bin", "node_modules", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(p) {
		case ".go", ".html", ".yaml", ".yml":
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码失败: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("一个源文件都没扫到 —— 检查本身失效了")
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// 本包在 cmd/bdsync/，仓库根是上两级
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("取仓库根失败: %v", err)
	}
	return abs
}

func relToRepo(t *testing.T, p string) string {
	t.Helper()
	rel, err := filepath.Rel(repoRoot(t), p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

var _ = fmt.Sprintf // 保留 fmt 依赖以便将来扩展报错信息
