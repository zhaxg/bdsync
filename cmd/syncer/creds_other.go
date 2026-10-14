//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 非 Windows：没有 DPAPI，宁可不持久化，也不落明文凭据。
// （Linux 部署件尚未提供；双击场景本来就以 Windows 为主。）

func credsPath() string {
	base, _ := os.UserConfigDir()
	return filepath.Join(base, "syncer", "credentials")
}

func saveCredentialsDPAPI(cookie string) error {
	if strings.TrimSpace(cookie) != "" {
		return fmt.Errorf("当前平台不提供凭据加密存储：请改用环境变量 SYNCER_COOKIE")
	}
	return nil
}

func loadCredentialsDPAPI() string { return "" }

func deleteCredentialsDPAPI() {}

func credentialsHint() string { return "" }
