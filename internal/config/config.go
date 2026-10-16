// Package config -- YAML 配置（design §14）。
//
// 优先级：命令行 flag > 配置文件 > 默认值。
// 安全约束：凭据（BDUSS/STOKEN）**绝不进配置文件**----防止凭据随配置文件被拷走/提交。
// 来源优先级：环境变量 > 二进制同目录 credentials.dat（明文）> 看板录入
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration -- 支持 "5m"、"10s"、"24h" 的时长。
type Duration struct{ d int64 } // 纳秒

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		// 也接受纯数字（秒）
		var n int64
		if err2 := node.Decode(&n); err2 == nil {
			d.d = n * 1_000_000_000
			return nil
		}
		return err
	}
	v, err := parseDuration(s)
	if err != nil {
		return fmt.Errorf("无法解析时长 %q（如 5m / 30s / 24h）: %w", s, err)
	}
	d.d = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return formatDuration(d.d), nil }

// Seconds -- 时长（秒）。
func (d Duration) Seconds() float64 { return float64(d.d) / 1e9 }

// Nanos -- 时长（纳秒）。
func (d Duration) Nanos() int64 { return d.d }

// String -- "5m" 形式（表单回显 / banner 展示）。
func (d Duration) String() string { return formatDuration(d.d) }

// DurationOf -- time.Duration → 配置时长。
func DurationOf(v time.Duration) Duration { return Duration{d: int64(v)} }

// Clone -- 深拷贝（含 Exclude 切片；其余均为值类型）。
func Clone(c *Config) *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Exclude = append([]string(nil), c.Exclude...)
	return &out
}

// ApplyMap -- 将嵌套 settings 映射合并进已有配置（YAML 语义：只覆盖出现的键）。
//
// 这里**故意不启用严格模式**：入参来自看板「基本字段」表单（daemon.configSave），
// 走 map 中转只是为了复用 YAML 的"只覆盖出现的键"语义，本函数是通用导出接口，
// 对入参键集不做假设；若开严格模式，调用方传入的扩展/暂未识别键会直接变成硬错误，
// 破坏看板正向保存流程。拼写错误由 Load / Parse 两个入口负责拦截。
func ApplyMap(cfg *Config, m map[string]any) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, cfg)
}

// ByteSize -- 支持 "2GB"、"512MB"、"1048576" 的字节数。
type ByteSize int64

func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		var n int64
		if err2 := node.Decode(&n); err2 == nil {
			*b = ByteSize(n)
			return nil
		}
		return err
	}
	v, err := ParseByteSize(s)
	if err != nil {
		return fmt.Errorf("无法解析大小 %q（如 512MB / 2GB）: %w", s, err)
	}
	*b = ByteSize(v)
	return nil
}

// Int64 -- 字节数值。
func (b ByteSize) Int64() int64 { return int64(b) }

func parseDuration(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// 复用 time.ParseDuration 的语法（ns/us/ms/s/m/h）
	var total int64
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9') {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("缺少数值部分")
		}
		n, err := strconv.ParseInt(rest[:i], 10, 64)
		if err != nil {
			return 0, err
		}
		rest = rest[i:]
		unit := rest
		j := 0
		for j < len(rest) && (rest[j] < '0' || rest[j] > '9') {
			j++
		}
		unit = rest[:j]
		rest = rest[j:]
		switch unit {
		case "ns":
			total += n
		// "us" 有两种写法：U+00B5 MICRO SIGN 与 U+03BC GREEK SMALL LETTER MU，
		// 肉眼完全一样但字节不同。都用码点写，避免编辑器/编码环节把它们合并成一个。
		case "\u00B5s", "\u03BCs":
			total += n * 1_000
		case "ms":
			total += n * 1_000_000
		case "s", "":
			total += n * 1_000_000_000
		case "m":
			total += n * 60 * 1_000_000_000
		case "h":
			total += n * 3600 * 1_000_000_000
		default:
			return 0, fmt.Errorf("未知单位 %q", unit)
		}
	}
	return total, nil
}

