package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"bdsync/internal/baidu"
	"bdsync/internal/config"
	"bdsync/internal/detector"
	"bdsync/internal/index"
	"bdsync/internal/logx"
	"bdsync/internal/power"
	"bdsync/internal/sender"
	"bdsync/web"
)

// version -- 运行时版本号。
//
// 优先取构建期注入的 -ldflags "-X main.version=..."（CI 按 git tag 注入，
// 见 .github/workflows/release.yml）；开发态没注入时退回下面的默认值，
// 保证 `go build` / `go run` 不带任何 flag 也能跑。
//
// 用 var 而非 const：const 无法被链接器覆写（-X 只对 string 类型的 var 生效）。
var version = defaultVersion

// defaultVersion -- 未注入时的兜底版本号（开发态）。
// 发布构建由 CI 用 -ldflags "-X main.version=$TAG" 覆盖成真实 tag，
// 所以这里只需要跟得上节奏，别让本地 go build 出来的 exe 显示过时的版本。
const defaultVersion = "v0.7.2"

// quietHeartbeat -- 静默期心跳间隔。
//
// 为什么需要：P2 要求"无变更时绝对空闲"，所以真正常驻的实例日志会长时间没有新行
// （实测出现过 103 分钟的静默段）。这在设计上是正确的，但只看日志文件无法区分
// 「安静地活着」和「卡死/被杀」----两者长得一模一样。
//
// 折中：**只在 QUIET/暂停时打**，有活干时一行都不多打，保住 P2 的低噪声目标；
// 一旦进入静默，就每 quietHeartbeat 留一个活体证据。
//
// 30 分钟对观察者太长（要等半小时才能确认一次）。改成 10 分钟：一天 144 行、
// 约 15KB，相对 10MB 轮转上限可忽略，仍远低于"刷屏"。
const quietHeartbeat = 10 * time.Minute

// activity -- 上次扫描/排空的摘要（看板展示）。
type activity struct {
	At      time.Time
	Summary string
}

// runtimeState -- daemon 运行态：配置快照、后端实例、状态机标志、控制面。
// web 层只通过它的方法读写，不感知 daemon 循环细节。
type runtimeState struct {
	mu        sync.RWMutex
	cfg       *config.Config // 生效配置（结构性字段仅展示，改动需重启）
	poll      *detector.Poll
	exclude   []string
	paused    bool
	active    bool
	wake      bool // 恢复运行后立即检测
	lastScan  *activity
	lastDrain *activity

	// 启动后不变
	stop       context.CancelFunc
	localRoot  string
	remoteRoot string
	backend    string
	// degraded -- usn 打不开/失效后已降级 poll（design 硬规矩1)）。
	// 存在 runtimeState 里而非只当循环局部变量：rt.backend 是启动时的快照，
	// 降级后不再变，日志/看板会一直显示 "usn" ---- 排障时会指向错误的后端。
	degraded  atomic.Bool
	schemaVer int
	startedAt time.Time
	quietCap  int
	stopping  atomic.Bool
}

type rtSnapshot struct {
	cfg       *config.Config
	poll      *detector.Poll
	exclude   []string
	paused    bool
	active    bool
	degraded  bool
	lastScan  *activity
	lastDrain *activity
}

// backendName -- 展示用后端名。降级后必须显示 poll，否则日志/看板会把
// 故障指向一个实际没在跑的后端（design §4.9：usn 失效 → 降级 poll）。
func (r *runtimeState) backendName() string {
	if r.backend != "usn" {
		return r.backend
	}
	if r.degraded.Load() {
		return "poll（usn 已降级）"
	}
	return "usn"
}

func (r *runtimeState) snap() rtSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return rtSnapshot{
		cfg: r.cfg, poll: r.poll, exclude: r.exclude,
		paused: r.paused, active: r.active,
		degraded: r.degraded.Load(),
		lastScan: r.lastScan, lastDrain: r.lastDrain,
	}
}

func (r *runtimeState) setPaused(v bool) {
	r.mu.Lock()
	r.paused = v
	if !v {
		r.wake = true // 恢复 → 下个 tick 立即检测
	}
	r.mu.Unlock()
}

func (r *runtimeState) consumeWake() bool {
	r.mu.Lock()
	w := r.wake
	r.wake = false
	r.mu.Unlock()
	return w
}

func (r *runtimeState) setActive(v bool) {
	r.mu.Lock()
	r.active = v
	r.mu.Unlock()
}

func (r *runtimeState) setLastScan(a *activity) {
	r.mu.Lock()
	r.lastScan = a
	r.mu.Unlock()
}

func (r *runtimeState) setLastDrain(a *activity) {
	r.mu.Lock()
	r.lastDrain = a
	r.mu.Unlock()
}

