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
	"syscall"
	"time"

	"bdsync/internal/config"
	"bdsync/internal/detector"
	"bdsync/internal/index"
	"bdsync/internal/web"
)

// trace — 诊断用分步追踪（设 BDSYNC_TRACE=<文件> 开启）。
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

// cmdWeb — 双击入口：先起看板，再谈配置。
//
// 设计意图（降低门槛）：
//
//	零配置双击 → 进程不退出，127.0.0.1 看板一定起来 → 控制台打印地址
//	  → 网页向导填「本地目录 / 远端目录 / 凭据」
//	    → 校验通过：写 bdsync.yaml，启动备份运行时（和 daemon 完全同一套代码）
//	    → 校验失败：页面停在向导并显示原因，人改完再点一次
//
// 配置失败绝不静默退出：那正是新手最容易卡住的地方。
func cmdWeb(args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	addr := fs.String("addr", "", "看板地址（默认取配置 web.addr，回环地址）")
	noBrowser := fs.Bool("no-browser", false, "不自动打开浏览器")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// ── 1. 尽量加载配置；失败不致命，转入向导模式 ──
	eff := config.Clone(appCfg)
	setupErr := error(nil)
	if err := eff.Validate(); err != nil {
		setupErr = err
	} else if err := eff.ValidatePair(); err != nil {
		setupErr = err
	} else if _, cerr := loadCookie(); cerr != nil {
		setupErr = cerr
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
		configPath: appCfgPath,
		version:    version,
	}
	srv := web.New(eff.Web.Addr, hub, st8.status, st8.ctrl(), web.ConfigIO{
		Get:  func() (string, map[string]any, []string) { return st8.configGet() },
		Save: func(p map[string]any) ([]string, []string, error) { return st8.configSave(p) },
	}).WithSetup(web.SetupIO{
		State: st8.state,
		Apply: st8.apply,
		Probe: st8.probe,
	})
	if err := srv.Start(); err != nil {
		// 端口已被占用 = 大概率已经有一个实例在跑：给一句人话 + 直接开看板
		if isAddrInUse(err) {
			fmt.Println()
			fmt.Println("  已经有 bdsync 在运行（看板端口被占用）。")
			fmt.Printf("  直接打开： %s\n", webAddrHint(eff))
			fmt.Println()
			if !*noBrowser {
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

	fmt.Println()
	fmt.Println("  ============================================================")
	fmt.Println("   bdsync 备份服务已启动")
	fmt.Println("  ============================================================")
	fmt.Printf("   看板地址： %s\n", srv.URL())
	if setupErr != nil {
		fmt.Println("   当前状态： 还没配置 —— 请在网页里填「本地目录 / 网盘目录 / 凭据」")
		fmt.Println("             （三项就行；填错会留在页面上，不会退出）")
	} else {
		fmt.Println("   当前状态： 正在启动备份运行时…")
	}
	fmt.Println("   停止服务： 关闭本窗口 / 按 Ctrl+C")
	fmt.Println("  ============================================================")
	fmt.Println()
	if !*noBrowser {
		go func() {
			time.Sleep(600 * time.Millisecond) // 等看板完全就绪再开
			openBrowser(srv.URL())
		}()
	}

	st8.srv = srv
	setupHub = hub
	st8.logf("[看板] %s", srv.URL())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if setupErr == nil {
		go st8.startService(ctx)
	}

	// 进程生命周期 = 看板生命周期（配置失败也一直挂着，等人改）
	<-ctx.Done()
	fmt.Println("已退出。")
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// isAddrInUse — 端口被占用（另一个实例已在跑）。
func isAddrInUse(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "only one usage of each socket address") ||
		strings.Contains(msg, "每个套接字地址") ||
		strings.Contains(msg, "address already in use")
}

// runWebEntry — web 模式的统一入口（双击 exe 无参数时也走这里）。
func runWebEntry(args []string) {
	if err := cmdWeb(args); err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		// 不立刻退出：让用户能读到错误（双击场景没有终端可回看）
		fmt.Println()
		fmt.Println("  启动失败，按任意键关闭窗口…")
		fmt.Scanln()
		os.Exit(1)
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

	// 运行态
	running bool
	rt      *runtimeState
	st      *index.Store
	stopSvc context.CancelFunc
	svcErr  error

	// 凭据（内存态，DPAPI 持久化由 creds_*.go 处理）
	cookie string

	// notice — 需要跨页面刷新保留的提示（如自动分库说明）
	notice string

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

// state — /api/setup 的引导状态（页面据此决定显示向导还是控制台）。
func (s *setupState) state() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"version":         s.version,
		"config_path":     s.configPath,
		"has_config_file": s.configPath != "" && fileExists(s.configPath),
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

// control — 服务未就绪时的控制面（停止可用，其余是安全的空操作）。
func (s *setupState) ctrl() web.Control { return &setupControl{s: s} }

type setupControl struct{ s *setupState }

func (c *setupControl) Pause() bool  { return false }
func (c *setupControl) Resume() bool { return false }
func (c *setupControl) Paused() bool { return false }
func (c *setupControl) Stop() {
	if c.s.stopSvc != nil {
		c.s.stopSvc()
	}
}

// status — 服务未运行时给出"向导模式"状态；运行后交给真实 provider。
func (s *setupState) status() any {
	s.mu.Lock()
	rt, running, svcErr := s.rt, s.running, s.svcErr
	s.mu.Unlock()
	if running && rt != nil {
		return statusProvider(rt, s.st)()
	}
	return map[string]any{
		"setup":     true,
		"version":   s.version,
		"active":    false,
		"paused":    false,
		"stopped":   false,
		"backend":   "",
		"error":     errText(svcErr),
		"root":      appCfg.Pair.LocalRoot,
		"remote":    appCfg.Pair.Remote,
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
	rawCookie, _ := payload["cookie"].(string)
	bduss, _ := payload["bduss"].(string)
	stoken, _ := payload["stoken"].(string)
	remember, _ := payload["remember"].(bool)

	localRoot = strings.TrimSpace(localRoot)
	remote = strings.TrimSpace(remote)
	rawCookie = strings.TrimSpace(rawCookie)

	var miss []string
	if localRoot == "" {
		miss = append(miss, "本地目录")
	}
	if remote == "" {
		miss = append(miss, "远端目录")
	}
	cookie := ""
	warn := ""
	switch {
	case rawCookie != "":
		// 直接粘贴的 Cookie 串（推荐路径）
		if !strings.Contains(rawCookie, "BDUSS=") {
			return "", errors.New("Cookie 串里没有 BDUSS=…，请确认复制完整")
		}
		cookie = rawCookie
		if !strings.Contains(strings.ToUpper(rawCookie), "STOKEN=") {
			// 实测：只有 BDUSS 时网盘列目录会被拒（-6 请重新登录）——提示但不阻断
			warn = "提醒：这串 Cookie 里没有 STOKEN，网盘很可能拒绝列目录（实测如此）。"
		}
	case strings.TrimSpace(bduss) != "":
		cookie = "BDUSS=" + strings.TrimSpace(bduss)
		if s := strings.TrimSpace(stoken); s != "" {
			cookie += "; STOKEN=" + s
		} else {
			warn = "提醒：没填 STOKEN，网盘很可能拒绝列目录（实测如此）。"
		}
	default:
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

	// 凭据：先落内存（立即可用），再按用户意愿 DPAPI 加密持久化
	trace("apply: 凭据处理前 remember=%v len=%d", remember, len(cookie))
	s.mu.Lock()
	s.cookie = cookie
	s.mu.Unlock()
	if remember {
		if err := saveCredentialsDPAPI(cookie); err != nil {
			return "", fmt.Errorf("保存凭据失败：%w（可取消勾选『记住凭据』，仅本次会话有效）", err)
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

	// 起运行时（失败也不退出：页面停在向导 + 错误）
	if warn != "" {
		if setupHub != nil {
			setupHub.Append("[!] " + warn)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.stopSvc = cancel
	s.mu.Unlock()
	go s.startService(ctx)
	return "配置已保存，正在启动备份…", nil
}

func defaultConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		return exe[:strings.LastIndexAny(exe, `\/`)+1] + "bdsync.yaml"
	}
	return "bdsync.yaml"
}

// startService — 起备份运行时；错误回传页面（进程继续活着）。
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
	cookie := s.currentCookie()
	if cookie == "" {
		s.fail(errors.New("缺少网盘凭据：请在向导中填写 BDUSS/STOKEN"))
		return
	}
	setCookieForRun(cookie)

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
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	case err := <-errCh:
		s.fail(err)
	}
}

func (s *setupState) fail(err error) {
	s.mu.Lock()
	s.running = false
	s.svcErr = err
	s.mu.Unlock()
	if setupHub != nil {
		setupHub.Append("[!] " + err.Error())
	}
}

func (s *setupState) currentCookie() string {
	s.mu.Lock()
	c := s.cookie
	s.mu.Unlock()
	if c != "" {
		return c
	}
	env, _ := loadCookie()
	return env
}

// setupHub 由 cmdWeb 注入（向导阶段的日志也要进 SSE）。

// splitDBForNewRoot — 既有索引库绑的是别的本地目录时，自动改用一份新库。
//
// 一份索引库只对应一个备份对（这是有意的一致性约束）。既然向导里换目录
// 就意味着换一次备份，直接报错让人理解成本太高 —— 这里代为分家：
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

// sanitizeName — 文件名安全化（Windows 非法字符 → _）。
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

// probe — 检测本地目录（只读遍历前两层，快速反馈路径是否填对）。
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
