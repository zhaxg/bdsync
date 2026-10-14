//go:build windows

package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// 凭据持久化：DPAPI（CryptProtectData）按当前 Windows 用户加密，
// 落盘文件 %APPDATA%\bdsync\credentials.dat 只有同一用户能解密。
//
// 为什么允许"记住凭据"：双击启动是主场景，用户不该每次敲环境变量；
// 但凭据绝不能以明文躺在配置或数据目录里 —— DPAPI 把这条约束兜住了。

var (
	crypt32              = syscall.NewLazyDLL("crypt32.dll")
	cryptProtectDataFunc = crypt32.NewProc("CryptProtectData")
	cryptUnprotectFunc   = crypt32.NewProc("CryptUnprotectData")
	localFreeFunc        = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
)

const (
	entropyPurpose = "bdsync/cookie/v1"
	dpapiFlag      = 0x01 // CRYPTPROTECT_UI_FORBIDDEN
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func blobFromBytes(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

// blobKeepAlive — 构造 DATA_BLOB 并保证底层字节在系统调用期间存活。
//
// 踩过的坑：直接把 []byte(cookie) 的 &b[0] 交给 CryptProtectData 时，GC 看不到
// 这个引用（它只经由 C 结构体里的裸指针可达），可能在调用期间回收内存 →
// 野指针访问违规 → 整个进程被系统干掉（Go 的 recover 都救不回来，stderr 全空）。
// 所以：切片必须由 Go 变量持有，并用 runtime.KeepAlive 在调用后显式续命。
func blobKeepAlive(b []byte) (*dataBlob, func()) {
	blob := blobFromBytes(b)
	return blob, func() { runtime.KeepAlive(b) }
}

func bytesFromBlob(b *dataBlob) []byte {
	if b == nil || b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

// credsPath — 凭据文件路径（%APPDATA%\bdsync\credentials.dat）。
func credsPath() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = home
	}
	return filepath.Join(base, "bdsync", "credentials.dat")
}

// saveCredentialsDPAPI — 加密写入；失败不阻塞（调用方降级为"仅本次会话"）。
func saveCredentialsDPAPI(cookie string) error {
	if strings.TrimSpace(cookie) == "" {
		_ = os.Remove(credsPath())
		return nil
	}
	desc, err := syscall.UTF16PtrFromString(entropyPurpose)
	if err != nil {
		return err
	}
	raw := []byte(cookie) // 必须由 Go 变量持有（见 blobKeepAlive 注释）
	in, keepAlive := blobKeepAlive(raw)
	var out dataBlob
	trace("dpapi: CryptProtectData 调用前")
	r, _, e := cryptProtectDataFunc.Call(
		uintptr(unsafe.Pointer(in)), uintptr(unsafe.Pointer(desc)), 0, 0, 0, dpapiFlag,
		uintptr(unsafe.Pointer(&out)))
	keepAlive()
	runtime.KeepAlive(desc)
	trace("dpapi: CryptProtectData 返回 r=%v cbData=%d", r, out.cbData)
	if r == 0 {
		return fmt.Errorf("DPAPI 加密失败: %w", e)
	}
	data := bytesFromBlob(&out)
	trace("dpapi: 读出 %d 字节", len(data))
	if err := os.MkdirAll(filepath.Dir(credsPath()), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(credsPath(), []byte(base64.StdEncoding.EncodeToString(data)), 0o600); err != nil {
		return err
	}
	trace("dpapi: 写文件完成")
	// 释放必须显式传 pbData：LocalFree(&out) 把的是 DATA_BLOB 结构体地址，不是数据地址
	localFreeFunc.Call(uintptr(unsafe.Pointer(out.pbData)))
	trace("dpapi: LocalFree 完成")
	return nil
}

// loadCredentialsDPAPI — 解密读取；任何失败都当作"没有凭据"。
func loadCredentialsDPAPI() string {
	raw, err := os.ReadFile(credsPath())
	if err != nil {
		return ""
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(data) == 0 {
		return ""
	}
	in, keepAlive := blobKeepAlive(data)
	var out dataBlob
	trace("dpapi: CryptUnprotectData 调用前")
	r, _, _ := cryptUnprotectFunc.Call(
		uintptr(unsafe.Pointer(in)), 0, 0, 0, 0, dpapiFlag,
		uintptr(unsafe.Pointer(&out)))
	keepAlive()
	trace("dpapi: CryptUnprotectData 返回 r=%v cbData=%d", r, out.cbData)
	if r == 0 {
		return ""
	}
	out2 := bytesFromBlob(&out)
	localFreeFunc.Call(uintptr(unsafe.Pointer(out.pbData)))
	if len(out2) > 64*1024 { // 合理性上限：凭据串不该超过几十 KB
		return ""
	}
	return string(out2)
}

func deleteCredentialsDPAPI() { _ = os.Remove(credsPath()) }

func credentialsHint() string {
	if st, err := os.Stat(credsPath()); err == nil && st.Size() > 0 {
		return credsPath()
	}
	return ""
}
