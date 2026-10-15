package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bdsync/internal/config"
	"bdsync/internal/detector"
	"bdsync/internal/index"
	"bdsync/web"
)

// trace -- 诊断用分步追踪（设 BDSYNC_TRACE=<文件> 开启）。
// 存在的意义：崩溃类问题在 stderr 里常常什么都不剩（进程被直接带走），
// 靠这个文件可以精确定位"死在哪一步"。
func trace(format string, a ...any) {
	p := os.Getenv("BDSYNC_TRACE")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, a...)...)
	_ = f.Close()
}

// cmdWeb -- 双击入口：先起看板，再谈配置。
//
// 设计意图（降低门槛）：
//
//	零配置双击 → 进程不退出，127.0.0.1 看板一定起来 → 控制台打印地址（**不自动开浏览器**）
//	  → 用户自己 Ctrl+单击地址打开
//	    → 网页向导填「本地目录 / 远端目录 / 凭据」
//	      → 校验通过：写 bdsync.yaml（含 service.initialized），启动备份运行时
//	      → 校验失败：页面停在向导并显示原因，人改完再点一次
//
// 已初始化的机器（有配置 + pair 齐全）→ 直接进服务态，不再走向导；
// 凭据过期只是运行时错误，显示在看板的「凭据」卡片里就地修复。
//
// 配置失败绝不静默退出：那正是新手最容易卡住的地方。
func cmdWeb(args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	addr := fs.String("addr", "", "看板地址（默认取配置 web.addr，回环地址）")
	browser := fs.Bool("browser", false, "启动后自动打开浏览器（默认不打开，只打印地址）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// ── 1. 尽量加载配置；失败不致命，转入向导模式 ──
	eff := config.Clone(appCfg)
	initialized := isInitialized(eff)
	setupErr := error(nil)
	if err := eff.Validate(); err != nil {
		setupErr = err
	} else if err := eff.ValidatePair(); err != nil {
		setupErr = err
	} else if !initialized {
		// 已初始化时 cookie 缺失不是"没配完"，而是运行时错误：
		// 让它进服务态，由看板「凭据」卡片就地补，别把人拽回向导。
		if _, cerr := loadCookie(); cerr != nil {
			setupErr = cerr
		}
	}
	if *addr != "" {
		eff.Web.Addr = *addr
		// 校验（关键：addr 必须是回环，design §13）
		if err := eff.Validate(); err != nil {
			return err
		}
	}

	// ── 2. 看板先起来（这是"双击就能看见"的保证）──
	hub := web.NewHub()
	st8 := &setupState{
		configPath:  appCfgPath,
		version:     version,
		initialized: initialized,
	}
	srv := web.New(eff.Web.Addr, hub, st8.status, st8.ctrl(), web.ConfigIO{
		Get:  func() (string, map[string]any, []string) { return st8.configGet() },
		Save: func(p map[string]any) ([]string, []string, error) { return st8.configSave(p) },
	}).WithSetup(web.SetupIO{
		State: st8.state,
		Apply: st8.apply,
		Probe: st8.probe,
	}).WithCreds(web.CredsIO{
		Status: st8.credsStatus,
		Save:   st8.credsSave,
	})
	if err := srv.Start(); err != nil {
		// 端口已被占用 = 大概率已经有一个实例在跑：给一句人话 + 直接开看板
		if isAddrInUse(err) {
			fmt.Println()
			fmt.Println("  已经有 bdsync 在运行（看板端口被占用）。")
			fmt.Printf("  服务已启动到 %s （按住 Ctrl 单击即可用浏览器打开）\n", webAddrHint(eff))
			fmt.Println()
			if *browser {
				openBrowser(webAddrHint(eff))
			}
			return nil
		}
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Stop(c)
	}()

	printBanner(srv.URL(), initialized, setupErr)
	if *browser {
		go func() {
			time.Sleep(600 * time.Millisecond) // 等看板完全就绪再开
			openBrowser(srv.URL())
		}()
	}

	st8.srv = srv
	setupHub = hub
	st8.logf("[看板] %s", srv.URL())

	ctx, stop := signal.NotifyContext(context.Background(), stopSignals()...)
	defer stop()
	// procCtx = 信号 + 看板「停止」按钮的共同出口：取消它即结束整个进程
	procCtx, procCancel := context.WithCancel(ctx)
	defer procCancel()
	st8.procParent = procCtx
	st8.procCancel = procCancel

	if setupErr == nil {
		// 已初始化但缺凭据 → startService 会 fail 并把原因显示在看板上，
		// 页面停留在控制台（而非向导），让人用「网盘凭据」卡片就地补。
		st8.launchService(procCtx)
	}

	// 进程生命周期 = 看板生命周期（配置失败也一直挂着，等人改）
	<-procCtx.Done()
	fmt.Println("已退出。")
	return nil
}

// printBanner -- 启动横幅。
//
// 只打印地址、**绝不自动打开浏览器**：双击场景下弹窗口很突兀，
// 而且服务化（nssm）后根本没有桌面会话。地址做成纯文本，终端里
// Ctrl+单击（Windows Terminal / VS Code / PowerShell 7）即可打开。
func printBanner(url string, initialized bool, setupErr error) {
	fmt.Println()
	fmt.Println("  ============================================================")
	fmt.Println("   bdsync 备份服务已启动")
	fmt.Println("  ============================================================")
	fmt.Printf("   服务地址： %s\n", url)
	fmt.Println("             （在终端里按住 Ctrl 单击上面的地址即可用浏览器打开）")
	switch {
	case !initialized:
		fmt.Println("   当前状态： 还没初始化 ---- 在网页里填「本地目录 / 网盘目录 / 凭据」")
		fmt.Println("             （三项就行；填错会留在页面上，不会退出）")
	case setupErr != nil:
		fmt.Println("   当前状态： 已初始化，但启动备份运行时失败 ----")
		for _, line := range strings.Split(setupErr.Error(), "\n") {
			fmt.Println("             " + strings.TrimRight(line, "\r"))
		}
		fmt.Println("             （在看板「配置 → 网盘凭据」里补录 cookie 即可恢复）")
	default:
		fmt.Println("   当前状态： 正在启动备份运行时...")
	}
	fmt.Println("   停止服务： 关闭本窗口 / 按 Ctrl+C")
	fmt.Println("  ============================================================")
	fmt.Println()
}

// isInitialized -- 是否已完成首次初始化。
//
// 判据（任一成立）：
//  1. 配置文件里 service.initialized: true（新版本向导写入的显式标记）
//  2. 老配置没有标记，但配置文件存在且 pair 齐全 ---- 那本来就是"配过了"
//
// 只问"填过没有"，不问"现在能不能跑起来"：cookie 过期是运行时问题，
// 由看板凭据卡片修复，不该让人重走一遍向导。
func isInitialized(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Service.Initialized {
		return true
	}
	return appCfgPath != "" && fileExists(appCfgPath) && cfg.ValidatePair() == nil
}

// firstLine -- 只取首行（横幅里一行一条，避免大段错误糊在一行）。

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// isAddrInUse -- 端口被占用（另一个实例已在跑）。
func isAddrInUse(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "only one usage of each socket address") ||
		strings.Contains(msg, "每个套接字地址") ||
		strings.Contains(msg, "address already in use")
}