// applyLive -- 把保存的配置中的**可热更新子集**落进运行态（结构性字段不采纳，
// 由调用方把它们列为"需重启"），并按新排除规则重建 poll 实例。
func (r *runtimeState) applyLive(next *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.cfg
	// detector 间隔
	cur.Detector.PollInterval = next.Detector.PollInterval
	cur.Detector.UsnPollQuiet = next.Detector.UsnPollQuiet
	cur.Detector.UsnPollActive = next.Detector.UsnPollActive
	cur.Detector.ReconcileInterval = next.Detector.ReconcileInterval
	// sender
	cur.Sender = next.Sender
	// quota
	cur.Quota = next.Quota
	// exclude（重建 poll）
	cur.Exclude = append([]string(nil), next.Exclude...)
	r.cfg = &cur
	r.exclude = cur.Exclude
	r.poll = detector.NewPoll(r.localRoot, rPollStore, detector.Options{
		BatchSize: 2000, Exclude: r.exclude,
	})
}

// rPollStore -- applyLive 里重建 poll 所需的索引库（由 cmdDaemon 启动时注入）。
var rPollStore *index.Store

// ── web.Control 实现 ──
func (r *runtimeState) Pause() bool {
	if r.stopping.Load() {
		return false
	}
	r.setPaused(true)
	return true
}

func (r *runtimeState) Resume() bool {
	if r.stopping.Load() {
		return false
	}
	r.setPaused(false)
	return true
}

func (r *runtimeState) Stop() {
	if r.stopping.Swap(true) {
		return
	}
	if r.stop != nil {
		r.stop()
	}
}

func (r *runtimeState) Paused() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.paused
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	webEnable := fs.Bool("web", false, "启用本地看板（也可在配置 web.enabled 开启）")
	dbPath := fs.String("db", defaultDB(), "SQLite 索引文件")
	backend := fs.String("backend", appCfg.Detector.Mode, "检测后端: auto | usn | poll")
	excludeFlag := fs.String("exclude", "", "排除规则（逗号分隔）；默认取配置文件 exclude")
	appID := fs.Int("app-id", baidu.DefaultAppID, "百度 app_id")
	// 间隔/性能参数（默认值来自配置文件）
	pollInterval := fs.Duration("poll-interval", time.Duration(appCfg.Detector.PollInterval.Nanos()), "poll 后端扫描间隔")
	quietPoll := fs.Duration("quiet-poll", time.Duration(appCfg.Detector.UsnPollQuiet.Nanos()), "usn QUIET 态轮询间隔")
	activePoll := fs.Duration("active-poll", time.Duration(appCfg.Detector.UsnPollActive.Nanos()), "usn ACTIVE 态轮询间隔")
	reconcile := fs.Duration("reconcile", time.Duration(appCfg.Detector.ReconcileInterval.Nanos()), "强制全量对账周期")
	flushInterval := fs.Duration("flush-interval", time.Duration(appCfg.Sender.FlushInterval.Nanos()), "drain 最小间隔")
	flushBytes := fs.Int64("flush-bytes", appCfg.Sender.FlushBytes.Int64(), "drain 高水位（字节）")
	minGap := fs.Duration("min-gap", time.Duration(appCfg.Sender.MinGap.Nanos()), "两轮 drain 间隔")
	settle := fs.Duration("settle", time.Duration(appCfg.Sender.SettleDuration.Nanos()), "稳定性闸门时长")
	concurrency := fs.Int("concurrency", appCfg.Sender.Concurrency, "最大上传并发（AIMD 上限）")
	policy := fs.String("policy", appCfg.Sender.Policy, "同名文件策略")
	idleThreshold := fs.Duration("idle-threshold", time.Duration(appCfg.Quota.IdleThreshold.Nanos()), "静默多久回 QUIET")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Go 运行时调优（design §4.6：高 GC 阈值 + 软内存上限，绝不完全关 GC）
	debug.SetGCPercent(400)
	debug.SetMemoryLimit(256 << 20)
	_ = runtime.NumCPU()

	// 生效配置 = 配置文件 + 命令行覆盖
	eff := config.Clone(appCfg)
	eff.Service.DBPath = *dbPath
	eff.Detector.Mode = *backend
	eff.Detector.PollInterval = durCfg(*pollInterval)
	eff.Detector.UsnPollQuiet = durCfg(*quietPoll)
	eff.Detector.UsnPollActive = durCfg(*activePoll)
	eff.Detector.ReconcileInterval = durCfg(*reconcile)
	eff.Sender.FlushInterval = durCfg(*flushInterval)
	eff.Sender.FlushBytes = config.ByteSize(*flushBytes)
	eff.Sender.MinGap = durCfg(*minGap)
	eff.Sender.SettleDuration = durCfg(*settle)
	eff.Sender.Concurrency = *concurrency
	eff.Sender.Policy = *policy
	eff.Quota.IdleThreshold = durCfg(*idleThreshold)
	if strings.TrimSpace(*excludeFlag) != "" {
		eff.Exclude = excludeFrom(*excludeFlag)
	}
	if *webEnable {
		eff.Web.Enabled = true
	}
	if err := eff.Validate(); err != nil {
		return err
	}
	// 配置驱动：备份对必须显式声明（缺任一项 → 拒绝启动）
	if err := eff.ValidatePair(); err != nil {
		return err
	}

	ctx, cancelAll := context.WithCancel(context.Background())
	defer cancelAll()
	ctx, stop := signal.NotifyContext(ctx, stopSignals()...)
	defer stop()

	if eff.Web.Enabled {
		fmt.Printf("服务已启动到 %s （按住 Ctrl 单击即可用浏览器打开）\n", webAddrHint(eff))
	}
	return runService(ctx, stop, eff, serviceOpts{AppID: *appID})
}

