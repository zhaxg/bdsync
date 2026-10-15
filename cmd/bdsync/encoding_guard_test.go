//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// 源码里禁止出现的字符 —— 这些是 GBK（cp936，国内 Windows 默认）装不下的，
// 打进控制台就是问号或乱码。
//
// 背景：本程序在 Windows 上把 stdout/stderr 的 UTF-8 字节转成控制台当前代码页。
// 转码层**不做任何字符替换**（早期那套"符号换成等价字符"的回退表已删），所以
// **源码侧必须自己干净** —— 输出里出现装不下的符号，只会得到 '?'。
//
// 本测试把这个约束固化成红线：将来谁往输出里塞了个对勾/叉号之类的符号，
// 这里立刻红，而不是等到用户在 936 代码页的机器上看到一堆问号才发现。
func forbiddenConsoleRunes() map[rune]string {
	return map[rune]string{
		0x2713:  "U+2713 CHECK MARK",
		0x2714:  "U+2714 HEAVY CHECK MARK",
		0x2717:  "U+2717 BALLOT X",
		0x2718:  "U+2718 HEAVY BALLOT X",
		0x26A0:  "U+26A0 WARNING SIGN",
		0x26A1:  "U+26A1 WARNING SIGN VS",
		0x21BB:  "U+21BB CLOCKWISE OPEN CIRCLE ARROW",
		0x21BA:  "U+21BA ANTICLOCKWISE OPEN CIRCLE ARROW",
		0x2605:  "U+2605 BLACK STAR",
		0x2606:  "U+2606 WHITE STAR",
		0x2460:  "U+2460 CIRCLED DIGIT ONE",
		0x2461:  "U+2461 CIRCLED DIGIT TWO",
		0x2462:  "U+2462 CIRCLED DIGIT THREE",
		0x24EB:  "U+24EB NEGATIVE CIRCLED NUMBER ELEVEN",
		0x21D2:  "U+21D2 RIGHTWARDS DOUBLE ARROW",
		0x1F389: "U+1F389 PARTY POPPER",
		0x1F4A1: "U+1F4A1 LIGHT BULB",
		0x1F4A5: "U+1F4A5 COLLISION",
		0x1F512: "U+1F512 LOCK",
		0x1F513: "U+1F513 UNLOCK",
		0x1F6AB: "U+1F6AB PROHIBITED",
		0x1F7E2: "U+1F7E2 HEAVY GREEN CIRCLE",
		0xFE0F:  "U+FE0F VARIATION SELECTOR-16",
		0x200D:  "U+200D ZERO WIDTH JOINER",
		0x200B:  "U+200B ZERO WIDTH SPACE",
		0x00A0:  "U+00A0 NO-BREAK SPACE",
		0x221A:  "U+221A SQUARE ROOT",
		0x203B:  "U+203B REFERENCE MARK",
		0x2070:  "U+2070 SUPERSCRIPT ZERO",
		0x2074:  "U+2074 SUPERSCRIPT FOUR",
		0x2075:  "U+2075 SUPERSCRIPT FIVE",
		0x2076:  "U+2076 SUPERSCRIPT SIX",
		0x2077:  "U+2077 SUPERSCRIPT SEVEN",
		0x2078:  "U+2078 SUPERSCRIPT EIGHT",
		0x2079:  "U+2079 SUPERSCRIPT NINE",
	}
}

// 允许的例外：这些字符是**被处理的目标**（识别后消除），不是输出。
// netdiskname 的上标数字表就靠它们识别 COM1 / lpt3 这类网盘保留名变体。
var allowedInputOnly = map[rune]string{
	0x00B9: "netdiskname 上标数字归一化（被测输入）",
	0x00B2: "netdiskname 上标数字归一化（被测输入）",
	0x00B3: "netdiskname 上标数字归一化（被测输入）",
}

// 扫源码：返回 (文件, 行号, 字符, 说明) 列表
func scanSourceFiles(t *testing.T) []string {
	t.Helper()
	forbidden := forbiddenConsoleRunes()

	// 本包在 cmd/bdsync/，仓库根是 ../..
	root := filepath.Join("..", "..")
	var findings []string

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 读不到就跳过，不让检查本身成为故障源
		}
		if info.IsDir() {
			base := filepath.Base(p)
			if base == ".git" || base == "bin" || base == "docs" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(p)
		if ext != ".go" && ext != ".html" {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		// 非法 UTF-8 本身就是乱码，直接报出来
		if !utf8.Valid(raw) {
			findings = append(findings, filepath.ToSlash(p)+": 文件不是合法 UTF-8")
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		for i, line := range strings.Split(string(raw), "\n") {
			for _, r := range line {
				name, bad := forbidden[r]
				if !bad {
					continue
				}
				if _, ok := allowedInputOnly[r]; ok {
					continue
				}
				// 码点转义（\u2713）不算，这是显式且安全���写法
				if strings.Contains(line, `\u`+strings.ToLower(strings.TrimPrefix(name, "U+"))) {
					continue
				}
				findings = append(findings, fmt.Sprintf(
					"%s:%d  含 %s", filepath.ToSlash(rel), i+1, name))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码失败: %v", err)
	}
	return findings
}

func TestSourceHasNoConsoleUnsafeRunes(t *testing.T) {
	if findings := scanSourceFiles(t); len(findings) > 0 {
		t.Errorf("源码含 GBK 装不下的字符 %d 处 —— 会在 cp936 控制台显示为 '?'：\n  %s\n"+
			"请改用 ASCII/中文标点；确实需要保留的，先确认它在输出路径上。",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// 输出路径的实测：把控制台转码器真正跑一遍，确认本程序典型输出在 cp936 下无损。
func TestRealOutputSurvivesCP936(t *testing.T) {
	samples := []string{
		"[OK] usn 自检通过：写入的探测文件在 10s 内产生了 journal 事件",
		"[X] 配置读取失败: bdsync.yaml 不存在",
		"[!] usn 后端失效，降级 poll",
		"[~] desktop/a.txt -> desktop/b.txt（history 策略：归档+重传）",
		"[状态] -> QUIET（空闲；CPU 上限 5%，IDLE 优先级）",
		"[drain] 开始排空：1 文件 / 0 B（最老等待 15h16m53s） + 15 项待归档",
		"[心跳] QUIET：静默 30m0s - 队列 0 项（0 B）- RSS 46.5 MB - 后端 usn - 已运行 34m0s",
		"[对账-启动] 远端 6235 目录 / 43757 文件，与清单一致 43757 | 补建目录 0 | 错误 0",
		"[归] .cache/20260605/0648d4513237be80bceb75a1c97b34e6.dat -> .history/2026-10-15/",
		"扫描完成：新增 120 / 修改 3 / 删除 0 / 未变 43634（723 ms）",
	}
	for _, s := range samples {
		if !gbkEncodable(s) {
			t.Errorf("这条真实输出在 cp936 下装不下，会变问号：%q", s)
		}
	}
}

// gbkEncodable — 用与生产完全相同的转码器编码，并严格校验产物是合法 GBK。
func gbkEncodable(s string) bool {
	tr := newConsoleTransformer(936)
	if tr == nil {
		return false
	}
	out, _, err := transform.String(tr, s)
	if err != nil {
		return false
	}
	// 严格解码：有任何非法字节就报错
	dec := simplifiedchinese.GBK.NewDecoder()
	if _, err := dec.String(out); err != nil {
		return false
	}
	// 且往返无损（防止 ReplaceUnsupported 静默替换成 '?' 却没被发现）
	back, err := dec.String(out)
	if err != nil || back != s {
		return false
	}
	return true
}