// runWebEntry -- web 模式的统一入口（双击 exe 无参数时也走这里）。
func runWebEntry(args []string) {
	if err := cmdWeb(args); err != nil {
		fmt.Fprintln(os.Stderr, "[X]", err)
		// 双击场景：停下来让人读到错误（服务模式下绝不等按键，见 creds_api.go）
		fmt.Println()
		pauseBeforeExit()
		exitProcess(1) // 不用 os.Exit：转码管道要收尾（console_windows.go）
	}
}

func webAddrHint(cfg *config.Config) string {
	addr := cfg.Web.Addr
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr + "/"
}

// ── 向导状态机 ────────────────────────────────────────────────

// setupHub 由 cmdWeb 注入（向导阶段的日志也要进 SSE）。
var setupHub *web.Hub

type setupState struct {
	mu         sync.Mutex
	configPath string
	version    string

	// initialized -- 已完成首次初始化（配置文件 service.initialized 或老配置齐全）。
	// 为 true 时页面不再显示向导：cookie 过期只是运行时错误，用「凭据」卡片就地修。
	initialized bool

	// 运行态
	running bool
	rt      *runtimeState
	st      *index.Store
	stopSvc context.CancelFunc
	svcDone chan struct{} // startService 返回时关闭（重启用：等旧的退出干净）
	svcErr  error

	// 凭据（内存态，落盘由 creds.go 处理）
	cookie string

	// notice -- 需要跨页面刷新保留的提示（如自动分库说明）
	notice string

	// procParent -- 进程级上下文（Ctrl+C/SIGTERM）；每次 launchService 挂它下面
	procParent context.Context
	// procCancel -- 取消 procParent（看板「停止」按钮用：语义是结束进程）
	procCancel context.CancelFunc

	// 外层看板（向导阶段就已存在；运行时复用，避免二次监听同一端口）
	srv *web.Server
}