func formatDuration(ns int64) string {
	if ns%3600_000_000_000 == 0 {
		return fmt.Sprintf("%dh", ns/3600_000_000_000)
	}
	if ns%60_000_000_000 == 0 {
		return fmt.Sprintf("%dm", ns/60_000_000_000)
	}
	if ns%1_000_000_000 == 0 {
		return fmt.Sprintf("%ds", ns/1_000_000_000)
	}
	return fmt.Sprintf("%dms", ns/1_000_000)
}

// ParseByteSize -- "512MB" / "2GB" / "1048576"。
func ParseByteSize(s string) (int64, error) {
	orig := strings.TrimSpace(s)
	s = strings.ToUpper(orig)
	mult := int64(1)
	for _, m := range []struct {
		suf string
		val int64
	}{
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	} {
		if strings.HasSuffix(s, m.suf) {
			mult = m.val
			s = strings.TrimSuffix(s, m.suf)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	// 负值没有合法语义（高水位、文件上限等都是"越大越晚触发"），
	// 一旦混进来会让阈值判断整体失效，必须在解析处就拒绝。
	if n < 0 {
		return 0, fmt.Errorf("大小不能为负：%q", orig)
	}
	return int64(n * float64(mult)), nil
}

// Config -- 顶层配置（字段对齐 design §14；未实现的项有意省略而非留空占位）。
type Config struct {
	Pair     PairConfig     `yaml:"pair"` // * 必填：本地根目录 <-> 远端网盘目录
	Detector DetectorConfig `yaml:"detector"`
	Sender   SenderConfig   `yaml:"sender"`
	Quota    QuotaConfig    `yaml:"quota"`
	Policy   PolicyConfig   `yaml:"policy"`
	Mirror   MirrorConfig   `yaml:"mirror"`
	Exclude  []string       `yaml:"exclude"`
	Service  ServiceConfig  `yaml:"service"`
	Web      WebConfig      `yaml:"web"`
}

// MirrorConfig -- 镜像模式（folderRA == folderLA + 远端历史）。
//
// 语义：主树**路径清单**永远等于本地清单；所有会被镜像操作销毁的远端旧版本
// 先归档进 history_dir（服务器端 Move，零带宽），到期整目录清理。
// 默认开启（用户拍板 2026-10）；关闭后回退 design 原始"只备份不删"语义。
type MirrorConfig struct {
	Enabled         bool     `yaml:"enabled"`
	HistoryDir      string   `yaml:"history_dir"`      // 远端历史根目录（RA 内，单级目录名）
	HistoryDays     int      `yaml:"history_days"`     // 保留期；0 = 永久保留
	OnRename        string   `yaml:"on_rename"`        // mv（远端直接改名）| history（归档+重传）
	RemoteReconcile Duration `yaml:"remote_reconcile"` // 远端清单对账周期（多余路径 → 归档）

	// historyDaysSet -- mirror.history_days 是否**在原始 YAML 中显式出现**。
	// 非导出：不参与 yaml.Marshal，也不对外暴露。用来把「用户显式写了」
	// 与「用户没写、取的是 Default() 预填值」区分开（数值可能一样，意图不同）。
	historyDaysSet bool
}

type DetectorConfig struct {
	Mode              string   `yaml:"mode"` // auto | usn | poll
	PollInterval      Duration `yaml:"poll_interval"`
	UsnPollQuiet      Duration `yaml:"usn_poll_quiet"`
	UsnPollActive     Duration `yaml:"usn_poll_active"`
	ReconcileInterval Duration `yaml:"reconcile_interval"`
}

type SenderConfig struct {
	FlushInterval  Duration `yaml:"flush_interval"`
	FlushBytes     ByteSize `yaml:"flush_bytes"`
	MinGap         Duration `yaml:"min_gap"`
	Concurrency    int      `yaml:"concurrency"`
	SettleDuration Duration `yaml:"settle_duration"`
	Policy         string   `yaml:"policy"` // overwrite | skip
}

type QuotaConfig struct {
	IdleThreshold Duration `yaml:"idle_threshold"`
	// HistoryDays -- 远端历史保留天数。**权威副本在 Mirror.HistoryDays**；
	// 这里只是让它能写在 idle_threshold 旁边（都是"多久"类旋钮，挤在一起更好找）。
	// 它是结构体已知字段，故严格模式下也能解析进来；normalizeHistoryDays() 负责同步回 Mirror。
	HistoryDays  int `yaml:"history_days"`
	QuietCPUCap  int `yaml:"quiet_cpu_cap"`  // %
	ActiveCPUCap int `yaml:"active_cpu_cap"` // %
}

// defaultHistoryDays -- Default() 预填的保留期（两处都没写时生效）。
const defaultHistoryDays = 90

// normalizeHistoryDays -- 抹平 history_days 的两个位置。
//
// 为什么需要：yaml.v3 默认非 strict，写在 quota: 下的 history_days 若不显式接收会被
// **静默丢弃**（不报错、不生效）----用户以为改了其实没改。这里让两处都能写，
// 且以 Mirror.HistoryDays 为准（它是真正的消费者，PruneHistory 读的就是它）。
//
// 优先级判据用「键是否显式出现」（historyDaysSet）而非「值是否 ≠ 默认值」：
// 后者会把用户在 mirror 下显式写的 0（永久保留）或恰好等于默认值的 90 误判成"没写"，
// 继而被 quota.history_days 静默覆盖。historyDaysSet 由 Load / Parse 解出原始 YAML 后置位。
func (c *Config) normalizeHistoryDays() {
	switch {
	case c.Mirror.historyDaysSet:
		// mirror 下显式写过 history_days（含 0=永久保留、或恰好等于默认值 90）→ 以它为准
	case c.Quota.HistoryDays > 0:
		// mirror 未写、quota 写了正值 → 退到 quota
		c.Mirror.HistoryDays = c.Quota.HistoryDays
	}
	c.Quota.HistoryDays = c.Mirror.HistoryDays // 回写，保证两处显示一致
}

// yamlKeyPresent -- 判断原始 YAML 中 section.key 是否**显式出现**。
//
// 与「值等于默认值」不同：用于区分「用户显式写了」与「取的是 Default() 预填值」，
// 二者数值可能完全相同但意图不同（见 normalizeHistoryDays）。
// 解析失败时不报错（正式解码处会报），这里只做尽力探测。
func yamlKeyPresent(data []byte, section, key string) bool {
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return false
	}
	sec, ok := m[section].(map[string]any)
	if !ok {
		return false
	}
	_, ok = sec[key]
	return ok
}

type PolicyConfig struct {
	PropagateDelete bool `yaml:"propagate_delete"` // 设计上必须 false（备份语义，design §9.2）
	RapidUpload     bool `yaml:"rapid_upload"`
}

// PairConfig -- 备份对（**必填**）：本地只读根目录 <-> 远端网盘目录。
// 刻意不进 flag、不给默认值：备份目标必须是显式声明的事实，
// 缺任何一项 → 启动即拒绝（避免把 83 GB 传到猜错的地方）。
type PairConfig struct {
	LocalRoot string `yaml:"local_root"` // 本地根目录（只读）
	Remote    string `yaml:"remote"`     // 远端网盘目录（/开头）
}

type ServiceConfig struct {
	DBPath   string `yaml:"db_path"`
	LogFile  string `yaml:"log_file"`
	LogMaxMB int    `yaml:"log_max_mb"`
	LogKeep  int    `yaml:"log_keep"`
	// Initialized -- 首次配置完成标记（向导保存时置 true）。
	//
	// 有它：双击直接进服务态，凭据过期时进控制台用「凭据」卡片修复，不再走一遍向导。
	// 没它（老配置）：只要配置文件存在且 pair 齐全，也按已初始化处理。
	// 语义只问"填过没有"，不问"能不能跑起来"----后者由运行时把错误显示在看板上。
	Initialized bool `yaml:"initialized"`
}

// WebConfig -- 看板（design §13：SSE、127.0.0.1、默认关闭、1Hz 上限）。
type WebConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"` // 必须是回环地址（Validate 强制）
}

