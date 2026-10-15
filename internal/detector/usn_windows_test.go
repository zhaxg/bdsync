//go:build windows

package detector

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// buildV2 -- 合成 USN_RECORD_V2。
func buildV2(t *testing.T, reason uint32, attrs uint32, ref, parent uint64, usn uint64, name string) []byte {
	t.Helper()
	u16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(u16)*2)
	for i, c := range u16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], c)
	}
	const header = 60
	recLen := header + len(nameBytes)
	b := make([]byte, recLen)
	le := binary.LittleEndian
	le.PutUint32(b[0:], uint32(recLen))
	le.PutUint16(b[4:], 2) // MajorVersion
	le.PutUint16(b[6:], 0)
	le.PutUint64(b[8:], ref)
	le.PutUint64(b[16:], parent)
	le.PutUint64(b[24:], usn)
	le.PutUint64(b[32:], 0x0123456789ABCDEF) // TimeStamp
	le.PutUint32(b[40:], reason)
	le.PutUint32(b[44:], 0) // SourceInfo
	le.PutUint32(b[48:], 0) // SecurityId
	le.PutUint32(b[52:], attrs)
	le.PutUint16(b[56:], uint16(len(nameBytes))) // FileNameLength
	le.PutUint16(b[58:], header)                 // FileNameOffset
	copy(b[header:], nameBytes)
	return b
}

// buildV4 -- 合成 USN_RECORD_V4（128 位引用号变体）。
func buildV4(t *testing.T, reason uint32, attrs uint64, ref, parent, usn uint64, name string) []byte {
	t.Helper()
	u16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(u16)*2)
	for i, c := range u16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], c)
	}
	const header = 68
	recLen := header + len(nameBytes)
	b := make([]byte, recLen)
	le := binary.LittleEndian
	le.PutUint32(b[0:], uint32(recLen))
	le.PutUint16(b[4:], 4) // MajorVersion = 4
	le.PutUint64(b[8:], ref)
	le.PutUint64(b[24:], parent)
	le.PutUint64(b[40:], usn)
	le.PutUint32(b[48:], reason)
	le.PutUint64(b[56:], attrs)
	le.PutUint16(b[64:], uint16(len(nameBytes)))
	le.PutUint16(b[66:], header)
	copy(b[header:], nameBytes)
	return b
}

func TestParseUsnRecordsV2(t *testing.T) {
	r1 := buildV2(t, reasonFileCreate, 0x20, 0x100, 0x10, 100, "创建文件.txt")
	r2 := buildV2(t, reasonDataExtend, 0x20, 0x101, 0x10, 200, "a.txt")
	r3 := buildV2(t, reasonRenameOldName|reasonRenameNewName, 0x20, 0x102, 0x20, 300, "old.txt")
	buf := append(append(r1, r2...), r3...)

	recs := parseUsnRecords(buf)
	if len(recs) != 3 {
		t.Fatalf("解析出 %d 条，期望 3 条", len(recs))
	}
	if recs[0].Name != "创建文件.txt" || recs[0].Ref != 0x100 || recs[0].ParentRef != 0x10 {
		t.Errorf("记录1 解析错误: %+v", recs[0])
	}
	if recs[0].Reason != reasonFileCreate || recs[0].Attrs != 0x20 || recs[0].Usn != 100 {
		t.Errorf("记录1 字段错误: %+v", recs[0])
	}
	if recs[1].Name != "a.txt" || recs[1].Reason != reasonDataExtend {
		t.Errorf("记录2 解析错误: %+v", recs[1])
	}
	if recs[2].Name != "old.txt" || recs[2].Usn != 300 {
		t.Errorf("记录3 解析错误: %+v", recs[2])
	}
}

func TestParseUsnRecordsV4(t *testing.T) {
	r := buildV4(t, reasonFileDelete, 0x10, 0xABCD, 0x10, 500, "删除我.dat")
	recs := parseUsnRecords(r)
	if len(recs) != 1 {
		t.Fatalf("V4 解析出 %d 条", len(recs))
	}
	if recs[0].Name != "删除我.dat" || recs[0].Ref != 0xABCD ||
		recs[0].Reason != reasonFileDelete || recs[0].Attrs != 0x10 || recs[0].Usn != 500 {
		t.Errorf("V4 解析错误: %+v", recs[0])
	}
}

func TestParseUsnRecordsDefensive(t *testing.T) {
	// 截断记录：RecordLength 超出剩余长度 → 停止，不 panic
	good := buildV2(t, reasonFileCreate, 0, 1, 2, 3, "x")
	buf := append(append([]byte{}, good...), 0x78, 0x56) // 尾部残缺
	recs := parseUsnRecords(buf)
	if len(recs) != 1 {
		t.Fatalf("应解析出 1 条完整记录，得到 %d", len(recs))
	}
	// 全垃圾 → 0 条，不 panic
	垃圾 := make([]byte, 64)
	for i := range 垃圾 {
		垃圾[i] = 0xFF
	}
	if recs := parseUsnRecords(垃圾); len(recs) != 0 {
		t.Fatalf("垃圾输入应得 0 条，得到 %d", len(recs))
	}
	// 空输入
	if recs := parseUsnRecords(nil); len(recs) != 0 {
		t.Fatal("空输入应得 0 条")
	}
}
