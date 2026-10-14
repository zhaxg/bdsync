package detector

import (
	"path"
	"strings"
)

// Matcher — 排除规则（design §14 exclude）。
//
// 规则语法：
//   - 单组件 glob（如 `*.tmp`、`~$*`、`node_modules`）：命中路径的**任一组件**
//     即排除，深度不限（`a/b/node_modules/c` 会被 `node_modules` 排除）
//   - 带 `/` 的模式（如 `src/gen/*.o`）：整路径语义，`*` 不跨 `/`
//   - 以 `/` 结尾仅为可读性（与不带 `/` 等价）
//   - 不支持 `**`（需要时再加）
//
// 大小写：Windows 下不敏感，其余平台敏感。
type Matcher struct {
	patterns []string
	fold     bool
}

// NewMatcher — 编译排除规则（空规则返回可安全调用的空 Matcher）。
func NewMatcher(rules []string, foldCase bool) *Matcher {
	m := &Matcher{fold: foldCase}
	for _, r := range rules {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		r = strings.TrimSuffix(r, "/")
		if r == "" {
			continue
		}
		m.patterns = append(m.patterns, r)
	}
	return m
}

// Match — rel（'/' 分隔）是否被排除。nil Matcher 永远返回 false。
func (m *Matcher) Match(rel string) bool {
	if m == nil || len(m.patterns) == 0 || rel == "" || rel == "." {
		return false
	}
	key := rel
	if m.fold {
		key = strings.ToLower(rel)
	}

	for _, raw := range m.patterns {
		pat := raw
		if m.fold {
			pat = strings.ToLower(pat)
		}
		// 整路径匹配
		if ok, _ := path.Match(pat, key); ok {
			return true
		}
		// 单组件模式：逐组件匹配（命中任一组件 = 排除该文件/子树）
		if strings.ContainsRune(pat, '/') {
			continue // 带路径的模式只有整路径语义
		}
		rest := key
		for {
			comp := rest
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				comp = rest[:i]
				rest = rest[i+1:]
			} else {
				rest = ""
			}
			if ok, _ := path.Match(pat, comp); ok {
				return true
			}
			if rest == "" {
				break
			}
		}
	}
	return false
}