func (s *setupState) logf(format string, a ...any) {
	if setupHub != nil {
		setupHub.Append(fmt.Sprintf(format, a...))
	}
}

func (s *setupState) setNotice(msg string) {
	s.mu.Lock()
	s.notice = msg
	s.mu.Unlock()
}

// state -- /api/setup 的引导状态（页面据此决定显示向导还是控制台）。
func (s *setupState) state() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"version":         s.version,
		"config_path":     s.configPath,
		"has_config_file": s.configPath != "" && fileExists(s.configPath),
		"initialized":     s.initialized,
		"running":         s.running,
		"error":           errText(s.svcErr),
		"notice":          s.notice,
		"db_path":         appCfg.Service.DBPath,
		"root":            appCfg.Pair.LocalRoot,
		"remote":          appCfg.Pair.Remote,
		"cookie_env":      os.Getenv("BDSYNC_COOKIE") != "",
		"cookie_saved":    credentialsHint() != "",
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// control -- 服务未就绪时的控制面（停止可用，其余是安全的空操作）。
func (s *setupState) ctrl() web.Control { return &setupControl{s: s} }

type setupControl struct{ s *setupState }

func (c *setupControl) Pause() bool  { return false }
func (c *setupControl) Resume() bool { return false }
func (c *setupControl) Paused() bool { return false }

// Stop -- 「停止」按钮的语义是**结束进程**（页面上写的就是这句），
// 所以取消进程级上下文，而不是只停一次运行时：否则进程还活着、端口还被占着，
// nssm 会认为服务没停，双击场景下窗口也会留个僵尸挂着。
func (c *setupControl) Stop() {
	c.s.mu.Lock()
	cancel, stop := c.s.procCancel, c.s.stopSvc
	c.s.mu.Unlock()
	if cancel != nil {
		cancel()
		return
	}
	if stop != nil {
		stop()
	}
}

// status -- 服务未运行时给出"没在跑"的状态；运行后交给真实 provider。
//
// stopped 的取值刻意区分两件事：未初始化（还没配置，页面该显示向导）
// 与 已初始化但没起来（cookie 过期/磁盘满之类，页面该显示控制台 + 错误）。
func (s *setupState) status() any {
	s.mu.Lock()
	rt, running, svcErr, inited := s.rt, s.running, s.svcErr, s.initialized
	s.mu.Unlock()
	if running && rt != nil {
		return statusProvider(rt, s.st)()
	}
	return map[string]any{
		"setup":     !inited,
		"version":   s.version,
		"active":    false,
		"paused":    false,
		"stopped":   inited, // 已初始化却没跑 = 停了，不是"正在配置"
		"backend":   "",
		"error":     errText(svcErr),
		"root":      appCfg.Pair.LocalRoot,
		"remote":    appCfg.Pair.Remote,
		"db_path":   appCfg.Service.DBPath,
		"creds":     credsSource(),
		"pending":   map[string]any{"count": 0, "bytes": 0},
		"rss_bytes": liveHeap(),
	}
}

// ── 向导应用：校验 → 落配置 → 起运行时 ────────────────────────

