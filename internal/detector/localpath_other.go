//go:build !windows && !linux

package detector

import (
	"fmt"
	"os"
	"path/filepath"
)

// ValidateLocalRoot — 非 Windows/Linux 平台仅做基础校验（design.md §4.10）。
func ValidateLocalRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("无法解析路径 %q: %w", root, err)
	}
	st, err := os.Stat(abs) // 只读操作
	if err != nil {
		return fmt.Errorf("✗ 根目录不可访问：%s\n  %v", abs, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("✗ 根目录不是目录：%s", abs)
	}
	return nil
}

// dirFileRef — 非 Windows 平台不支持（poll 不依赖它）。
func dirFileRef(string) (uint64, bool) { return 0, false }

// IsNTFS — 非 Windows 恒 false。
func IsNTFS(string) bool { return false }
