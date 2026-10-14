//go:build linux

package detector

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 网络/集群文件系统黑名单（design.md §1.1）：
// 这些挂载上文件变动无法可靠检测（stat 慢、句柄失效、事件机制受限），
// 支持它们必须引入有损后端 + 强制对账，当前范围明确排除。
var remoteFSTypes = map[string]bool{
	"nfs":            true,
	"nfs4":           true,
	"cifs":           true,
	"smb3":           true,
	"9p":             true,
	"fuse.sshfs":     true,
	"fuse.rclone":    true,
	"ceph":           true,
	"glusterfs":      true,
	"afs":            true,
	"fuse.gocryptfs": true, // 本地但属 FUSE 转发，stat 语义不稳
}

type mountInfo struct {
	mountpoint string
	fstype     string
}

// ValidateLocalRoot 启动时强制校验（design.md §1.1）：
// 解析 /proc/self/mountinfo 取最长前缀匹配，拒绝网络文件系统挂载。
func ValidateLocalRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("无法解析路径 %q: %w", root, err)
	}
	abs = filepath.Clean(abs)

	st, err := os.Stat(abs) // 只读操作
	if err != nil {
		return fmt.Errorf("✗ 根目录不可访问：%s\n  %v", abs, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("✗ 根目录不是目录：%s", abs)
	}

	mounts, err := loadMounts()
	if err != nil {
		// 读不到 mountinfo（容器异常等）：不阻塞，仅告警到 stderr
		fmt.Fprintf(os.Stderr, "警告：无法读取 /proc/self/mountinfo，跳过网络盘校验：%v\n", err)
		return nil
	}
	// 最长前缀匹配
	var best *mountInfo
	for i := range mounts {
		m := &mounts[i]
		if !pathWithin(abs, m.mountpoint) {
			continue
		}
		if best == nil || len(m.mountpoint) > len(best.mountpoint) {
			best = m
		}
	}
	if best != nil && remoteFSTypes[best.fstype] {
		return fmt.Errorf("✗ 不支持网络/远程文件系统挂载：%s\n"+
			"  %s 位于 %s（fstype=%s）。本工具仅支持本地文件系统，\n"+
			"  原因：远程挂载上文件变动无法可靠检测，可能导致备份静默遗漏文件。\n"+
			"  如需备份 NAS 数据，请先将数据同步到本地磁盘再使用本工具。",
			abs, abs, best.mountpoint, best.fstype)
	}
	return nil
}

// pathWithin — p 是否位于 dir 之下（含 dir 本身），按路径分量判断。
func pathWithin(p, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// loadMounts 解析 /proc/self/mountinfo：
//
//	36 25 0:31 / /path rw,relatime - ext4 /dev/sda1 rw
//
// 字段：mountpoint = 第 5 列；"-" 之后第 1 列 = fstype。八进制转义（\040 空格）需还原。
func loadMounts() ([]mountInfo, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []mountInfo
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		sep := -1
		for i, fd := range fields {
			if fd == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}
		mp := unescapeMountPath(fields[4])
		fstype := fields[sep+1]
		out = append(out, mountInfo{mountpoint: mp, fstype: fstype})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var c byte
			ok := true
			for j := 1; j <= 3; j++ {
				d := s[i+j]
				if d < '0' || d > '7' {
					ok = false
					break
				}
				c = c<<3 | (d - '0')
			}
			if ok {
				b.WriteByte(c)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// dirFileRef — Linux 无 USN 等价物（design §4.10），恒失败（poll 不依赖它）。
func dirFileRef(string) (uint64, bool) { return 0, false }

// IsNTFS — 非 Windows 恒 false（usn 后端仅 Windows）。
func IsNTFS(string) bool { return false }
