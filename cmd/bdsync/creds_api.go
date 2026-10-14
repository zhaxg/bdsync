package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ── 服务模式（nssm / 计划任务 / systemd 托管）─────────────────────
//
// 判据只用**显式**信号，不做 stdout/管道之类的猜测：
// PowerShell 里跑原生命令时 stdout 本来就是管道，任何启发式都会把人误判成
// 服务模式（后果：启动失败不等按键、直接退窗口，人还没读到错误）。
//
//	nssm AppParameters 加 --service，或 AppEnvironment 加 BDSYNC_SERVICE=1
//
// 服务模式下禁止做两件"只有交互式终端才成立"的事：
//   - 启动失败后等人按键（nssm 下 stdin 常是无人喂的管道 → 进程永远卡住，
//     服务管理器只能判超时强杀，看起来就像"服务起不来"）
//   - 自动弹浏览器（服务会话没有桌面，rundll32 要么静默失败要么挂住）
var serviceMode = detectServiceMode()

func detectServiceMode() bool {
	if os.Getenv("BDSYNC_SERVICE") != "" {
		return true
	}
	return hasFlag(os.Args[1:], "--service") || hasFlag(os.Args[1:], "-service")
}

// stripServiceArgs — 从分发参数里剥掉 --service（它不是任何子命令的 flag）。
func stripServiceArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--service" || a == "-service" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// stdinInteractive — 当前进程的 stdin 是否接在控制台上。
//
// 与 serviceMode 分开：前者回答"是不是服务"，这个回答"此刻有没有人能按键"。
// 重定向到文件/管道时 fmt.Scanln 会一直挂着等数据 —— 那不是交互，是死等。
func stdinInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// pauseBeforeExit — 双击场景给用户读错误的时间；服务/重定向下绝不阻塞。
func pauseBeforeExit() {
	if serviceMode || !stdinInteractive() {
		return
	}
	fmt.Print("  按回车关闭窗口…")
	var line string
	_, _ = fmt.Scanln(&line)
}

// ── 凭据（cookie）查询与录入 ─────────────────────────────────────
//
// 面板侧要解决的问题：cookie 会过期。既不能进配置文件，也不该逼人重走向导，
// 所以看板上给一张「网盘凭据」卡片：看来源、贴新的、立即生效。
//
// 硬约束：**任何返回值都不含 BDUSS/STOKEN 本身**，连前几位都不回显 ——
// 看板只需要知道"有没有、从哪来、存在哪"。

// credsSource — 当前凭据来源，优先级与 loadCookie 一致。
// env（环境变量）> session（本次进程内录入）> file（credentials.dat）> none。
func credsSource() string {
	if os.Getenv("BDSYNC_COOKIE") != "" || os.Getenv("BDSYNC_BDUSS") != "" {
		return "env"
	}
	if c, _ := memCookie.Load().(string); c != "" {
		// 值可能是本次录入的，也可能是启动时从 credentials.dat 读回来的 —— 以记录的来源为准
		if src, _ := memCookieSrc.Load().(string); src != "" {
			return src
		}
		return "session"
	}
	if credentialsHint() != "" {
		return "file"
	}
	return "none"
}

// credsStatusPublic — /api/credentials GET 的载荷。只有元信息，没有凭据内容。
func credsStatusPublic() map[string]any {
	src := credsSource()
	return map[string]any{
		"source":    src,
		"path":      credentialsHint(),
		"has":       src != "none",
		"persisted": credentialsHint() != "",
	}
}

// cookieFromPayload — 从看板载荷提取 cookie。
//
// 整串粘贴优先（推荐路径），其次 bduss/stoken 拼装。
// 返回 empty=true 表示载荷里没给凭据（调用方自行决定是复用现有还是报错）。
func cookieFromPayload(payload map[string]any) (cookie, warn string, empty bool, err error) {
	raw, _ := payload["cookie"].(string)
	bduss, _ := payload["bduss"].(string)
	stoken, _ := payload["stoken"].(string)
	raw, bduss, stoken = strings.TrimSpace(raw), strings.TrimSpace(bduss), strings.TrimSpace(stoken)

	switch {
	case raw != "":
		if !strings.Contains(raw, "BDUSS=") {
			return "", "", false, errors.New("Cookie 串里没有 BDUSS=…，请确认复制完整")
		}
		cookie = raw
		if !strings.Contains(strings.ToUpper(raw), "STOKEN=") {
			// 实测：只有 BDUSS 时网盘列目录会被拒（-6 请重新登录）——提示但不阻断
			warn = "这串 Cookie 里没有 STOKEN，网盘很可能拒绝列目录（实测如此）"
		}
	case bduss != "":
		cookie = "BDUSS=" + bduss
		if stoken != "" {
			cookie += "; STOKEN=" + stoken
		} else {
			warn = "没填 STOKEN，网盘很可能拒绝列目录（实测如此）"
		}
	default:
		empty = true
	}
	return cookie, warn, empty, nil
}

// saveCookieToDisk — 按 remember 落盘（明文 credentials.dat）。
// remember=false 时同时清掉已有文件：那意味着"别给我记住"，留着旧的下次会读到旧凭据。
func saveCookieToDisk(cookie string, remember bool) error {
	if remember {
		if err := saveCredentials(cookie); err != nil {
			return errCredsSaveFailed(err)
		}
		return nil
	}
	deleteCredentials()
	return nil
}