func (s *setupState) apply(payload map[string]any) (string, error) {
	trace("apply: 进入")
	defer trace("apply: 返回")
	localRoot, _ := payload["local_root"].(string)
	remote, _ := payload["remote"].(string)
	remember, _ := payload["remember"].(bool)

	localRoot = strings.TrimSpace(localRoot)
	remote = strings.TrimSpace(remote)

	var miss []string
	if localRoot == "" {
		miss = append(miss, "本地目录")
	}
	if remote == "" {
		miss = append(miss, "远端目录")
	}
	cookie, warn, empty, cerr := cookieFromPayload(payload)
	if cerr != nil {
		return "", cerr
	}
	if empty {
		if c, _ := loadCookie(); c != "" {
			cookie = c // 环境变量 / 已记住的凭据
		} else {
			miss = append(miss, "网盘凭据")
		}
	}
	if len(miss) > 0 {
		return "", fmt.Errorf("还缺少：%s", strings.Join(miss, "、"))
	}
	if !strings.HasPrefix(remote, "/") {
		return "", errors.New("远端目录要以 / 开头，例如 /bdsync-backup")
	}
	if err := detector.ValidateLocalRoot(localRoot); err != nil {
		return "", err
	}

	cfg := config.Default()
	cfg.Pair.LocalRoot = localRoot
	cfg.Pair.Remote = remote
	// 向导只收必需项；并发/排除等高级项留给启动后的「配置」页
	// （仍兼容 API 直传，便于将来自动化）
	if v, ok := payload["concurrency"].(float64); ok && v > 0 {
		cfg.Sender.Concurrency = int(v)
	}
	if v, ok := payload["settle_seconds"].(float64); ok && v >= 0 {
		cfg.Sender.SettleDuration = config.DurationOf(time.Duration(v) * time.Second)
	}
	if ex, ok := payload["exclude"].([]any); ok {
		for _, e := range ex {
			if t := strings.TrimSpace(fmt.Sprint(e)); t != "" {
				cfg.Exclude = append(cfg.Exclude, t)
			}
		}
	}
	// 看板地址沿用当前进程实际监听的（否则重启后端口会漂）
	cfg.Web.Enabled = true
	cfg.Web.Addr = appCfg.Web.Addr
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	// 换目录 = 换索引：既有库绑的是别的本地目录时，自动另起一份新库
	// （旧库原地保留、绝不动数据；不该让新手去理解"请换一个 --db"）
	if note, err := splitDBForNewRoot(cfg, localRoot); err != nil {
		return "", err
	} else if note != "" {
		s.setNotice(note)
	}

	// 首次配置完成 → 打标记。下次启动直接进服务态，不再走向导。
	cfg.Service.Initialized = true

	// 凭据：先落内存（立即可用），再按用户意愿落盘
	trace("apply: 凭据处理前 remember=%v len=%d", remember, len(cookie))
	s.mu.Lock()
	s.cookie = cookie
	s.mu.Unlock()
	if remember {
		if err := saveCredentials(cookie); err != nil {
			return "", errCredsSaveFailed(err)
		}
	} else {
		warn = strings.TrimSpace(warn + " 凭据只在本次运行有效，重启后需重新填写。")
	}
	trace("apply: 凭据处理完成")

	// 落配置（写 exe 同目录的 bdsync.yaml）
	path := s.configPath
	if path == "" {
		path = defaultConfigPath()
	}
	trace("apply: 写配置 %s", path)
	if err := writeConfigFile(path, cfg); err != nil {
		return "", err
	}
	trace("apply: 配置写完")
	appCfg = cfg
	appCfgPath = path
	s.mu.Lock()
	s.initialized = true
	s.configPath = path
	s.mu.Unlock()

	// 起运行时（失败也不退出：页面停在向导 + 错误）
	if warn != "" {
		if setupHub != nil {
			setupHub.Append("[!] " + warn)
		}
	}
	s.launchService()
	return "配置已保存，正在启动备份...", nil
}

func defaultConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		return exe[:strings.LastIndexAny(exe, `\/`)+1] + "bdsync.yaml"
	}
	return "bdsync.yaml"
}