// serviceOpts -- runService 的可注入项（web 模式复用同一套运行时，
// 但看板要先于配置就绪而存在）。
type serviceOpts struct {
	AppID   int
	Hub     *web.Hub    // 复用外部 hub（web 模式：向导阶段也要有日志流）
	WebSrv  *web.Server // 复用外部已启动的看板（web 模式：避免二次监听同一端口）
	OnReady func(rt *runtimeState, st *index.Store)
}

// runService -- 备份运行时主体：索引库 → 后端 → Sender → 看板 → 检测/排空循环。
// daemon 与 web（配置完成后）都走这里，保证两条入口行为完全一致。
func runService(ctx context.Context, stop context.CancelFunc, eff *config.Config, opts serviceOpts) error {
	trace("runService: 打开索引 %s (root=%s)", eff.Service.DBPath, eff.Pair.LocalRoot)
	defer trace("runService: 退出")
	appID := opts.AppID
	if appID == 0 {
		appID = baidu.DefaultAppID
	}
	// 索引库就地部署在配置（=二进制）旁；不存在则按配置新建并绑定 pair
	st, err := index.Open(eff.Service.DBPath, eff.Pair.LocalRoot)
	if err != nil {
		return err
	}
	defer st.Close()
	rPollStore = st

	localRoot, remoteRoot := eff.Pair.LocalRoot, eff.Pair.Remote
	boundRoot, boundRemote, _, schemaVer, _, err := st.PairInfo()
	if err != nil {
		return err
	}
	if boundRoot != "" && !sameLocalPath(boundRoot, localRoot) {
		return fmt.Errorf("索引库已绑定本地根目录 %s，与配置的 pair.local_root %s 不符\n"+
			"  换同步目标请换一个 db_path（或换一个 --db），本工具不会删除已有索引。",
			boundRoot, localRoot)
	}
	if boundRemote != "" && !sameRemotePath(boundRemote, remoteRoot) {
		return fmt.Errorf("索引库已绑定远端目录 %s，与配置的 pair.remote %s 不符\n"+
			"  这通常意味着换了备份目的地：请同步修改配置或换一份索引库。\n"+
			"  （不会动网盘上已有的任何文件）", boundRemote, remoteRoot)
	}
	if boundRemote == "" {
		if err := st.SetRemoteRoot(ctx, remoteRoot); err != nil {
			return err
		}
	}
	if err := detector.ValidateLocalRoot(localRoot); err != nil {
		return err
	}
	if err := detector.ValidateNoLocalHistory(localRoot, eff.Mirror.HistoryDir); err != nil {
		return err
	}

	// 后端选择：Windows 本地 NTFS → usn；否则 poll（design §4.1 auto 规则）
	useUsn := false
	switch eff.Detector.Mode {
	case "usn":
		if !detector.IsNTFS(localRoot) {
			return errors.New("usn 后端仅支持 Windows 本地 NTFS（design §4.1）")
		}
		useUsn = true
	case "poll":
	case "", "auto":
		useUsn = detector.IsNTFS(localRoot)
	default:
		return fmt.Errorf("未知后端: %s", eff.Detector.Mode)
	}
	mode := "poll"
	if useUsn {
		mode = "usn"
	}
	if err := st.SetDetectorMode(mode); err != nil {
		return err
	}

	cookie, err := loadCookie()
	if err != nil {
		return err
	}
	bc, err := baidu.New(appID, cookie)
	if err != nil {
		return err
	}

	// 日志：文件轮转（design §4.8）+ SSE 看板事件源
	lg, err := logx.New(eff.Service.LogFile, eff.Service.LogMaxMB, eff.Service.LogKeep)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	defer lg.Close()
	hub := opts.Hub
	if hub == nil {
		hub = web.NewHub()
	}
	logf := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		logx.Logf(lg, true, "%s", line)
		hub.Append(line)
	}

	power.ConfigureCaps(eff.Quota.QuietCPUCap, eff.Quota.ActiveCPUCap)

	snd := sender.New(st, bc, sender.Options{
		LocalRoot:   localRoot,
		RemoteRoot:  remoteRoot,
		Policy:      eff.Sender.Policy,
		Settle:      time.Duration(eff.Sender.SettleDuration.Nanos()),
		Concurrency: eff.Sender.Concurrency,
		MirrorOn:    eff.Mirror.Enabled,
		HistoryDir:  eff.Mirror.HistoryDir,
		HistoryDays: eff.Mirror.HistoryDays,
		OnRename:    eff.Mirror.OnRename,
		Logf:        logf,
	})
	poll := detector.NewPoll(localRoot, st, detector.Options{BatchSize: 2000, Exclude: eff.Exclude})

	if err := power.InitJob(); err != nil {
		logf("[!] Job Object 不可用（CPU 硬上限失效，仅靠优先级）: %v", err)
	}
	power.SetQuiet()

	rt := &runtimeState{
		cfg: eff, poll: poll, exclude: eff.Exclude,
		stop: stop, localRoot: localRoot, remoteRoot: remoteRoot,
		backend: mode, schemaVer: schemaVer, startedAt: time.Now(),
		quietCap: eff.Quota.QuietCPUCap,
	}
	if opts.OnReady != nil {
		opts.OnReady(rt, st)
	}

	// ── 本地看板（design §13：127.0.0.1、默认关闭）──
	webSrv := opts.WebSrv
	if webSrv == nil && eff.Web.Enabled {
		webSrv = web.New(eff.Web.Addr, hub, statusProvider(rt, st), rt, web.ConfigIO{
			Get: func() (string, map[string]any, []string) {
				return configGet(rt)
			},
			Save: func(payload map[string]any) ([]string, []string, error) {
				return configSave(rt, payload)
			},
		}).WithCreds(daemonCreds())
		if err := webSrv.Start(); err != nil {
			return err
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = webSrv.Stop(sctx)
		}()
		logf("[看板] %s", webSrv.URL())
	}

	logf("daemon 启动  后端=%s  本地=%s → 网盘=%s", mode, localRoot, remoteRoot)
	logf("  调度: flush_interval=%s 高水位=%s min_gap=%s | 对账=%s | 并发<=%d settle=%s",
		eff.Sender.FlushInterval, humanBytes(eff.Sender.FlushBytes.Int64()), eff.Sender.MinGap,
		eff.Detector.ReconcileInterval, eff.Sender.Concurrency, eff.Sender.SettleDuration)
	if len(eff.Exclude) > 0 {
		logf("  排除: %s", strings.Join(eff.Exclude, ", "))
	}
	if eff.Mirror.Enabled {
		logf("  镜像: 开启（.history/%d 天，on_rename=%s，远端对账 %s）----修改/删除会先归档旧版再动主树",
			eff.Mirror.HistoryDays, eff.Mirror.OnRename, eff.Mirror.RemoteReconcile)
	} else {
		logf("  镜像: 关闭（design 原始备份语义：不传播删除）")
	}
	if eff.Service.LogFile != "" {
		logf("  日志: %s（<=%dMB x %d 份轮转）", eff.Service.LogFile, eff.Service.LogMaxMB, eff.Service.LogKeep)
	}

	// 启动即全量对账：排除规则立即生效、dir_ref/游标引导（poll 成本 ~0.5s）
	runFullScan(ctx, rt, localRoot, "启动对账")

	// 启动即远端清单对账（镜像模式；design镜像 Q2：清单外 → 归档）
	rrInterval := time.Duration(eff.Mirror.RemoteReconcile.Nanos())
	if eff.Mirror.Enabled && rrInterval > 0 {
		if rres, rerr := snd.ReconcileRemote(ctx, false); rerr != nil {
			logf("[对账] 启动对账失败（按周期重试）: %v", rerr)
		} else {
			logf("[对账-启动] %s", rres.Summary())
		}
	}

	var (
		lastChange          = time.Now()
		lastDetect          time.Time
		lastDrainEnd        = time.Now().Add(-time.Duration(eff.Sender.FlushInterval.Nanos()))
		nextReconcile       = time.Now().Add(time.Duration(eff.Detector.ReconcileInterval.Nanos()))
		lastRemoteReconcile = time.Now()
		degraded            bool // usn 打不开/失效 → 本进程内降级 poll（design 硬规矩1)）
		lastBeat            = time.Now()
		quietSince          time.Time // 零值 = 当前不是静默态
	)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			logf("daemon 退出（停止请求/信号）")
			return nil
		case now := <-tick.C:
			snap := rt.snap()
			if rt.consumeWake() {
				lastDetect = time.Time{} // 恢复后立即检测
				lastChange = now
			}

			// ── 静默期心跳（放在 paused 早退之前：暂停时同样需要活体证据）──
			// 静默 = 非 ACTIVE，即 QUIET（空闲）或 PAUSED（用户暂停）。两者都会长时间
			// 没有新行，只看日志无法区分"安静地活着"和"卡死"。resume 或有活干时
			// 立刻清零，保证"忙的时候一行都不多打"（design P2）。
			if snap.active {
				quietSince = time.Time{}
				lastBeat = now
			} else if quietSince.IsZero() {
				quietSince = now
				lastBeat = now
			}
			if !quietSince.IsZero() && now.Sub(lastBeat) >= quietHeartbeat {
				lastBeat = now
				rss, _, memOK := processMem()
				mem := "--"
				if memOK {
					mem = humanBytes(rss)
				}
				elapsed := now.Sub(quietSince).Round(time.Minute)
				uptime := now.Sub(rt.startedAt).Round(time.Minute)
				if pend, perr := st.PendingStats(ctx); perr == nil {
					state, suffix := "QUIET", ""
					if snap.paused {
						state, suffix = "PAUSED", "（已暂停）"
					}
					logf("[心跳] %s%s：静默 %s - 队列 %d 项（%s）- RSS %s - 后端 %s - 已运行 %s",
						state, suffix, elapsed,
						pend.Count+pend.History, humanBytes(pend.Bytes), mem, rt.backendName(), uptime)
				}
			}

			if snap.paused {
				continue // 暂停：不检测、不排空（控制由看板/信号触发）
			}
			cfg := snap.cfg

			// ── 强制对账（design §4.9 兜底）──
			if now.After(nextReconcile) {
				if ok := runFullScan(ctx, rt, localRoot, "周期对账"); ok {
					lastChange = now
					markActive(rt, &lastChange, logf)
				}
				nextReconcile = now.Add(time.Duration(cfg.Detector.ReconcileInterval.Nanos()))
			}

			// ── 检测 ──
			interval := time.Duration(cfg.Detector.PollInterval.Nanos())
			if useUsn && !degraded {
				if snap.active {
					interval = time.Duration(cfg.Detector.UsnPollActive.Nanos())
				} else {
					interval = time.Duration(cfg.Detector.UsnPollQuiet.Nanos())
				}
			}
			if now.Sub(lastDetect) >= interval {
				lastDetect = now
				changed := false
				if useUsn && !degraded {
					res, needFull, err := detector.RunUsnCycle(ctx, localRoot, st, snap.exclude)
					switch {
					case err != nil:
						logf("[!] usn 后端失效，降级 poll（本进程内不再尝试 usn）: %v", err)
						degraded = true
						rt.degraded.Store(true) // 同步给 runtimeState，否则日志/看板仍显示 usn
					case needFull:
						logf("[!] usn 游标失效/journal 重建（正常路径）→ 全量扫描")
						changed = runFullScan(ctx, rt, localRoot, "游标失效")
						nextReconcile = now.Add(time.Hour) // 自适应压到 1h（design §4.9）
					default:
						if res.AddedFiles+res.Modified+res.Removed+res.Resurrected > 0 {
							changed = true
							logf("[usn] 变更 %d 新增 / %d 修改 / %d 删除（%s）",
								res.AddedFiles, res.Modified, res.Removed, msHuman(res.WallMs))
							rt.setLastScan(&activity{At: now, Summary: fmt.Sprintf(
								"usn 新增 %d / 修改 %d / 删除 %d", res.AddedFiles, res.Modified, res.Removed)})
						}
					}
				} else {
					changed = runFullScan(ctx, rt, localRoot, "")
				}
				if changed {
					markActive(rt, &lastChange, logf)
				}
			}

			// ── 远端清单对账（镜像模式；启动后按 mirror.remote_reconcile 周期）──
			if cfg.Mirror.Enabled && rrInterval > 0 && now.Sub(lastRemoteReconcile) >= rrInterval {
				lastRemoteReconcile = now
				if rres, rerr := snd.ReconcileRemote(ctx, false); rerr != nil {
					logf("[对账] 失败（下轮重试）: %v", rerr)
				} else {
					logf("[对账] %s", rres.Summary())
					if rres.ArchivedFiles+rres.ArchivedDirs+rres.MissingDirs > 0 {
						markActive(rt, &lastChange, logf)
					}
				}
			}

			// ── 状态衰减（滞回，design §4.4）──
			if snap.active && now.Sub(lastChange) > time.Duration(cfg.Quota.IdleThreshold.Nanos()) {
				if pend, perr := st.PendingStats(ctx); perr == nil && pend.Count == 0 && pend.History == 0 {
					rt.setActive(false)
					power.SetQuiet()
					power.TrimMemory() // 进入 QUIET 才修剪（design §4.4）
					quietSince = now   // 心跳计时从这一刻起算
					lastBeat = now
					logf("[状态] → QUIET（空闲；CPU 上限 %d%%，IDLE 优先级）", cfg.Quota.QuietCPUCap)
				}
			}

			// ── drain 触发（design §8：双触发 + min_gap；镜像归档积压也算触发源）──
			pend, perr := st.PendingStats(ctx)
			if perr == nil {
				histBacklog := cfg.Mirror.Enabled && pend.History > 0
				if (pend.Count > 0 || histBacklog) && now.Sub(lastDrainEnd) >= time.Duration(cfg.Sender.MinGap.Nanos()) {
					highWater := pend.Bytes >= cfg.Sender.FlushBytes.Int64()
					intervalDue := now.Sub(lastDrainEnd) >= time.Duration(cfg.Sender.FlushInterval.Nanos())
					if highWater || intervalDue || histBacklog {
						if !snap.active {
							markActive(rt, &lastChange, logf)
						}
						what := fmt.Sprintf("%d 文件 / %s", pend.Count, humanBytes(pend.Bytes))
						if pend.Count > 0 {
							what += fmt.Sprintf("（最老等待 %s）", time.Since(time.Unix(0, pend.OldestNs)).Round(time.Second))
						}
						if histBacklog {
							what += fmt.Sprintf(" + %d 项待归档", pend.History)
						}
						logf("[drain] 开始排空：%s", what)
						dstats, err := snd.Drain(ctx, 0) // work-conserving：排空到低水位
						lastDrainEnd = time.Now()
						summary := fmt.Sprintf("秒传 %d / 上传 %d / 归档 %d / 改名 %d / 推迟 %d / 失败 %d - 传输 %s",
							dstats.RapidOK, dstats.Uploaded, dstats.Archived, dstats.Renamed,
							dstats.Deferred, dstats.Failed, humanBytes(dstats.Bytes))
						if dstats.PrunedDirs > 0 {
							summary += fmt.Sprintf(" - 清空目录 %d", dstats.PrunedDirs)
						}
						logf("[drain] 完成：%s（%s）", summary, msHuman(int64(dstats.Elapsed/time.Millisecond)))
						rt.setLastDrain(&activity{At: lastDrainEnd, Summary: summary})
						if err != nil {
							if errors.Is(err, context.Canceled) {
								return nil // 信号/看板停止
							}
							if dstats.FatalError != nil {
								return fmt.Errorf("致命错误，daemon 停止: %w", dstats.FatalError)
							}
							logf("[!] drain 出错（下轮重试）: %v", err)
						}
						lastChange = time.Now()
					}
				}
			}
		}
	}
}

