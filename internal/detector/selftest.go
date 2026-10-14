package detector

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// SelfTest 相关的通用小工具。
// 注意：探测文件会写入 root —— 只允许显式 `syncer selftest` 命令触发，
// 绝不随 scan/daemon 自动执行（对备份目录的只读约束，design 之外的工程约定）。

func writeProbeFile(p string) error { return os.WriteFile(p, []byte("probe"), 0o600) }

func removeFile(p string) { _ = os.Remove(p) }

// SelfTest — poll 的自检：写探测文件 → 走一遍真实扫描路径确认能发现 → 删除。
// poll 是主动扫描，唯一可能失效的是环境（权限/句柄/目录被换），故以实际走
// 扫描路径为准（design 硬规矩①的 poll 语义）。
func (p *Poll) SelfTest(ctx context.Context, root string) error {
	walkRoot, err := normalizeWalkRoot(root)
	if err != nil {
		return err
	}
	probeRel := fmt.Sprintf(".syncer-probe-%d.tmp", time.Now().UnixNano())
	probeAbs := filepath.Join(walkRoot, probeRel)
	if err := writeProbeFile(probeAbs); err != nil {
		return fmt.Errorf("写探测文件失败（root 需可写）: %w", err)
	}
	defer removeFile(probeAbs)

	found := false
	walkErr := filepath.WalkDir(walkRoot, func(path string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(walkRoot, path)
		if rerr == nil && filepath.ToSlash(rel) == probeRel {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("扫描失败: %w", walkErr)
	}
	if !found {
		return fmt.Errorf("探测文件写入后未被扫描发现（后端失效）")
	}
	return nil
}
