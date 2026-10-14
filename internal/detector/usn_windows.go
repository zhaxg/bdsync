//go:build windows

package detector

// NTFS USN Journal 检测后端（design §4.2）。
//
// 关键性质：空闲时只读内核已缓存的 journal——不打开任何文件、不唤醒磁盘、
// 成本与目录规模无关。丢失可检测：journal ID 变化 / 游标低于 LowestValidUsn
// → 回退全量扫描（这是正常路径，不是异常，design §4.2d）。
//
// 路径解析：USN 记录只有 (父引用号, 文件名)。目录引用号 → 相对路径的映射存在
// dir_ref 表（全量扫描时重建 + 运行中增量维护）。父引用号解析不到 = 在监控
// 根目录之外 → 忽略（对账扫描兜底）。

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"syncer/internal/index"
)

// FSCTL 与 Reason 位（winioctl.h，见 design 附录 A）
const (
	fsctlUsnReadJournal = 0x000900BB
	fsctlReadUsnJournal = 0x000900A4

	reasonDataExtend    = 0x00000001
	reasonDataOverwrite = 0x00000002
	reasonFileCreate    = 0x00000100
	reasonFileDelete    = 0x00000200
	reasonRenameOldName = 0x00001000
	reasonRenameNewName = 0x00002000

	fileAttributeDirectory = 0x10

	usnReasonMask = reasonDataExtend | reasonDataOverwrite | reasonFileCreate |
		reasonFileDelete | reasonRenameOldName | reasonRenameNewName
)

// kv 键（每 pair 一份游标状态）
const (
	kvUsnJournalID = "usn.journal_id"
	kvUsnCursor    = "usn.cursor"
)

type journalMeta struct {
	JournalID      uint64
	FirstUsn       uint64
	NextUsn        uint64
	LowestValidUsn uint64
}

type usnRecord struct {
	Ref       uint64
	ParentRef uint64
	Usn       uint64
	Reason    uint32
	Attrs     uint32
	Name      string
}

// UsnBackend — 卷级 USN 读取器。
type UsnBackend struct {
	root    string // 普通绝对路径
	volPath string // \\?\E:
	rootRef uint64 // root 目录的引用号
	h       syscall.Handle
	store   *index.Store
}

var _ Backend = (*UsnBackend)(nil)

func (u *UsnBackend) Name() string { return "usn" }
func (u *UsnBackend) Start(context.Context, string) error {
	return fmt.Errorf("%w: usn 由 RunUsnCycle 驱动", ErrNotImplemented)
}
func (u *UsnBackend) Events() <-chan Event { return nil }
func (u *UsnBackend) Stats() Stats         { return Stats{Backend: "usn"} }

func (u *UsnBackend) Close() error {
	if u.h != 0 {
		syscall.CloseHandle(u.h)
		u.h = 0
	}
	return nil
}