func markActive(rt *runtimeState, lastChange *time.Time, logf func(string, ...any)) {
	rt.mu.Lock()
	was := rt.active
	rt.active = true
	rt.mu.Unlock()
	if !was {
		power.SetActive()
		logf("[状态] → ACTIVE（NORMAL 优先级，CPU 上限 %d%%）", rt.snap().cfg.Quota.ActiveCPUCap)
	}
	*lastChange = time.Now()
}

// runFullScan -- poll 全量扫描（usn 的降级路径 + 周期/启动对账）。返回是否有变更。
func runFullScan(ctx context.Context, rt *runtimeState, root, reason string) bool {
	snap := rt.snap()
	runID, err := rPollStore.NextRunID(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] 获取扫描轮次失败: %v\n", err)
		return false
	}
	res, err := snap.poll.ScanAll(ctx, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] 全量扫描失败: %v\n", err)
		return false
	}
	changed := res.AddedFiles+res.AddedDirs+res.Modified+res.Removed+res.Resurrected > 0
	summary := fmt.Sprintf("新增 %d / 修改 %d / 删除 %d / 未变 %d（%s）",
		res.AddedFiles+res.AddedDirs, res.Modified, res.Removed, res.Unchanged, msHuman(res.WallMs))
	if changed || reason != "" {
		prefix := "[scan]"
		if reason != "" {
			prefix = "[scan-" + reason + "]"
		}
		fmt.Printf("%s %s\n", prefix, summary)
	}
	rt.setLastScan(&activity{At: time.Now(), Summary: summary})
	return changed
}

