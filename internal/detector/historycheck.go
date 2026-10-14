package detector

import (
	"fmt"
	"os"
	"path/filepath"
)

// ValidateNoLocalHistory — 本地根目录不允许存在与 mirror.history_dir 同名的目录。
// 远端历史目录与本地同名会被正常镜像进来，与归档写入冲突（design镜像 边界5），
// 启动/扫描时直接拦住，要求改配置。
func ValidateNoLocalHistory(root, historyDir string) error {
	if historyDir == "" {
		return nil
	}
	st, err := os.Stat(filepath.Join(root, historyDir))
	if err != nil || !st.IsDir() {
		return nil
	}
	return fmt.Errorf("✗ 本地存在同名目录 %s\n"+
		"  与远端历史目录（mirror.history_dir=%s）冲突：它会被镜像进主树并干扰归档。\n"+
		"  请重命名本地目录，或修改 mirror.history_dir 配置。", st.Name(), historyDir)
}