// SelfTest — 写探测文件并确认 journal 事件可达（唯一能发现"环境变了"的手段，design 硬规矩①）。
// 只由显式 `syncer selftest` 命令触发（会向 root 写入并删除一个临时文件）。
func (u *UsnBackend) SelfTest(ctx context.Context, root string) error {
	probeRel := fmt.Sprintf(".syncer-probe-%d.tmp", time.Now().UnixNano())
	probeAbs := root + `\` + probeRel
	if err := writeProbeFile(probeAbs); err != nil {
		return fmt.Errorf("写探测文件失败: %w", err)
	}
	defer removeFile(probeAbs)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cyc, err := u.DetectCycle(ctx)
		if err != nil {
			return err
		}
		for _, p := range cyc.Changed {
			if p == probeRel || strings.EqualFold(p, probeRel) {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("10s 内未收到探测文件的 USN 事件（后端可能已静默失效）")
}

// OpenUsn — 打开卷（只读）并取 root 引用号。
func OpenUsn(root string, st *index.Store) (*UsnBackend, error) {
	abs, err := normalizeWalkRoot(root) // \\?\D:\data 形式
	if err != nil {
		return nil, err
	}
	// 卷路径用本地设备命名空间 \\.\E:（MSDN 标准形式，\\?\ 打开卷会被拒）
	sep := strings.IndexByte(abs[4:], ':')
	if sep < 0 {
		return nil, fmt.Errorf("usn 后端仅支持盘符路径: %s", root)
	}
	vol := `\\.\` + abs[4:4+sep+1] // \\.\E:

	rootRef, ok := dirFileRef(abs)
	if !ok {
		return nil, fmt.Errorf("无法取得根目录引用号（非 NTFS 或无权限）: %s", root)
	}

	ptr, err := syscall.UTF16PtrFromString(vol)
	if err != nil {
		return nil, err
	}
	// 普通用户只读打开即可（design §4.2e），无需 SeManageVolumePrivilege
	h, err := syscall.CreateFile(ptr,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("打开卷 %s 失败（将回退 poll）: %w", vol, err)
	}
	return &UsnBackend{root: root, volPath: vol, rootRef: rootRef, h: h, store: st}, nil
}

// readMeta — FSCTL_USN_READ_JOURNAL 取日志元数据（V2 含 JournalID，失败退 V0）。
func (u *UsnBackend) readMeta() (journalMeta, error) {
	var out [64]byte
	var returned uint32
	err := syscall.DeviceIoControl(u.h, fsctlUsnReadJournal,
		nil, 0, &out[0], uint32(len(out)), &returned, nil)
	if err != nil {
		return journalMeta{}, fmt.Errorf("FSCTL_USN_READ_JOURNAL: %w", err)
	}
	le := binary.LittleEndian
	if returned >= 60 {
		return journalMeta{
			JournalID:      le.Uint64(out[0:]),
			FirstUsn:       le.Uint64(out[8:]),
			NextUsn:        le.Uint64(out[16:]),
			LowestValidUsn: le.Uint64(out[24:]),
		}, nil
	}
	if returned >= 24 { // V0：无 JournalID
		return journalMeta{
			FirstUsn:       le.Uint64(out[0:]),
			NextUsn:        le.Uint64(out[8:]),
			LowestValidUsn: le.Uint64(out[16:]),
		}, nil
	}
	return journalMeta{}, fmt.Errorf("journal 元数据过短: %d 字节", returned)
}

// readBatch — FSCTL_READ_USN_JOURNAL 从 startUsn 起读一批记录。
func (u *UsnBackend) readBatch(startUsn, journalID uint64) (uint64, []usnRecord, error) {
	var in [40]byte
	le := binary.LittleEndian
	// READ_USN_JOURNAL_DATA_V0: StartUsn@0, ReasonMask@8, ReturnOnlyOnClose@12,
	// Timeout@16, BytesToWaitFor@24, UsnJournalID@32
	le.PutUint64(in[0:], startUsn)
	le.PutUint32(in[8:], usnReasonMask)
	le.PutUint64(in[32:], journalID)

	buf := make([]byte, 1<<20)
	var returned uint32
	err := syscall.DeviceIoControl(u.h, fsctlReadUsnJournal,
		&in[0], uint32(len(in)), &buf[0], uint32(len(buf)), &returned, nil)
	if err != nil {
		// 日志被清空/重建时返回 ERROR_INVALID_DATA 等 → 交给上层判全量
		return 0, nil, err
	}
	if returned < 8 {
		return startUsn, nil, nil
	}
	next := le.Uint64(buf[:8])
	if returned <= 8 {
		return next, nil, nil
	}
	return next, parseUsnRecords(buf[8:returned]), nil
}

// parseUsnRecords — 防御性解析 USN_RECORD_V2/V4（design 附录 A：V2 需 Win8.1+；
// V4 为 128 位引用号变体，按长度自适应）。
func parseUsnRecords(b []byte) []usnRecord {
	le := binary.LittleEndian
	var out []usnRecord
	for len(b) >= 16 {
		recLen := le.Uint32(b[0:4])
		if recLen < 16 || int(recLen) > len(b) {
			break // 损坏/截断：停，不猜
		}
		rec := b[:recLen]
		major := le.Uint16(rec[4:6])
		var ref, parent uint64
		var nameOff, nameLen int
		v4 := false
		switch major {
		case 2, 3:
			ref = le.Uint64(rec[8:])
			parent = le.Uint64(rec[16:])
			if len(rec) < 60 {
				break
			}
			nameOff = int(le.Uint16(rec[58:]))
			nameLen = int(le.Uint16(rec[56:]))
		case 4:
			// V4（NTFS 罕见，ReFS 常见）：128 位引用号取低 64 位；
			// 布局 Usn@40 Reason@48 FileAttributes@56(QWORD) NameLen@64 NameOff@66
			ref = le.Uint64(rec[8:])
			parent = le.Uint64(rec[24:])
			if len(rec) < 68 {
				break
			}
			nameOff = int(le.Uint16(rec[66:]))
			nameLen = int(le.Uint16(rec[64:]))
			v4 = true
		default:
			b = b[recLen:]
			continue
		}
		if nameOff > 0 && nameLen > 0 && nameOff+nameLen <= len(rec) {
			name := decodeUTF16(rec[nameOff : nameOff+nameLen])
			r := usnRecord{
				Ref:       ref,
				ParentRef: parent,
				Name:      name,
			}
			if v4 {
				r.Usn = le.Uint64(rec[40:])
				r.Reason = le.Uint32(rec[48:])
				r.Attrs = uint32(le.Uint64(rec[56:]))
			} else {
				r.Usn = le.Uint64(rec[24:])
				r.Reason = le.Uint32(rec[40:])
				r.Attrs = le.Uint32(rec[52:])
			}
			out = append(out, r)
		}
		b = b[recLen:]
	}
	return out
}

// UsnCycleResult — 一轮 journal 检测的结果。
type UsnCycleResult struct {
	Changed        []string // 值得看一眼的路径（相对 root，'/' 分隔）
	Deleted        []string
	Dirs           int // 新建目录数
	Records        int
	Renamed        int
	FullScanNeeded bool // 游标失效/journal 重建 → 上层回退全量扫描（正常路径，design §4.2d）
	Cursor         uint64
	JournalNext    uint64
}

// DetectCycle — 读一段 journal，产出变更/删除路径集合。
// 硬规矩②：这里只产出"值得看一眼"的路径；判定永远由上层读索引完成。
func (u *UsnBackend) DetectCycle(ctx context.Context) (*UsnCycleResult, error) {
	res := &UsnCycleResult{}

	meta, err := u.readMeta()
	if err != nil {
		return nil, err
	}

	rawJID, _ := u.store.GetKV(ctx, kvUsnJournalID)
	rawCursor, _ := u.store.GetKV(ctx, kvUsnCursor)
	storedJID, _ := parseU64(rawJID)
	cursor, _ := parseU64(rawCursor)

	// 首次：只记游标（全量扫描已覆盖现状）
	if storedJID == 0 || cursor == 0 {
		if err := u.saveCursor(ctx, meta.JournalID, meta.NextUsn); err != nil {
			return nil, err
		}
		// 防漏保险：dir_ref 从未引导过 → 必须先全量扫描建表，
		// 否则新文件的父引用号解析不到会被当"根之外"忽略（静默漏文件！）
		if n, err := u.store.DirRefCount(ctx); err == nil && n == 0 {
			res.FullScanNeeded = true
		}
		res.Cursor = meta.NextUsn
		res.JournalNext = meta.NextUsn
		return res, nil
	}
	// journal ID 变化（格式化/修复/换盘）或游标低于有效下限（回绕）
	// → 记录作废，回退全量扫描。这是长期运行的正常路径，不是异常（design §4.2d）。
	if meta.JournalID != storedJID || cursor < meta.LowestValidUsn || cursor > meta.NextUsn {
		if err := u.saveCursor(ctx, meta.JournalID, meta.NextUsn); err != nil {
			return nil, err
		}
		res.FullScanNeeded = true
		res.Cursor = meta.NextUsn
		res.JournalNext = meta.NextUsn
		return res, nil
	}

	var (
		newRefs  = map[uint64]string{}    // 本批新建目录的 overlay（先内存后落库）
		oldNames = map[uint64]usnRecord{} // 改名配对（同批内 OLD→NEW）
		changed  = map[string]bool{}
		deleted  = map[string]bool{}
		persist  []index.DirRef
	)
	resolve := func(parent uint64, name string) (string, bool) {
		if parent == u.rootRef {
			return name, true
		}
		if p, ok := newRefs[parent]; ok {
			if p == "" {
				return name, true
			}
			return p + "/" + name, true
		}
		p, ok, err := u.store.ResolveRef(ctx, parent)
		if err != nil || !ok {
			return "", false // 解析不到 = 在监控根之外（或已删除）→ 忽略，对账兜底
		}
		if p == "" {
			return name, true
		}
		return p + "/" + name, true
	}

	const maxBatches = 512 // 安全阀
	for i := 0; i < maxBatches; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, recs, err := u.readBatch(cursor, meta.JournalID)
		if err != nil {
			// 读取失败（journal 被重建等）→ 全量兜底
			res.FullScanNeeded = true
			_ = u.saveCursor(ctx, meta.JournalID, meta.NextUsn)
			return res, nil
		}
		for _, rec := range recs {
			res.Records++
			isDir := rec.Attrs&fileAttributeDirectory != 0
			switch {
			case rec.Reason&reasonRenameOldName != 0:
				oldNames[rec.Ref] = rec

			case rec.Reason&reasonRenameNewName != 0:
				newPath, ok := resolve(rec.ParentRef, rec.Name)
				if !ok {
					continue
				}
				old, paired := oldNames[rec.Ref]
				if !paired {
					if isDir {
						// 目录改名但 OLD 记录不在本段（游标起点在配对中间）→ 全量兜底
						res.FullScanNeeded = true
					} else {
						changed[newPath] = true
					}
					continue
				}
				oldPath, ok2 := resolve(old.ParentRef, old.Name)
				if !ok2 {
					changed[newPath] = true
					continue
				}
				res.Renamed++
				if isDir {
					// 目录改名：dir_ref 与 entry 子树整体重写（SQL 前缀替换）
					if err := u.store.RewriteSubtree(ctx, oldPath, newPath); err != nil {
						return nil, err
					}
				} else {
					if err := u.store.MoveEntry(ctx, oldPath, newPath); err != nil {
						return nil, err
					}
					changed[newPath] = true
				}

			case rec.Reason&reasonFileDelete != 0:
				if p, ok := resolve(rec.ParentRef, rec.Name); ok {
					deleted[p] = true
					// 目录删除：dir_ref 留陈旧项，下次全量重建自愈（design §4.9 硬规矩③）
				}

			case isDir && rec.Reason&reasonFileCreate != 0:
				// 新目录：立即登记引用，后续记录才能解析到它
				p, ok := resolve(rec.ParentRef, rec.Name)
				if !ok {
					continue
				}
				newRefs[rec.Ref] = p
				persist = append(persist, index.DirRef{Ref: rec.Ref, RelPath: p})
				changed[p] = true
				res.Dirs++

			default:
				if p, ok := resolve(rec.ParentRef, rec.Name); ok {
					changed[p] = true
				}
			}
		}
		cursor = next
		if err := u.saveCursor(ctx, meta.JournalID, cursor); err != nil {
			return nil, err
		}
		if len(recs) == 0 || cursor >= meta.NextUsn {
			break
		}
	}

	if len(persist) > 0 {
		if err := u.store.AddDirRefs(ctx, persist); err != nil {
			return nil, err
		}
	}
	for p := range changed {
		if !deleted[p] {
			res.Changed = append(res.Changed, p)
		}
	}
	for p := range deleted {
		res.Deleted = append(res.Deleted, p)
	}
	res.Cursor = cursor
	res.JournalNext = meta.NextUsn
	return res, nil
}

func (u *UsnBackend) saveCursor(ctx context.Context, jid, cursor uint64) error {
	if err := u.store.PutKV(ctx, kvUsnJournalID, []byte(fmt.Sprint(jid))); err != nil {
		return err
	}
	return u.store.PutKV(ctx, kvUsnCursor, []byte(fmt.Sprint(cursor)))
}

func parseU64(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("empty")
	}
	var v uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad u64")
		}
		v = v*10 + uint64(c-'0')
	}
	return v, nil
}

// RunUsnCycle — 一轮 usn 检测 + 点查写库（查询次数 ∝ 变更数，design §4.7）。
// FullScan=true 时上层必须回退 poll 全量扫描（这轮的变更不做删除判定）。
func RunUsnCycle(ctx context.Context, root string, st *index.Store, exclude []string) (*ScanResult, bool, error) {
	start := time.Now()
	res := &ScanResult{Root: root, Backend: "usn"}

	u, err := OpenUsn(root, st)
	if err != nil {
		return res, true, err // 打不开 → 上层降级 poll
	}
	defer u.Close()

	cyc, err := u.DetectCycle(ctx)
	if err != nil {
		return res, true, err
	}
	res.WalkOK = true
	defer func() {
		res.WallMs = ms(time.Since(start))
		res.At = time.Now()
	}()

	if cyc.FullScanNeeded {
		res.WalkOK = false
		return res, true, nil
	}
	res.Files = int64(len(cyc.Changed))
	res.Dirs = int64(cyc.Dirs)
	res.Skipped = int64(len(cyc.Deleted))

	if len(cyc.Changed) == 0 && len(cyc.Deleted) == 0 {
		return res, false, nil
	}

	runID, err := st.NextRunID(ctx)
	if err != nil {
		return res, false, err
	}
	added, modified, resurrected, gone, conflicted, err := ApplyChanges(ctx, st, root, cyc.Changed, cyc.Deleted, runID, exclude)
	if err != nil {
		return res, false, err
	}
	res.AddedFiles = int64(added)
	res.Modified = int64(modified)
	res.Resurrected = int64(resurrected)
	res.Removed = int64(gone)
	res.Conflicted = int64(conflicted)
	return res, false, nil
}

// decodeUTF16 — USN 记录里的文件名（UTF-16LE）→ UTF-8。
func decodeUTF16(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

// RunUsnSelftest — 显式自检入口（syncer selftest 用）：写探测文件并等 journal 事件。
func RunUsnSelftest(ctx context.Context, root string, st *index.Store) error {
	u, err := OpenUsn(root, st)
	if err != nil {
		return err
	}
	defer u.Close()
	return u.SelfTest(ctx, root)
}
