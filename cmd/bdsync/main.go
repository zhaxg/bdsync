// bdsync -- 本地 → 百度网盘 单向备份守护进程（design.md 设计稿 v1）。
//
// Phase 1：本地全量扫描（poll 后端）+ SQLite 索引 + 只读校验。
//
// 只读保证：scan/validate/status 对数据根目录只做 readdir + stat，
// 不创建、不修改、不删除任何文件。所有写入仅落在 --db 指定的索引文件上。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"bdsync/internal/baidu"
	"bdsync/internal/config"
	"bdsync/internal/detector"
	"bdsync/internal/index"
	"bdsync/internal/sender"
)

// appCfg -- 启动时加载的配置（flag > 配置文件 > 默认值）。
var appCfg *config.Config

// appCfgPath -- 实际加载的配置文件路径（看板保存时写回该文件；空 = bdsync.yaml）。
var appCfgPath string

// defaultDB -- 索引库路径（service.db_path，可被 --db 覆盖）。
func defaultDB() string { return appCfg.Service.DBPath }

// excludeFrom -- --exclude（逗号分隔）优先，否则用配置文件的 exclude 列表。
func excludeFrom(flagVal string) []string {
	if strings.TrimSpace(flagVal) == "" {
		return appCfg.Exclude
	}
	var out []string
	for _, p := range strings.Split(flagVal, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stripConfigArgs -- --config 已在预解析阶段消费，从分发参数中剥掉，
// 避免子命令 FlagSet 报 "flag provided but not defined"。
func stripConfigArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" {
			i++ // 连值一起剥
			continue
		}
		if strings.HasPrefix(a, "--config=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// loadConfigFromArgs -- 从 os.Args 里预解析 --config（先于 flag 解析，使配置
// 能作为 flag 的默认值）。未指定时探测 ./bdsync.yaml。
func loadConfigFromArgs(args []string) error {
	path := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" && i+1 < len(args) {
			path = args[i+1]
			break
		}
		if strings.HasPrefix(a, "--config=") {
			path = strings.TrimPrefix(a, "--config=")
			break
		}
	}
	if path == "" {
		path = config.FindDefault()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	appCfg = cfg
	appCfgPath = path
	return nil
}

// openConfiguredStore -- 配置驱动的统一入口（scan/send/daemon/status/check/reconcile 通用）：
//
//  1. 校验 pair 必填（配置驱动：目标不明 → 拒绝运行）
//  2. 打开索引库；不存在则按配置新建（就地部署：库与二进制同目录）
//  3. 绑定/核对本地根目录（ensurePair 内已实现"换根目录即报错"）
//  4. 远端：库内为空则按配置写入；已绑定且与配置不符 → 报错（防把数据传到两个地方）
func openConfiguredStore(ctx context.Context, dbOverride string) (*index.Store, string, string, error) {
	if err := appCfg.ValidatePair(); err != nil {
		return nil, "", "", err
	}
	localRoot := appCfg.Pair.LocalRoot
	remoteRoot := appCfg.Pair.Remote
	dbPath := defaultDB()
	if dbOverride != "" {
		dbPath = dbOverride
	}

	st, err := index.Open(dbPath, localRoot)
	if err != nil {
		return nil, "", "", err
	}
	_, boundRemote, _, _, _, err := st.PairInfo()
	if err != nil {
		st.Close()
		return nil, "", "", err
	}
	if boundRemote != "" && !sameRemotePath(boundRemote, remoteRoot) {
		st.Close()
		return nil, "", "", fmt.Errorf("索引库已绑定远端目录 %s，与配置的 pair.remote %s 不符\n"+
			"  这通常意味着换了备份目的地：请同步修改配置，或换一个 --db 指向新库。\n"+
			"  （不会动网盘上已有的任何文件）", boundRemote, remoteRoot)
	}
	if boundRemote == "" {
		if err := st.SetRemoteRoot(ctx, remoteRoot); err != nil {
			st.Close()
			return nil, "", "", err
		}
	}
	return st, localRoot, remoteRoot, nil
}

// sameLocalPath -- 本地路径比较：先 Clean 统一分隔符风格（`D:/x` == `D:\x`），
// Windows 上再忽略大小写。
func sameLocalPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// sameRemotePath -- 远端网盘路径比较：只忽略结尾斜杠，**大小写敏感**
// （网盘路径区分大小写，同名不同案是两个目录）。
func sameRemotePath(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// fillMemMetrics -- 填充内存分解指标（扫描报告用）。
func fillMemMetrics(res *detector.ScanResult) {
	if rss, peak, ok := processMem(); ok {
		res.RssBytes, res.PeakRssBytes = rss, peak
	}
	rm := sampleMem()
	res.HeapBytes = rm.HeapAlloc
	res.GoSysBytes = rm.Sys
	res.HeapSysBytes = rm.HeapSys
	res.HeapRelBytes = rm.HeapReleased
}

func usage() {
	fmt.Fprint(os.Stderr, `bdsync -- 本地 → 百度网盘 单向备份

  * 直接双击本 exe（不带任何参数）即可启动：控制台打印看板地址，**不自动开浏览器**，
    自己在终端里按住 Ctrl 单击地址打开，网页里填三项就能开始备份。

用法:
  bdsync scan     [--root <目录>] [--db ...] [--backend auto|usn|poll] [--exclude 规则] [--walk-only] [--json]
  bdsync daemon   [--config bdsync.yaml] [--db ...] [--backend auto|usn|poll]    （常驻：检测+排空+电源状态机）
  bdsync send     [--db ...] [--dry-run] [--max N]
  bdsync status   [--db ...] [--json]
  bdsync reconcile [--db ...] [--dry-run] [--prune [--days 90]]  （远端清单对账 / 历史过期清理）
  bdsync check    [--db ...] [--path 相对路径]   （下载校验内容一致性）
  bdsync meta     --remote /网盘/路径
  bdsync selftest [--db ...] [--backend auto|usn|poll]  （会向 root 写探测文件）
  bdsync validate --root <目录>
  bdsync web      [--browser]   （双击入口；--browser 才自动开浏览器）

服务化（nssm / 计划任务 / systemd）:
  bdsync --service            或环境变量 BDSYNC_SERVICE=1
    - 不弹浏览器、启动失败不等按键（否则进程会卡在 Scanln 上被服务管理器判超时）
    - stdout 可重定向到文件；停止走 SIGINT/SIGTERM（nssm 的 CTRL_C/关闭窗口）
    - 配置/凭据/索引一律按 exe 所在目录解析，与工作目录无关

配置（就地便携：exe + bdsync.yaml + index.db + credentials.dat + logs/ 同一文件夹，可整体搬走）:
  --config bdsync.yaml   未指定时探测 <exe同目录>/bdsync.yaml → ./bdsync.yaml → ...
  必填 pair（缺任一项拒绝启动，备份目标不许靠猜）:
    pair.local_root: "D:/YourData"          本地根目录（严格只读）
    pair.remote:     "/bdsync-backup"     网盘内路径（/ 开头）
  service.db_path 默认 index.db（与二进制同目录）；log_file 默认 logs/bdsync.log
  相对路径一律以配置文件所在目录为基准解析；从任何目录调用结果一致

凭据（BDUSS/STOKEN）绝不进配置文件，两个来源（优先级从高到低）:
  1) 环境变量  set BDSYNC_COOKIE="BDUSS=xxx; STOKEN=yyy"   （或 BDSYNC_BDUSS / BDSYNC_STOKEN）
  2) 二进制同目录 credentials.dat ---- 明文文件，看板里勾「记住凭据」生成
     cookie 过期时不必重走向导：看板「配置 → 网盘凭据」贴一条新的即可，保存后自动重启运行时

后端:
  usn   Windows 本地 NTFS：读 USN Journal，空闲零文件打开/零磁盘唤醒（design §4.2）
  poll  全量扫描，天然完整，所有平台的基线与降级路径（design §4.1）

`)
}

func main() {
	// --service 不是任何子命令的 flag：先剥掉，它只影响 serviceMode（见 creds_api.go）
	args := os.Args
	if hasFlag(args[1:], "--service") || hasFlag(args[1:], "-service") {
		args = append([]string{args[0]}, stripServiceArgs(args[1:])...)
	}

	// 零门槛入口：**直接双击 exe**（不带子命令）= 启动看板 + 向导
	if len(args) < 2 {
		if err := loadConfigFromArgs(nil); err != nil {
			fmt.Fprintln(os.Stderr, "[X] 配置读取失败:", err)
			pauseBeforeExit()
			os.Exit(1)
		}
		runWebEntry(nil)
		return
	}
	if args[1] == "help" || args[1] == "-h" || args[1] == "--help" {
		usage()
		return
	}
	if err := loadConfigFromArgs(args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "[X]", err)
		pauseBeforeExit()
		os.Exit(2)
	}
	cmdArgs := stripConfigArgs(args[2:])
	var err error
	switch args[1] {
	case "scan":
		err = cmdScan(cmdArgs)
	case "send":
		err = cmdSend(cmdArgs)
	case "status":
		err = cmdStatus(cmdArgs)
	case "daemon":
		err = cmdDaemon(cmdArgs)
	case "web":
		runWebEntry(cmdArgs)
	case "selftest":
		err = cmdSelftest(cmdArgs)
	case "reconcile":
		err = cmdReconcile(cmdArgs)
	case "validate":
		err = cmdValidate(cmdArgs)
	case "meta":
		err = cmdMeta(cmdArgs)
	case "check":
		err = cmdCheck(cmdArgs)
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		pauseBeforeExit()
		os.Exit(1)
	}
}

// hasFlag -- 参数里是否含指定 flag（用于服务模式识别与剥离）。
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	root := fs.String("root", appCfg.Pair.LocalRoot, "本地根目录（严格只读）；默认取配置 pair.local_root")
	dbPath := fs.String("db", defaultDB(), "SQLite 索引文件")
	batch := fs.Int("batch", 2000, "每批写库条数（分批落库，避免超长事务）")
	walkOnly := fs.Bool("walk-only", false, "只遍历计数不写库（性能基线）")
	repeat := fs.Int("repeat", 1, "同进程内重复扫描次数（测稳态内存是否随轮次增长）")
	backend := fs.String("backend", "auto", "检测后端: auto | usn | poll（usn=Windows NTFS journal，空闲零 I/O）")
	exclude := fs.String("exclude", "", "排除规则（逗号分隔，如 *.tmp,~$*,node_modules,.stfolder/）；默认取配置文件 exclude")
	jsonOut := fs.Bool("json", false, "以 JSON 输出结果")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" {
		return appCfg.ValidatePair() // 报"缺少 pair.local_root"的完整指引
	}

	// design §1.1：启动强制校验本地固定磁盘
	if err := detector.ValidateLocalRoot(*root); err != nil {
		return err
	}
	if err := detector.ValidateNoLocalHistory(*root, appCfg.Mirror.HistoryDir); err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	rootAbs = filepath.Clean(rootAbs)

	ctx, stop := signal.NotifyContext(context.Background(), stopSignals()...)
	defer stop()

	var st *index.Store
	var runID int64
	if !*walkOnly {
		st, err = index.Open(*dbPath, rootAbs)
		if err != nil {
			return err
		}
		defer st.Close()
		runID, err = st.NextRunID(ctx)
		if err != nil {
			return err
		}
	}

	// ── usn 模式：journal 增量（空闲零文件打开、零磁盘唤醒，design §4.1）──
	// 游标失效 / 后端不可用 → 回退 poll 全量扫描（这是正常路径，design §4.2d）。
	useUsn := !*walkOnly && *repeat <= 1
	switch *backend {
	case "usn":
		if !detector.IsNTFS(rootAbs) {
			return errors.New("usn 后端仅支持 Windows 本地 NTFS（design §4.1）")
		}
	case "poll":
		useUsn = false
	case "auto":
		useUsn = useUsn && detector.IsNTFS(rootAbs)
	default:
		return fmt.Errorf("未知后端: %s（可选 auto|usn|poll）", *backend)
	}
	if useUsn {
		_ = st.SetDetectorMode("usn")
		cpuA, hasCpu := processCPUTime()
		res, needFull, uerr := detector.RunUsnCycle(ctx, rootAbs, st, excludeFrom(*exclude))
		switch {
		case uerr != nil:
			fmt.Printf("[!] usn 后端不可用（%v），回退 poll 全量扫描\n", uerr)
		case needFull:
			fmt.Println("[!] 游标失效/journal 重建（design §4.2d 正常路径）→ 全量扫描")
		default:
			if hasCpu {
				if cpuB, ok := processCPUTime(); ok {
					res.CpuMs = int64((cpuB - cpuA) / time.Millisecond)
				}
			}
			fillMemMetrics(res)
			if err := st.SaveScanStats(ctx, res); err != nil {
				res.NoteErr(fmt.Errorf("保存扫描统计失败: %w", err))
			}
			printScanReport(res, *dbPath, dbFileSize(*dbPath), *jsonOut)
			return nil
		}
	}
	if !*walkOnly {
		_ = st.SetDetectorMode("poll")
	}

	p := detector.NewPoll(rootAbs, st, detector.Options{BatchSize: *batch, WalkOnly: *walkOnly, Exclude: excludeFrom(*exclude)})

	loops := *repeat
	if loops < 1 {
		loops = 1
	}
	type memSample struct{ wall, cpu, rss, heapSys, live int64 }
	samples := make([]memSample, 0, loops)

	var last *detector.ScanResult
	var scanErr error
	for i := 0; i < loops; i++ {
		tLoop := time.Now()
		cpuA, hasCpu := processCPUTime()

		res, err := p.ScanAll(ctx, runID)
		if res == nil {
			return err
		}
		scanErr = err

		if res.WallMs == 0 {
			res.WallMs = msSince(tLoop)
		}
		if hasCpu {
			if cpuB, ok := processCPUTime(); ok {
				res.CpuMs = int64((cpuB - cpuA) / time.Millisecond)
			}
		}
		fillMemMetrics(res)

		last = res
		samples = append(samples, memSample{res.WallMs, res.CpuMs, res.RssBytes, res.HeapSysBytes, res.HeapBytes})

		if st != nil && scanErr == nil {
			if err := st.SaveScanStats(ctx, res); err != nil {
				res.NoteErr(fmt.Errorf("保存扫描统计失败: %w", err))
			}
		}
		if scanErr != nil {
			break
		}
	}

	if loops > 1 && !*jsonOut {
		fmt.Printf("重复扫描稳态观测（同进程 %d 轮）：\n", loops)
		for i, s := range samples {
			fmt.Printf("  第 %d 轮  %6s | CPU %6s | RSS %7s | Go堆sys %7s | 存活堆 %8s\n",
				i+1, msHuman(s.wall), msHuman(s.cpu),
				humanBytes(s.rss), humanBytes(s.heapSys), humanBytes(s.live))
		}
		// 模拟进入 QUIET 态的内存修剪（design §4.4：进入 QUIET 调 FreeOSMemory）
		debug.FreeOSMemory()
		if rss, _, ok := processMem(); ok {
			rm := sampleMem()
			fmt.Printf("  FreeOSMemory 后（模拟 QUIET 态）  RSS %7s | Go堆sys %7s | 存活堆 %8s\n",
				humanBytes(rss), humanBytes(rm.HeapSys), humanBytes(rm.HeapAlloc))
		}
		fmt.Println()
	}

	if *walkOnly {
		printScanReport(last, "", 0, *jsonOut)
	} else {
		printScanReport(last, *dbPath, dbFileSize(*dbPath), *jsonOut)
	}
	return scanErr
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite 索引文件")
	jsonOut := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, cfgLocal, cfgRemote, err := openConfiguredStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	localRoot, remoteRoot, mode, schemaVer, pairID, err := st.PairInfo()
	if err != nil {
		return err
	}
	_ = cfgLocal
	if remoteRoot == "" {
		remoteRoot = cfgRemote
	}
	counts, err := st.Counts(ctx)
	if err != nil {
		return err
	}
	pend, perr := st.PendingStats(ctx)
	if perr != nil {
		return err
	}
	var last detector.ScanResult
	haveStats := st.LoadScanStats(ctx, &last) == nil

	if *jsonOut {
		out := statusJSON{
			DB:      *dbPath,
			DBBytes: dbFileSize(*dbPath),
			Root:    localRoot,
			Remote:  remoteRoot,
			Mode:    mode,
			Schema:  schemaVer,
			PairID:  pairID,
			Counts:  counts,
			Pending: statusPending{
				Count: pend.Count, Bytes: pend.Bytes,
				OldestNs: pend.OldestNs, Deferred: pend.Deferred,
				History: pend.History,
			},
		}
		if haveStats {
			out.Last = &last
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printStatus(*dbPath, dbFileSize(*dbPath), localRoot, remoteRoot, mode, schemaVer, pairID,
		counts, pend, &last, haveStats)
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	root := fs.String("root", "", "要校验的根目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" {
		return errors.New("缺少 --root")
	}
	if err := detector.ValidateLocalRoot(*root); err != nil {
		return err
	}
	abs, _ := filepath.Abs(*root)
	fmt.Printf("[OK] %s\n  是本地固定磁盘上的目录，可作为备份根目录（校验过程只读）。\n", abs)
	return nil
}

type statusPending struct {
	Count    int64 `json:"count"`
	Bytes    int64 `json:"bytes"`
	OldestNs int64 `json:"oldest_ns"`
	Deferred int64 `json:"deferred"`
	History  int64 `json:"history"` // 镜像归档积压
}

type statusJSON struct {
	DB      string               `json:"db"`
	DBBytes int64                `json:"db_bytes"`
	Root    string               `json:"root"`
	Remote  string               `json:"remote,omitempty"`
	Mode    string               `json:"mode"`
	Schema  int                  `json:"schema"`
	PairID  int64                `json:"pair_id"`
	Counts  *index.Counts        `json:"counts"`
	Pending statusPending        `json:"pending"`
	Last    *detector.ScanResult `json:"last_scan,omitempty"`
}

// loadCookie -- BDUSS/STOKEN 只从环境变量读取，绝不落盘（design 安全约束）。
// memCookie / memCookieSrc -- 运行期由网页向导或「凭据」卡片注入的凭据（仅内存，不落盘）
// 及其来源。来源必须跟着值一起记：只看"memCookie 有没有值"会把冷启动从
// credentials.dat 读回来的凭据误报成"仅本次进程（未保存）"---- 与事实相反。
var (
	memCookie    atomic.Value // string
	memCookieSrc atomic.Value // string: "session" | "file"
)

// setCookieForRun -- 供网页把用户刚输入的凭据交给运行时（来源=本次录入）。
func setCookieForRun(c string) { setCookieForRunFrom(c, "session") }

func setCookieForRunFrom(c, src string) {
	memCookie.Store(c)
	memCookieSrc.Store(src)
}

func loadCookie() (string, error) {
	if c := os.Getenv("BDSYNC_COOKIE"); c != "" {
		return c, nil
	}
	bduss := os.Getenv("BDSYNC_BDUSS")
	if bduss != "" {
		cookie := "BDUSS=" + bduss
		if stoken := os.Getenv("BDSYNC_STOKEN"); stoken != "" {
			cookie += "; STOKEN=" + stoken
		}
		return cookie, nil
	}
	// 网页向导本次输入（仅内存）
	if c, _ := memCookie.Load().(string); c != "" {
		return c, nil
	}
	// 网页向导"记住凭据"：二进制同目录 credentials.dat（明文）
	if c := loadCredentials(); c != "" {
		return c, nil
	}
	return requireCookie()
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite 索引文件")
	remote := fs.String("remote", "", "远端根目录（如 /bdsync-backup；首次发送时绑定，之后复用）")
	dryRun := fs.Bool("dry-run", false, "只显示待传统计与列表，不联网")
	maxFiles := fs.Int("max", 0, "本次最多上传文件数（0=不限）")
	settle := fs.Duration("settle", time.Duration(appCfg.Sender.SettleDuration.Nanos()), "稳定性闸门时长")
	policy := fs.String("policy", appCfg.Sender.Policy, "同名文件策略: overwrite | skip")
	concurrency := fs.Int("concurrency", appCfg.Sender.Concurrency, "最大上传并发（AIMD 上限）")
	appID := fs.Int("app-id", baidu.DefaultAppID, "百度 app_id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), stopSignals()...)
	defer stop()

	// 配置驱动：本地根目录与远端目录都来自 pair（--remote 仅为显式覆盖）
	st, localRoot, cfgRemote, err := openConfiguredStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	remoteRoot := cfgRemote
	if *remote != "" {
		if !strings.EqualFold(*remote, cfgRemote) {
			return fmt.Errorf("--remote %s 与配置的 pair.remote %s 不符\n"+
				"  备份目的地由配置声明（设计原则：目标必须显式、不靠命令行临时指定）。\n"+
				"  请改配置，或换一个 --db 指向另一份索引。", *remote, cfgRemote)
		}
	}
	if err := detector.ValidateNoLocalHistory(localRoot, appCfg.Mirror.HistoryDir); err != nil {
		return err
	}

	if *dryRun {
		return printDryRun(ctx, st, localRoot, remoteRoot, false)
	}

	cookie, err := loadCookie()
	if err != nil {
		return err
	}
	bc, err := baidu.New(*appID, cookie)
	if err != nil {
		return err
	}

	s := sender.New(st, bc, sender.Options{
		LocalRoot:   localRoot,
		RemoteRoot:  remoteRoot,
		Policy:      *policy,
		Settle:      *settle,
		Concurrency: *concurrency,
		MirrorOn:    appCfg.Mirror.Enabled,
		HistoryDir:  appCfg.Mirror.HistoryDir,
		HistoryDays: appCfg.Mirror.HistoryDays,
		OnRename:    appCfg.Mirror.OnRename,
		Logf: func(format string, a ...any) {
			fmt.Printf(format+"\n", a...)
		},
	})

	fmt.Printf("发送  本地 %s\n      → 网盘 %s\n", localRoot, remoteRoot)
	stats, err := s.Drain(ctx, *maxFiles)
	printSendStats(stats)
	return err
}

func cmdMeta(args []string) error {
	fs := flag.NewFlagSet("meta", flag.ContinueOnError)
	remotePath := fs.String("remote", "", "远端路径（如 /bdsync-smoke/a.txt）")
	appID := fs.Int("app-id", baidu.DefaultAppID, "百度 app_id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *remotePath == "" {
		return errors.New("缺少 --remote")
	}
	cookie, err := loadCookie()
	if err != nil {
		return err
	}
	bc, err := baidu.New(*appID, cookie)
	if err != nil {
		return err
	}
	fi, err := bc.Meta(*remotePath)
	if err != nil {
		return err
	}
	fmt.Printf("[OK] %s\n  fs_id=%d  md5=%s  size=%d  mtime=%s  isdir=%v\n",
		fi.Path, fi.FsID, fi.MD5, fi.Size, time.Unix(fi.Mtime, 0).Format("2006-01-02 15:04:05"), fi.Isdir)
	if fi.Isdir {
		items, err := bc.List(*remotePath)
		if err != nil {
			return fmt.Errorf("列目录失败: %w", err)
		}
		fmt.Printf("  目录内容（%d 项）:\n", len(items))
		limit := len(items)
		if limit > 100 {
			limit = 100
		}
		for _, it := range items[:limit] {
			kind := "file"
			if it.Isdir {
				kind = "dir "
			}
			fmt.Printf("    %s %10d  %s  %s\n", kind, it.Size,
				time.Unix(it.Mtime, 0).Format("2006-01-02 15:04"), it.Path)
		}
		if limit < len(items) {
			fmt.Printf("    ... 其余 %d 项\n", len(items)-limit)
		}
	}
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite 索引文件")
	only := fs.String("path", "", "只校验指定文件（相对路径，逗号分隔）；空=全部 IDENTICAL 文件")
	appID := fs.Int("app-id", baidu.DefaultAppID, "百度 app_id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), stopSignals()...)
	defer stop()

	st, _, remoteRoot, err := openConfiguredStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	cookie, err := loadCookie()
	if err != nil {
		return err
	}
	bc, err := baidu.New(*appID, cookie)
	if err != nil {
		return err
	}

	var list []index.CheckEntry
	if *only != "" {
		list, err = st.CheckList(ctx, strings.Split(*only, ","))
	} else {
		list, err = st.CheckList(ctx, nil)
	}
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("没有可校验的 IDENTICAL 文件")
		return nil
	}

	pass, metaBad := 0, 0
	for _, e := range list {
		remotePath := path.Join(remoteRoot, e.RelPath)
		dmd5, dsize, err := bc.DownloadAndHash(remotePath, e.Size)
		if err != nil {
			fmt.Printf("[[X]] %s  下载失败: %v\n", e.RelPath, err)
			continue
		}
		if dmd5 != e.LMD5 {
			// 0 字节文件的历史行 l_md5 为空：空内容 md5 为标准常量
			if e.Size == 0 && e.LMD5 == "" && dmd5 == "d41d8cd98f00b204e9800998ecf8427e" {
				pass++
				fmt.Printf("[[OK]] %s  空文件 md5=%s\n", e.RelPath, dmd5)
				continue
			}
			fmt.Printf("[[X]] %s  内容不一致！本地 md5=%s 下载 md5=%s (size %d/%d)\n",
				e.RelPath, e.LMD5, dmd5, e.Size, dsize)
			continue
		}
		pass++
		note := "元数据一致"
		if e.RMD5 != e.LMD5 {
			metaBad++
			note = fmt.Sprintf("远端元数据 md5=%s 与实际内容不符（服务端已知问题，内容无损）", e.RMD5)
		}
		fmt.Printf("[[OK]] %s  md5=%s  %s\n", e.RelPath, dmd5, note)
	}
	fmt.Printf("\n校验完成：内容一致 %d/%d，其中远端元数据异常 %d\n", pass, len(list), metaBad)
	if pass != len(list) {
		return fmt.Errorf("%d 个文件内容校验失败", len(list)-pass)
	}
	return nil
}

func printDryRun(ctx context.Context, st *index.Store, localRoot, remoteRoot string, newlyBound bool) error {
	if newlyBound {
		fmt.Printf("（dry-run：已把远端根目录绑定为 %s）\n", remoteRoot)
	}
	pend, err := st.PendingStats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("dry-run：本地 %s\n      → 网盘 %s\n", localRoot, remoteRoot)
	fmt.Printf("  待传 %s 个文件 / %s", commas(pend.Count), humanBytes(pend.Bytes))
	if pend.Count > 0 {
		fmt.Printf("，最老等待 %s", time.Since(time.Unix(0, pend.OldestNs)).Round(time.Second))
	}
	fmt.Println()
	if pend.Deferred > 0 {
		fmt.Printf("  （其中 %s 个推迟中：settle 未到 / 写入中 / 上轮临时失败，drain 会自动重试）\n", commas(pend.Deferred))
	}
	// 镜像模式：归档预览（首跑安全网 ---- 升级后第一次 drain 前先看这里）
	if appCfg.Mirror.Enabled {
		fmt.Printf("  镜像模式    归档目录 %s，保留 %d 天\n",
			appCfg.Mirror.HistoryDir, appCfg.Mirror.HistoryDays)
		fmt.Printf("    将归档（本地已删 → 远端旧版移入 .history）: %s 项\n", commas(pend.History))
		if pend.History > 0 {
			items, _ := st.HistoryQueue(ctx, 10)
			for _, it := range items {
				fmt.Printf("      %s  (远端 mtime %s)\n", it.RelPath, time.Unix(it.RMtime, 0).UTC().Format("2006-01-02 15:04"))
			}
			if pend.History > 10 {
				fmt.Printf("      ... 其余 %s 项\n", commas(pend.History-10))
			}
		}
		if pac, _ := st.PreArchiveCount(ctx); pac > 0 {
			fmt.Printf("    覆盖前需先归档旧版（修改类上传）: %s 项\n", commas(pac))
		}
	}
	list, err := st.PendingList(ctx, 20)
	if err != nil {
		return err
	}
	for _, e := range list {
		fmt.Printf("    %10s  %s\n", humanBytes(e.Size), e.RelPath)
	}
	if int64(len(list)) < pend.Count {
		fmt.Printf("    ... 其余 %s 个\n", commas(pend.Count-int64(len(list))))
	}
	if pend.Count == 0 && pend.History == 0 {
		fmt.Println("  队列已排空 [OK]")
	}
	return nil
}

// cmdReconcile -- 手动触发远端清单对账（design镜像 Q2）或历史清理（--prune）。
func cmdReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite 索引文件")
	dryRun := fs.Bool("dry-run", false, "只预览，不改动远端")
	prune := fs.Bool("prune", false, "执行历史过期清理（而非清单对账）")
	pruneDays := fs.Int("days", appCfg.Mirror.HistoryDays, "历史保留天数（配合 --prune）")
	appID := fs.Int("app-id", baidu.DefaultAppID, "百度 app_id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !appCfg.Mirror.Enabled {
		return errors.New("镜像模式未开启（mirror.enabled），无对账/清理可做")
	}

	ctx, stop := signal.NotifyContext(context.Background(), stopSignals()...)
	defer stop()

	st, localRoot, remoteRoot, err := openConfiguredStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := detector.ValidateLocalRoot(localRoot); err != nil {
		return err
	}
	if err := detector.ValidateNoLocalHistory(localRoot, appCfg.Mirror.HistoryDir); err != nil {
		return err
	}

	cookie, err := loadCookie()
	if err != nil {
		return err
	}
	bc, err := baidu.New(*appID, cookie)
	if err != nil {
		return err
	}
	snd := sender.New(st, bc, sender.Options{
		LocalRoot:   localRoot,
		RemoteRoot:  remoteRoot,
		Policy:      appCfg.Sender.Policy,
		Settle:      time.Duration(appCfg.Sender.SettleDuration.Nanos()),
		MirrorOn:    true,
		HistoryDir:  appCfg.Mirror.HistoryDir,
		HistoryDays: appCfg.Mirror.HistoryDays,
		OnRename:    appCfg.Mirror.OnRename,
		Logf:        func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})

	if *prune {
		removed, err := snd.PruneHistory(ctx, *pruneDays, *dryRun)
		if err != nil {
			return err
		}
		verb := "可清理"
		if !*dryRun {
			verb = "已清理"
		}
		fmt.Printf("%s %d 个过期历史桶（保留 %d 天）:\n", verb, len(removed), *pruneDays)
		for _, name := range removed {
			fmt.Printf("    %s\n", name)
		}
		return nil
	}

	res, err := snd.ReconcileRemote(ctx, *dryRun)
	if err != nil {
		return err
	}
	verb := "将归档"
	if !*dryRun {
		verb = "已归档"
	}
	fmt.Printf("对账%s  %s\n", map[bool]string{true: "（dry-run）", false: ""}[*dryRun], res.Summary())
	if len(res.Samples) > 0 {
		fmt.Printf("  %s（前 %d 项）:\n", verb, len(res.Samples))
		for _, s := range res.Samples {
			fmt.Printf("    %s\n", s)
		}
	}
	return nil
}

func printSendStats(s *sender.Stats) {
	elapsed := s.Elapsed.Round(100 * time.Millisecond)
	fmt.Printf("完成  耗时 %s\n", elapsed)
	fmt.Printf("  秒传 %d / 分片上传 %d / 远端已有 %d / 归档 %d / 改名 %d / 推迟 %d / 失败 %d（共领取 %d）\n",
		s.RapidOK, s.Uploaded, s.RemoteHave, s.Archived, s.Renamed, s.Deferred, s.Failed, s.Claimed)
	fmt.Printf("  实际传输 %s（不含秒传与归档；归档为服务器端移动，零带宽）\n", humanBytes(s.Bytes))
	if s.Bytes > 0 && s.Elapsed > 0 {
		fmt.Printf("  平均速率 %s/s\n", humanBytes(int64(float64(s.Bytes)/s.Elapsed.Seconds())))
	}
	if s.FatalError != nil {
		fmt.Printf("  [X] 致命错误，已停止：%v\n", s.FatalError)
	}
}