// Default -- design §14 的默认值。
func Default() *Config {
	return &Config{
		Detector: DetectorConfig{
			Mode:              "auto",
			PollInterval:      Duration{d: 5 * 60 * 1_000_000_000},
			UsnPollQuiet:      Duration{d: 5 * 60 * 1_000_000_000},
			UsnPollActive:     Duration{d: 10 * 1_000_000_000},
			ReconcileInterval: Duration{d: 24 * 3600 * 1_000_000_000},
		},
		Sender: SenderConfig{
			FlushInterval:  Duration{d: 5 * 60 * 1_000_000_000},
			FlushBytes:     ByteSize(2 << 30),
			MinGap:         Duration{d: 30 * 1_000_000_000},
			Concurrency:    12,
			SettleDuration: Duration{d: 10 * 1_000_000_000},
			Policy:         "overwrite",
		},
		Quota: QuotaConfig{
			IdleThreshold: Duration{d: 5 * 60 * 1_000_000_000},
			QuietCPUCap:   5,
			ActiveCPUCap:  80,
		},
		Policy: PolicyConfig{
			PropagateDelete: false,
			RapidUpload:     true,
		},
		Mirror: MirrorConfig{
			Enabled:         true, // 默认开启镜像+历史（用户拍板）
			HistoryDir:      ".history",
			HistoryDays:     defaultHistoryDays,
			OnRename:        "mv",
			RemoteReconcile: Duration{d: 24 * 3600 * 1_000_000_000},
		},
		Service: ServiceConfig{
			// 默认就地部署：索引库与二进制同目录，日志在 logs/ 下
			DBPath:   "index.db",
			LogFile:  "logs/bdsync.log",
			LogMaxMB: 10,
			LogKeep:  7,
		},
		Web: WebConfig{
			Enabled: false, // design §13：默认关闭，--web 或配置开启
			Addr:    "127.0.0.1:8787",
		},
	}
}