// durCfg -- time.Duration → config.Duration。
func durCfg(d time.Duration) config.Duration { return config.DurationOf(d) }

// ── 看板状态快照（按需构建；服务端不做周期性 DB 轮询，design §13）──
func statusProvider(rt *runtimeState, st *index.Store) func() any {
	return func() any {
		snap := rt.snap()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		pend, perr := st.PendingStats(ctx)
		var pending map[string]any
		if perr == nil {
			pending = map[string]any{
				"count": pend.Count, "bytes": pend.Bytes, "deferred": pend.Deferred,
				"history":     pend.History,
				"oldest_sec":  time.Since(time.Unix(0, pend.OldestNs)).Seconds(),
				"oldest_time": time.Unix(0, pend.OldestNs).Format("15:04:05"),
			}
			if pend.Count == 0 {
				pending["oldest_sec"] = nil
			}
		}

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms) // 不强制 GC（看板请求路径，design 低开销约束）
		rss, _, _ := processMem()

		var lastScan, lastDrain map[string]any
		if a := snap.lastScan; a != nil {
			lastScan = map[string]any{
				"ago_sec": time.Since(a.At).Seconds(), "summary": a.Summary,
			}
		}
		if a := snap.lastDrain; a != nil {
			lastDrain = map[string]any{
				"ago_sec": time.Since(a.At).Seconds(), "summary": a.Summary,
			}
		}

		return map[string]any{
			"version":    version,
			"schema":     rt.schemaVer,
			"backend":    rt.backendName(),
			"root":       rt.localRoot,
			"remote":     rt.remoteRoot,
			"db_path":    snap.cfg.Service.DBPath,
			"paused":     snap.paused,
			"stopped":    rt.stopping.Load(),
			"active":     snap.active,
			"creds":      credsSource(),
			"quiet_cap":  snap.cfg.Quota.QuietCPUCap,
			"uptime_sec": time.Since(rt.startedAt).Seconds(),
			"rss_bytes":  rss,
			"heap_bytes": ms.HeapAlloc,
			"pending":    pending,
			"last_scan":  lastScan,
			"last_drain": lastDrain,
		}
	}
}