// startService -- 起备份运行时；错误回传页面（进程继续活着）。
func (s *setupState) startService(parent context.Context) {
	trace("startService: 进入")
	cfg := config.Clone(appCfg)
	if err := cfg.Validate(); err != nil {
		s.fail(err)
		return
	}
	if err := cfg.ValidatePair(); err != nil {
		s.fail(err)
		return
	}
	cookie, src := s.currentCookie()
	if cookie == "" {
		s.fail(errors.New("缺少网盘凭据：请在看板「配置 → 网盘凭据」里贴一条 cookie，或设置环境变量 BDSYNC_COOKIE"))
		return
	}
	setCookieForRunFrom(cookie, src)

	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.stopSvc = cancel
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		trace("runService goroutine: 开始")
		errCh <- runService(ctx, cancel, cfg, serviceOpts{
			AppID:  266719,
			Hub:    setupHub,
			WebSrv: s.srv, // 复用向导阶段就已监听的看板
			OnReady: func(rt *runtimeState, st *index.Store) {
				trace("OnReady: 控制面就绪")
				s.mu.Lock()
				s.rt, s.st = rt, st
				s.mu.Unlock()
			},
		})
	}()

	s.mu.Lock()
	s.running = true
	s.svcErr = nil
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		// 必须等 runService 真正退出（关索引库、关日志都在它的 defer 里）----
		// 否则 restartService 起的新实例会撞上 SQLite 写锁。
		// errCh 有 1 个缓冲，runService 收到 cancel 后一定会发，不会死锁。
		<-errCh
		s.mu.Lock()
		s.running = false
		s.rt, s.st = nil, nil
		s.mu.Unlock()
	case err := <-errCh:
		s.mu.Lock()
		s.rt, s.st = nil, nil
		s.mu.Unlock()
		s.fail(err)
	}
}

func (s *setupState) fail(err error) {
	s.mu.Lock()
	s.running = false
	if err != nil {
		s.svcErr = err
	}
	s.mu.Unlock()
	// err == nil 表示"正常停止"（信号/看板点停止）----既不是失败，也别覆盖已知错误
	if err != nil && setupHub != nil {
		setupHub.Append("[!] " + err.Error())
	}
}

// launchService -- 同步完成"起运行时"的状态布置，再交给 goroutine。
//
// 拆出来的原因：换 cookie 后要**等旧的完全退出**再起新的。startService 收到
// cancel 后还要走完 runService 的 defer（关索引库、关日志），这期间新进程起来
// 会撞上 SQLite 写锁 ---- 所以 svcDone 必须在启动前就建好，而不是等 goroutine
// 被调度到才创建。
//
// parent 是**进程级**生命周期上下文（Ctrl+C / SIGTERM）：向导里反复起停用的
// 每次独立 ctx 只挂在它下面，这样一次 Ctrl+C 无论当时处在第几次启动都能收干净。
func (s *setupState) launchService(parent ...context.Context) {
	ctx0 := context.Context(nil)
	if len(parent) > 0 {
		ctx0 = parent[0]
	}
	if ctx0 == nil {
		ctx0 = s.procCtx()
	}
	ctx, cancel := context.WithCancel(ctx0)
	done := make(chan struct{})
	s.mu.Lock()
	s.stopSvc = cancel
	s.svcDone = done
	s.mu.Unlock()
	go func() {
		defer close(done)
		s.startService(ctx)
	}()
}

// procCtx -- 进程级生命周期上下文（cmdWeb 注入）；未注入时退化为 Background。
func (s *setupState) procCtx() context.Context {
	s.mu.Lock()
	c := s.procParent
	s.mu.Unlock()
	if c == nil {
		return context.Background()
	}
	return c
}

// restartService -- 凭据更换后重建备份运行时（停旧 → 等干净 → 起新）。
//
// 为什么必须重启而非热替换：cookie 在 runService 里被固化进 baidu.Client，
// sender/mirror 持有它并在几十处无锁裸读。热替换要给整条链加锁换指针，
// 换来的只是省一次秒级重启 ---- 不划算。
func (s *setupState) restartService() error {
	s.mu.Lock()
	stop, done := s.stopSvc, s.svcDone
	s.mu.Unlock()

	if stop != nil {
		stop()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			return errors.New("旧的备份运行时 30s 内没停下来，请稍后重试")
		}
	}
	s.launchService()
	return nil
}

