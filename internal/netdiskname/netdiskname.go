// Package netdiskname — 网盘文件名校验规则（design §10）。
//
// 关键点：校验的是**网盘的规则**，不是本地文件系统的规则。
// Linux 本地允许的字符比 Windows 多，但目标是百度网盘，两边必须共用同一份规则表，
// 否则 Linux 扫到的文件会传到一半失败（design §10 跨平台一致性要求）。
//
// 拦截时机：扫描阶段（记 CONFLICT 状态），绝不等到上传时才发现（"传到一半失败，
// 前面几千文件白传"是 design §10 列举的头号坑）。
package netdiskname

import (
	"fmt"
	"strings"
)

// 非法字符（Windows 风格 + 反斜杠兜底；`/` 是路径分隔符，逐组件校验时不会出现）
const illegalChars = `:*?"<>|\`

// 保留设备名（Windows 保留名，网盘同样不接受）。
// 含 COM¹²³ 等上标数字变体（design §10：COM¹²³ 上标变体）。
var reservedBase = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// 上标数字 → 归一化（COM¹²³ 等变体，design §10）
var superscriptDigits = map[rune]rune{
	'¹': '1', '²': '2', '³': '3', '⁴': '4', '⁵': '5',
	'⁶': '6', '⁷': '7', '⁸': '8', '⁹': '9', '⁰': '0',
}

// Validate — 校验相对路径的每个组件。返回 nil = 网盘可接受。
func Validate(relPath string) error {
	if relPath == "" || relPath == "." {
		return nil
	}
	for _, comp := range strings.Split(relPath, "/") {
		if err := Component(comp); err != nil {
			return err
		}
	}
	return nil
}

// Component — 校验单个路径组件。
func Component(name string) error {
	if name == "" {
		return fmt.Errorf("空文件名组件")
	}
	if i := strings.IndexAny(name, illegalChars); i >= 0 {
		return fmt.Errorf("含网盘非法字符 %q（组件 %q）", string(name[i]), name)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("结尾不能是点或空格（组件 %q）——外部归档解压常见", name)
	}
	if strings.HasPrefix(name, " ") {
		return fmt.Errorf("开头不能是空格（组件 %q）", name)
	}

	// 保留设备名：取第一个点之前的基名，归一化上标数字后比对
	base := name
	if i := strings.IndexByte(name, '.'); i > 0 {
		base = name[:i]
	}
	if reservedBase[normalizeReserved(base)] {
		return fmt.Errorf("网盘保留设备名 %q（组件 %q）", base, name)
	}
	return nil
}

func normalizeReserved(s string) string {
	var b strings.Builder
	for _, r := range s {
		if n, ok := superscriptDigits[r]; ok {
			r = n
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// ErrPrefix — 扫描入库时写入 last_err 的前缀（ClaimBatch 用它过滤，见 store.go）。
const ErrPrefix = "非法文件名: "

// Marked — 判断 last_err 是否为本包产生的拦截记录。
func Marked(lastErr string) bool {
	return strings.HasPrefix(lastErr, ErrPrefix)
}