// daemonCreds -- `bdsync daemon`（独立看板）用的凭据接口。
//
// 与 webcmd 里 setupState 的实现差别只在"保存之后怎么办"：daemon 的运行时是
// 主循环本身，进程内没有安全的重启入口，所以落盘后如实告诉人"重启服务生效"。
// （cookie 在 runService 起点被固化进 baidu.Client，见 restartService 的注释。）
func daemonCreds() web.CredsIO {
	return web.CredsIO{
		Status: func() map[string]any { return credsStatusPublic() },
		Save: func(payload map[string]any) (map[string]any, error) {
			if clear, _ := payload["clear"].(bool); clear {
				deleteCredentials()
				return map[string]any{"message": "已清除二进制同目录的 credentials.dat"}, nil
			}
			cookie, warn, empty, err := cookieFromPayload(payload)
			if err != nil {
				return nil, err
			}
			if empty {
				return nil, errors.New("请粘贴新的 Cookie（或分别填 BDUSS / STOKEN）")
			}
			remember := true
			if v, ok := payload["remember"].(bool); ok {
				remember = v
			}
			if err := saveCookieToDisk(cookie, remember); err != nil {
				return nil, err
			}
			msg := "凭据已保存"
			if !remember {
				msg += "（仅本次进程有效，未持久化）"
			}
			if warn != "" {
				msg += "；提醒：" + warn
			}
			return map[string]any{
				"message":   msg + "。重启 bdsync 服务后生效。",
				"restarted": false,
			}, nil
		},
	}
}