// currentCookie -- 当前生效的 cookie 及其来源。
// 顺序刻意跟 loadCookie 一致：本次录入 > 环境变量 > credentials.dat。
// 来源必须跟着值一起返回，否则 startService 只能一律标成 session，
// 冷启动从文件读回来的会被误报成"仅本次进程（未保存）"。
func (s *setupState) currentCookie() (cookie, src string) {
	s.mu.Lock()
	c := s.cookie
	s.mu.Unlock()
	if c != "" {
		return c, "session"
	}
	if v := os.Getenv("BDSYNC_COOKIE"); v != "" {
		return v, "env"
	}
	if v := os.Getenv("BDSYNC_BDUSS"); v != "" {
		if st := os.Getenv("BDSYNC_STOKEN"); st != "" {
			v += "; STOKEN=" + st
		}
		return v, "env"
	}
	if v := loadCredentials(); v != "" {
		return v, "file"
	}
	return "", ""
}

// setupHub 由 cmdWeb 注入（向导阶段的日志也要进 SSE）。

// splitDBForNewRoot -- 既有索引库绑的是别的本地目录时，自动改用一份新库。
//
// 一份索引库只对应一个备份对（这是有意的一致性约束）。既然向导里换目录
// 就意味着换一次备份，直接报错让人理解成本太高 ---- 这里代为分家：
// 新库名 index-<目录名>.db（冲突再加序号），旧库原地保留、一行不改。
func splitDBForNewRoot(cfg *config.Config, root string) (string, error) {
	dbPath := cfg.Service.DBPath
	if _, err := os.Stat(dbPath); err != nil {
		return "", nil // 还没有库 → 运行时自建
	}
	st, err := index.OpenExisting(dbPath)
	if err != nil {
		return "", nil // 库损坏：留给运行时报错，不在这里下结论
	}
	defer st.Close()
	bound, _, _, _, _, err := st.PairInfo()
	if err != nil || bound == "" || sameLocalPath(bound, root) {
		return "", nil
	}
	dir := filepath.Dir(dbPath)
	base := "index"
	if root != "" {
		if name := sanitizeName(filepath.Base(root)); name != "" {
			base = "index-" + name
		}
	}
	for i := 0; ; i++ {
		cand := filepath.Join(dir, base+".db")
		if i > 0 {
			cand = filepath.Join(dir, fmt.Sprintf("%s-%d.db", base, i))
		}
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			cfg.Service.DBPath = cand
			return fmt.Sprintf("检测到这份 bdsync 之前备份的是别的目录（%s），已为 %s 自动新建独立索引 %s。\n"+
				"原索引库原地保留、未做任何改动；要接着备份原目录，把上面的本地目录改回去即可。",
				bound, root, filepath.Base(cand)), nil
		}
		if i > 50 {
			return "", fmt.Errorf("无法为 %s 找到可用的索引库文件名（%s*.db 都已存在）", root, base)
		}
	}
}

// sanitizeName -- 文件名安全化（Windows 非法字符 → _）。
func sanitizeName(s string) string {
	repl := func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '_'
		}
		return r
	}
	out := strings.Map(repl, strings.TrimSpace(s))
	out = strings.Trim(out, ". ")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// probe -- 检测本地目录（只读遍历前两层，快速反馈路径是否填对）。
func (s *setupState) probe(root string) (int, int, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return 0, 0, errors.New("请先填写本地目录")
	}
	if err := detector.ValidateLocalRoot(root); err != nil {
		return 0, 0, err
	}
	var files, dirs int
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, 0, fmt.Errorf("无法读取目录：%w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs++
			sub, _ := os.ReadDir(filepath.Join(root, e.Name()))
			for _, se := range sub {
				if !se.IsDir() {
					files++
				}
			}
		} else {
			files++
		}
	}
	return files, dirs, nil
}

// ── 网盘凭据（cookie 过期时就地换，不必重走向导）─────────────────

