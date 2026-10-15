package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 凭据持久化：**明文**存 <exe 同目录>/credentials.dat。
//
// 为什么是明文（有意的取舍，不是偷懒）：
//   - 自用工具，机器是自己的；DPAPI 换个 Windows 账号/重装就废，反而更麻烦
//   - cookie 本来就在浏览器里以同等强度躺着，这里再加密不提升实际安全边界
//   - DPAPI 那套（DATA_BLOB 手工编解码 + 裸指针 + keepAlive）是本项目 bug 密度
//     最高的一块，删掉它净收益为正
//
// 仍然守住的两条：
//   - **绝不进 bdsync.yaml** ---- 配置文件会被人当样例拷走/提交，凭据混进去才是真事故
//   - **绝不回传给看板** ---- /api/credentials 只报"有没有、从哪来"，不回显内容
//
// 代价（明说）：能读这个目录的人就能读到 cookie。所以 .gitignore 里有它，
// 而且目录权限跟随 exe 所在目录。
func credsPath() string {
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		return filepath.Join(filepath.Dir(exe), "credentials.dat")
	}
	return "credentials.dat"
}

// saveCredentials -- 写入；空串表示清除。失败不阻塞（调用方降级为"仅本次会话"）。
func saveCredentials(cookie string) error {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		deleteCredentials()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(credsPath()), 0o700); err != nil {
		return err
	}
	// 0600：同机其他用户读不了（Windows 上 ACL 仍由目录继承，权当尽力）
	return os.WriteFile(credsPath(), []byte(cookie), 0o600)
}

// loadCredentials -- 读取；文件不存在/为空都当作"没有凭据"。
func loadCredentials() string {
	raw, err := os.ReadFile(credsPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func deleteCredentials() { _ = os.Remove(credsPath()) }

// credentialsHint -- 凭据文件路径（存在且非空时）；看板只显示这个路径，不显示内容。
func credentialsHint() string {
	if st, err := os.Stat(credsPath()); err == nil && st.Size() > 0 {
		return credsPath()
	}
	return ""
}

// requireCookie -- 拿不到凭据时给出可操作的三条路，而不是一句"失败"。
func requireCookie() (string, error) {
	if c := loadCredentials(); c != "" {
		return c, nil
	}
	return "", errors.New("缺少网盘凭据（BDUSS/STOKEN）。三种方式任选其一：\n" +
		"  1) 看板「配置 → 网盘凭据」里贴一条 cookie（推荐；保存后自动重启生效）\n" +
		"  2) 环境变量 BDSYNC_COOKIE=\"BDUSS=xxx; STOKEN=yyy\"\n" +
		"  3) 首次向导勾选『记住凭据』---- 明文存到二进制同目录 credentials.dat")
}

// errCredsSaveFailed -- 保存失败时的提示（仍可选择不持久化）。
func errCredsSaveFailed(err error) error {
	return fmt.Errorf("保存凭据失败：%w（可取消勾选『记住凭据』，仅本次会话有效）", err)
}