// ── 看板配置读写 ──

// configGet -- 当前 YAML 全文 + 基本表单值 + 需重启的结构性字段。
func configGet(rt *runtimeState) (string, map[string]any, []string) {
	snap := rt.snap()
	text := "（尚未保存配置文件；点击保存后将生成 " + saveConfigPath() + "）"
	if b, err := os.ReadFile(saveConfigPath()); err == nil {
		text = string(b)
	}
	basic := map[string]any{
		"poll_interval":   snap.cfg.Detector.PollInterval.String(),
		"flush_interval":  snap.cfg.Sender.FlushInterval.String(),
		"settle_duration": snap.cfg.Sender.SettleDuration.String(),
		"concurrency":     snap.cfg.Sender.Concurrency,
		"idle_threshold":  snap.cfg.Quota.IdleThreshold.String(),
		"exclude":         snap.exclude,
	}
	return text, basic, []string{"detector.mode", "service.log_file / log_max_mb / log_keep / db_path", "web.enabled / web.addr"}
}

// configSave -- 载荷：{"yaml": "..."}（全文覆盖）或 {"settings": {...}}（基本字段）。
// 校验 → 热更新子集落运行态 → 写盘；结构性改动只写盘并列入 needs_restart。
func configSave(rt *runtimeState, payload map[string]any) (applied, restart []string, err error) {
	snap := rt.snap()
	var next *config.Config

	if y, ok := payload["yaml"].(string); ok && strings.TrimSpace(y) != "" {
		next, err = config.Parse([]byte(y))
		if err != nil {
			return nil, nil, err
		}
	} else if st, ok := payload["settings"].(map[string]any); ok {
		next = config.Clone(snap.cfg)
		m := map[string]any{}
		if v, _ := st["poll_interval"].(string); v != "" {
			m["detector"] = map[string]any{"poll_interval": v}
		}
		senderM := map[string]any{}
		if v, _ := st["flush_interval"].(string); v != "" {
			senderM["flush_interval"] = v
		}
		if v, _ := st["settle_duration"].(string); v != "" {
			senderM["settle_duration"] = v
		}
		if v, ok := st["concurrency"].(float64); ok && v > 0 {
			senderM["concurrency"] = int(v)
		}
		if len(senderM) > 0 {
			m["sender"] = senderM
		}
		if v, _ := st["idle_threshold"].(string); v != "" {
			m["quota"] = map[string]any{"idle_threshold": v}
		}
		if v, ok := st["exclude"].([]any); ok {
			var list []string
			for _, x := range v {
				if s, _ := x.(string); strings.TrimSpace(s) != "" {
					list = append(list, strings.TrimSpace(s))
				}
			}
			m["exclude"] = list
		}
		if len(m) == 0 {
			return nil, nil, fmt.Errorf("没有可保存的有效字段（留空 = 保持原值）")
		}
		if err = config.ApplyMap(next, m); err != nil {
			return nil, nil, err
		}
	} else {
		return nil, nil, fmt.Errorf("载荷需包含 yaml 或 settings")
	}

	if err = next.Validate(); err != nil {
		return nil, nil, err
	}

	// 结构性差异（只写盘，需重启）
	restart = structuralChanges(snap.cfg, next)
	// 热更新差异（立即生效）
	applied = liveChanges(snap.cfg, next)

	rt.applyLive(next)
	settle := time.Duration(next.Sender.SettleDuration.Nanos())
	senderLive(settle, next.Sender.Concurrency, next.Sender.Policy)
	power.ConfigureCaps(next.Quota.QuietCPUCap, next.Quota.ActiveCPUCap)
	rt.mu.Lock()
	rt.quietCap = next.Quota.QuietCPUCap
	rt.mu.Unlock()

	// 写盘（原子：临时文件 + rename）
	if err = writeConfigFile(saveConfigPath(), next); err != nil {
		return nil, nil, err
	}
	return applied, restart, nil
}