// Load -- 读取 YAML 配置；path 为空返回默认值。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	// 严格模式：拼错的键（如 flush_bytez）必须报错，而不是被静默忽略。
	// 静默忽略的代价是"用户以为改了，其实没改"----比启动失败难查得多。
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	// history_days 优先级看「键是否显式出现」而非值大小（详见 normalizeHistoryDays）
	cfg.Mirror.historyDaysSet = yamlKeyPresent(data, "mirror", "history_days")
	cfg.normalizeHistoryDays()
	// 相对路径以**配置文件所在目录**为基准（不是 CWD）----
	// 这样"exe + bdsync.yaml + var/"整个文件夹可以放任意位置、从任意目录启动。
	resolveAgainst(filepath.Dir(path), cfg)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置 %s 无效: %w", path, err)
	}
	return cfg, nil
}

// resolveAgainst -- 把配置里的相对路径锚定到 baseDir（配置文件所在目录）。
// 绝对路径原样保留；空值保持空（未配置日志文件 = 只打 stdout）。
func resolveAgainst(baseDir string, cfg *Config) {
	fix := func(p *string) {
		v := strings.TrimSpace(*p)
		if v == "" || filepath.IsAbs(v) {
			return
		}
		*p = filepath.Join(baseDir, v)
	}
	fix(&cfg.Service.DBPath)
	fix(&cfg.Service.LogFile)
	fix(&cfg.Pair.LocalRoot) // 本地根目录也允许相对配置写（如 ../资料）
	if l := strings.TrimSpace(cfg.Pair.LocalRoot); l != "" {
		// 规范化：D:/x == D:\x（后续所有比较都不再受书写风格影响）
		cfg.Pair.LocalRoot = filepath.Clean(l)
	}
	cfg.Pair.Remote = strings.TrimSpace(cfg.Pair.Remote)
}

// ValidatePair -- 备份对必须显式声明（配置驱动的核心约束）。
// 缺任一项都在启动阶段拦下：备份目标不能靠猜。
func (c *Config) ValidatePair() error {
	var missing []string
	if strings.TrimSpace(c.Pair.LocalRoot) == "" {
		missing = append(missing, "pair.local_root")
	}
	if c.Pair.Remote == "" {
		missing = append(missing, "pair.remote")
	}
	if len(missing) > 0 {
		return fmt.Errorf("配置缺少必填项：%s\n"+
			"  备份对必须显式声明 ---- bdsync 拒绝在目标不明时启动。\n"+
			"  示例：\n"+
			"    pair:\n"+
			"      local_root: \"D:/YourData\"         # 本地根目录（严格只读）\n"+
			"      remote: \"/我的备份\"              # 网盘内路径，必须以 / 开头",
			strings.Join(missing, "、"))
	}
	if !strings.HasPrefix(c.Pair.Remote, "/") {
		return fmt.Errorf("pair.remote 必须是网盘内路径（以 / 开头），得到 %q", c.Pair.Remote)
	}
	return nil
}