// credsStatus -- /api/credentials GET。只有来源元信息，**不含凭据内容**。
func (s *setupState) credsStatus() map[string]any {
	s.mu.Lock()
	running := s.running
	locked := s.cookie != ""
	s.mu.Unlock()

	st := credsStatusPublic()
	st["running"] = running
	// 有 s.cookie 说明本次进程内录入过（可能还没落盘）----如实报 session
	if locked && st["source"] == "none" {
		st["source"] = "session"
		st["has"] = true
	}
	return st
}

// credsSave -- /api/credentials POST。
//
// 流程：解析并校验 → 内存生效 → 按 remember 落盘 → 重启运行时换上新 cookie。
// 顺序有讲究：先落内存再落盘，盘写失败时仍能用本次会话跑起来（报错但不卡死）；
// 重启放最后，前两步任一失败都不会动正在跑的东西。
func (s *setupState) credsSave(payload map[string]any) (map[string]any, error) {
	// 清除：只删本地凭据文件；环境变量来源不受影响，正在跑的进程也不动
	if clear, _ := payload["clear"].(bool); clear {
		deleteCredentials()
		return map[string]any{
			"message":   "已清除二进制同目录的 credentials.dat",
			"restarted": false,
		}, nil
	}

	cookie, warn, empty, err := cookieFromPayload(payload)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, errors.New("请粘贴新的 Cookie（或分别填 BDUSS / STOKEN）")
	}

	// remember 缺省按 true：看板默认勾选"记住"，少传字段时不能默默丢掉持久化
	remember := true
	if v, ok := payload["remember"].(bool); ok {
		remember = v
	}

	// 1) 内存立即生效（loadCookie 的 memCookie 分支）
	setCookieForRun(cookie)
	s.mu.Lock()
	s.cookie = cookie
	s.mu.Unlock()

	// 2) 持久化（明文 credentials.dat）
	if err := saveCookieToDisk(cookie, remember); err != nil {
		return nil, err
	}

	// 3) 重启运行时，把新 cookie 换进 baidu.Client
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()

	msg := "凭据已更新"
	if warn != "" {
		msg += "（提醒：" + warn + "）"
	}
	if remember {
		msg += "，已保存到 " + credentialsHint() + "（明文文件）"
	} else {
		msg += "，仅本次进程有效（未持久化）"
	}

	if running {
		if err := s.restartService(); err != nil {
			return map[string]any{
				"message":   msg + "；但重启失败：" + err.Error(),
				"restarted": false,
			}, nil
		}
		return map[string]any{"message": msg + "，备份运行时已用新凭据重启", "restarted": true}, nil
	}
	// 没在跑（多半是 cookie 过期导致上次没起来）→ 直接拉起来
	s.mu.Lock()
	inited := s.initialized
	s.mu.Unlock()
	if inited {
		s.launchService()
		return map[string]any{"message": msg + "，正在启动备份运行时", "restarted": true}, nil
	}
	return map[string]any{"message": msg + "（还没完成首次配置，进入向导后生效）", "restarted": false}, nil
}

// ── 配置页（运行态的增删改）──────────────────────────────────

func (s *setupState) configGet() (string, map[string]any, []string) {
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	if rt != nil {
		return configGet(rt)
	}
	yamlText, _ := os.ReadFile(defaultConfigPath())
	return string(yamlText), map[string]any{
		"poll_interval":  appCfg.Detector.PollInterval.String(),
		"flush_interval": appCfg.Sender.FlushInterval.String(),
		"settle":         appCfg.Sender.SettleDuration.String(),
		"concurrency":    appCfg.Sender.Concurrency,
		"idle_threshold": appCfg.Quota.IdleThreshold.String(),
		"exclude":        appCfg.Exclude,
	}, nil
}

func (s *setupState) configSave(payload map[string]any) ([]string, []string, error) {
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	if rt != nil {
		return configSave(rt, payload)
	}
	// 未运行：仅落盘，等下次启动生效
	cfg := config.Clone(appCfg)
	if y, ok := payload["yaml"].(string); ok {
		nc, err := config.Parse([]byte(y))
		if err != nil {
			return nil, nil, err
		}
		cfg = nc
	}
	if err := writeConfigFile(defaultConfigPath(), cfg); err != nil {
		return nil, nil, err
	}
	appCfg = cfg
	return []string{"配置文件已保存（重启后生效）"}, nil, nil
}
