//go:build windows

package detector

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// GetDriveTypeW 返回值（winbase.h）
const (
	driveUnknown   = 0
	driveNoRootDir = 1
	driveRemovable = 2 // U 盘 → 拒绝
	driveFixed     = 3 // 唯一接受
	driveRemote    = 4 // 网络盘 → 拒绝
	driveCDROM     = 5
	driveRAMDisk   = 6
)

var procGetDriveType = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")
var procGetVolumeInformation = syscall.NewLazyDLL("kernel32.dll").NewProc("GetVolumeInformationW")

// ValidateLocalRoot 启动时强制校验（design.md §1.1 / P4）：
// 只接受本地固定磁盘。网络盘 / 可移动盘上变更无法可靠检测，会导致备份静默遗漏。
func ValidateLocalRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("无法解析路径 %q: %w", root, err)
	}

	// UNC 路径（\\NAS\share）与 \\?\UNC\ 一律拒绝
	p := strings.TrimPrefix(abs, `\\?\`)
	if strings.HasPrefix(p, `\\`) {
		return fmt.Errorf("[X] 不支持网络路径：%s\n"+
			"  本工具仅支持本地固定磁盘。原因：网络盘上文件变动无法可靠检测，\n"+
			"  可能导致备份静默遗漏文件。\n"+
			"  如需备份 NAS 数据，请先将数据同步到本地磁盘再使用本工具。", abs)
	}
	if len(p) < 3 || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
		return fmt.Errorf("[X] 不支持的路径形式（需要盘符路径，如 D:\\BackupRoot）：%s", abs)
	}

	// GetDriveTypeW 需要 "D:\" 形式的卷根
	volRoot := p[:2] + `\`
	ptr, err := syscall.UTF16PtrFromString(volRoot)
	if err != nil {
		return err
	}
	dt, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(ptr)))
	switch uint32(dt) {
	case driveFixed:
		// 唯一接受的类型
	case driveRemote:
		return fmt.Errorf("[X] 不支持网络盘/网络共享：%s\n"+
			"  本工具仅支持本地固定磁盘。原因：网络盘上文件变动无法可靠检测，\n"+
			"  可能导致备份静默遗漏文件。\n"+
			"  如需备份 NAS 数据，请先将数据同步到本地磁盘再使用本工具。", abs)
	case driveRemovable:
		return fmt.Errorf("[X] 不支持可移动磁盘：%s\n"+
			"  U 盘/移动硬盘可能随时被拔出，且不保证是 NTFS，变更检测不可靠。\n"+
			"  请将数据复制到本地固定磁盘后再备份。", abs)
	default:
		return fmt.Errorf("[X] 非本地固定磁盘（DriveType=%d）：%s\n"+
			"  仅接受本地固定磁盘（DRIVE_FIXED）。", dt, abs)
	}

	st, err := os.Stat(abs) // 只读操作
	if err != nil {
		return fmt.Errorf("[X] 根目录不可访问：%s\n  %v", abs, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("[X] 根目录不是目录：%s", abs)
	}
	return nil
}

// dirFileRef -- 取目录的 NTFS FileReferenceNumber（usn 模式路径解析用，design §4.2c）。
// 只读属性即可，无需 SeManageVolumePrivilege。
func dirFileRef(path string) (uint64, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	const fileReadAttributes = 0x80
	h, err := syscall.CreateFile(p,
		fileReadAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0, false
	}
	ref := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return ref, ref != 0
}

// IsNTFS -- 根目录所在卷是否 NTFS（usn 后端的前提）。
func IsNTFS(root string) bool {
	abs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	p := strings.TrimPrefix(abs, `\\?\`)
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	volRoot := p[:2] + `\`
	ptr, err := syscall.UTF16PtrFromString(volRoot)
	if err != nil {
		return false
	}
	var buf [64]uint16
	// GetVolumeInformationW 共 8 个参数：root, volName, volNameSize, serial,
	// maxCompLen, flags, fsName, fsNameSize
	r, _, _ := procGetVolumeInformation.Call(
		uintptr(unsafe.Pointer(ptr)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r == 0 {
		return false
	}
	return strings.EqualFold(syscall.UTF16ToString(buf[:]), "NTFS")
}