// senderLive -- 由 cmdDaemon 注入的在线参数下发（避免包级全局耦合）。
var senderLive = func(settle time.Duration, concurrency int, policy string) {}

func structuralChanges(cur, next *config.Config) []string {
	var out []string
	if cur.Detector.Mode != next.Detector.Mode {
		out = append(out, "detector.mode")
	}
	// initialized 是"是否初始化过"的标记，变了不代表运行时结构变化，不提示重启
	if cur.Service.DBPath != next.Service.DBPath ||
		cur.Service.LogFile != next.Service.LogFile ||
		cur.Service.LogMaxMB != next.Service.LogMaxMB ||
		cur.Service.LogKeep != next.Service.LogKeep {
		out = append(out, "service.*（db_path / 日志路径与轮转）")
	}
	if cur.Web != next.Web {
		out = append(out, "web.*（enabled / addr）")
	}
	return out
}

func liveChanges(cur, next *config.Config) []string {
	var out []string
	add := func(name string, a, b any) {
		if fmt.Sprint(a) != fmt.Sprint(b) {
			out = append(out, name)
		}
	}
	add("poll_interval", cur.Detector.PollInterval, next.Detector.PollInterval)
	add("usn_poll_quiet", cur.Detector.UsnPollQuiet, next.Detector.UsnPollQuiet)
	add("usn_poll_active", cur.Detector.UsnPollActive, next.Detector.UsnPollActive)
	add("reconcile_interval", cur.Detector.ReconcileInterval, next.Detector.ReconcileInterval)
	add("flush_interval", cur.Sender.FlushInterval, next.Sender.FlushInterval)
	add("flush_bytes", cur.Sender.FlushBytes, next.Sender.FlushBytes)
	add("min_gap", cur.Sender.MinGap, next.Sender.MinGap)
	add("settle_duration", cur.Sender.SettleDuration, next.Sender.SettleDuration)
	add("concurrency", cur.Sender.Concurrency, next.Sender.Concurrency)
	add("policy", cur.Sender.Policy, next.Sender.Policy)
	add("idle_threshold", cur.Quota.IdleThreshold, next.Quota.IdleThreshold)
	add("cpu_cap", fmt.Sprintf("%d/%d", cur.Quota.QuietCPUCap, cur.Quota.ActiveCPUCap),
		fmt.Sprintf("%d/%d", next.Quota.QuietCPUCap, next.Quota.ActiveCPUCap))
	add("exclude", strings.Join(cur.Exclude, ","), strings.Join(next.Exclude, ","))
	return out
}

func saveConfigPath() string {
	if appCfgPath != "" {
		return appCfgPath
	}
	return "bdsync.yaml"
}

func writeConfigFile(path string, cfg *config.Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := "# bdsync 配置（由看板保存；格式见 internal/config/bdsync.example.yaml）\n" +
		"# 凭据不进本文件：BDUSS/STOKEN 存二进制同目录 credentials.dat（明文），\n" +
		"#   或环境变量 BDSYNC_COOKIE\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), b...), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// cmdSelftest -- 显式后端自检（会向 root 写探测文件；绝不自动执行）。
func cmdSelftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite 索引文件")
	backend := fs.String("backend", "auto", "auto | usn | poll")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := index.OpenExisting(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	localRoot, _, mode, _, _, err := st.PairInfo()
	if err != nil {
		return err
	}
	if err := detector.ValidateLocalRoot(localRoot); err != nil {
		return err
	}

	fmt.Printf("selftest：将在 %s 写入并删除一个探测文件\n", localRoot)
	useUsn := (*backend == "usn") || (*backend == "auto" && (mode == "usn" && detector.IsNTFS(localRoot)))
	if useUsn {
		if err := detector.RunUsnSelftest(context.Background(), localRoot, st); err != nil {
			return err
		}
		fmt.Println("[OK] usn 自检通过：写入的探测文件在 10s 内产生了 journal 事件")
		return nil
	}
	p := detector.NewPoll(localRoot, st, detector.Options{})
	if err := p.SelfTest(context.Background(), localRoot); err != nil {
		return err
	}
	fmt.Println("[OK] poll 自检通过：探测文件可写、可被扫描路径发现、已清理")
	return nil
}