// Validate -- 语义校验（把"改了会出事"的配置在启动时拦住）。
func (c *Config) Validate() error {
	if c.Policy.PropagateDelete {
		return fmt.Errorf("policy.propagate_delete 必须为 false：这是备份工具，" +
			"传播删除会让备份失去意义（design §9.2）；清理远端请用显式 --prune（未实现）")
	}
	switch c.Detector.Mode {
	case "auto", "usn", "poll", "":
	default:
		return fmt.Errorf("detector.mode 只能是 auto|usn|poll，得到 %q", c.Detector.Mode)
	}
	switch c.Sender.Policy {
	case "overwrite", "skip", "rsync", "":
	default:
		return fmt.Errorf("sender.policy 只能是 overwrite|skip|rsync，得到 %q", c.Sender.Policy)
	}
	if c.Sender.Concurrency < 0 {
		return fmt.Errorf("sender.concurrency 不能为负")
	}
	// flush_bytes 是高水位：<=0 会让"队列超此立即触发"永不成立（或立即触发），
	// 静默改变发送节奏，启动时就拦住。
	if c.Sender.FlushBytes <= 0 {
		return fmt.Errorf("sender.flush_bytes 必须大于 0，得到 %d", c.Sender.FlushBytes.Int64())
	}
	// 镜像模式（design 增补：镜像+历史）
	switch c.Mirror.OnRename {
	case "mv", "history", "":
	default:
		return fmt.Errorf("mirror.on_rename 只能是 mv|history，得到 %q", c.Mirror.OnRename)
	}
	if c.Mirror.HistoryDays < 0 {
		return fmt.Errorf("mirror.history_days 不能为负")
	}
	hd := strings.TrimSpace(c.Mirror.HistoryDir)
	if hd == "" || strings.ContainsAny(hd, `/\`) || hd == "." || hd == ".." {
		return fmt.Errorf("mirror.history_dir 必须是单级目录名（如 .history），得到 %q", c.Mirror.HistoryDir)
	}
	// design §13：看板只允许监听回环地址，绝不对外暴露
	if c.Web.Enabled {
		if err := validateLoopbackAddr(c.Web.Addr); err != nil {
			return err
		}
	}
	return nil
}

// validateLoopbackAddr -- addr 必须是 127.0.0.1/localhost/::1。
func validateLoopbackAddr(addr string) error {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
		// 简单处理 [::1]:port
		host = strings.Trim(host, "[]")
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "":
		return nil
	}
	// 允许 127.x.x.x 网段
	if strings.HasPrefix(host, "127.") {
		return nil
	}
	return fmt.Errorf("web.addr 必须是回环地址（design §13 只监听 127.0.0.1），得到 %q", addr)
}

// Parse -- 解析 YAML 字节（Web 配置保存接口用；含校验）。
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	// 与 Load 一致：看板「YAML 全文」保存入口同样启用严格模式，拦住拼错的键。
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("YAML 解析失败: %w", err)
	}
	cfg.Mirror.historyDaysSet = yamlKeyPresent(data, "mirror", "history_days")
	cfg.normalizeHistoryDays()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// FindDefault -- 未显式指定 --config 时的探测顺序（先到先得）：
//
//  1. <exe 所在目录>/bdsync.yaml   ← 便携式：配置与二进制同目录，放哪都能跑
//  2. ./bdsync.yaml                ← 开发态：在仓库根目录直接跑
//  3. ./var/bdsync.yaml
//  4. <exe 所在目录>/var/bdsync.yaml
//
// exe 目录优先于 CWD：跟二进制一起走的配置才是用户预期的那个
// （否则把 bin/ 拷到别处时会静默读到另一份配置）。
func FindDefault() string {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands, filepath.Join(dir, "bdsync.yaml"),
			filepath.Join(dir, "var", "bdsync.yaml"))
	}
	cands = append(cands, "bdsync.yaml", filepath.Join("var", "bdsync.yaml"))
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
