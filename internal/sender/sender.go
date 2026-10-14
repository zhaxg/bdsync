// Package sender — 上传发送端（design §7/§9/§12）。
//
// 单文件流程（Phase 1 实测通过）：
//
//	稳定性闸门 → 哈希(MD5/SliceMD5/CRC32/分块MD5) → 秒传V2 → 未命中: 分片上传 → 合并
//	→ fix-md5 → meta 回写
//
// Phase 3：并发 + AIMD 自适应限流 + 断点续传（upload_id + done_blocks）。
//
// 硬性约束：
//   - 不传播删除（远端保留），本包没有删除远端的代码路径
//   - 所有不确定情况偏向重传（design §9.3）
//   - 退避必须带 full jitter（design §12，防惊群）
package sender

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
	"github.com/qjfoidnh/BaiduPCS-Go/baidupcs/pcserror"
	"github.com/qjfoidnh/BaiduPCS-Go/pcsutil/checksum"

	"syncer/internal/baidu"
	"syncer/internal/index"
)

// defaultContentSize — 秒传V2 的防伪切片长度（上游 DefaultContentSize = 4KB）。
const defaultContentSize = 4 * 1024

// BlockSize — 分块大小（上游 getBlockSize 阈值：<8GB→4MB，≥8GB→16MB，≥32GB→64MB）。
func BlockSize(fileSize int64) int64 {
	switch {
	case fileSize >= baidupcs.MaxUploadThreshold:
		return baidupcs.MaxUploadBlockSize
	case fileSize >= baidupcs.MiddleUploadThreshold:
		return baidupcs.MiddleUploadBlockSize
	default:
		return baidupcs.MinUploadBlockSize
	}
}

// Action — 错误分类结果（design §12）。
type Action int

const (
	ActionRetry    Action = iota // 抖动/限流 → 退避重试
	ActionFatal                  // 登录过期/超额 → 停止排空
	ActionFail                   // 本文件失败 → 记 ERROR，继续其余
	ActionDone                   // 完成（含秒传命中/远端已满足）
	ActionDeferred               // 稳定性闸门推迟（不算失败）
)

// isRateLimited — 远端「请求过于频繁」（errno 6）。
func isRateLimited(err error) bool {
	pe, ok := baidu.AsPCSError(err)
	return ok && pe.GetErrType() == pcserror.ErrTypeRemoteError && pe.GetRemoteErrCode() == 6
}

// Classify — 把 SDK 错误映射为处理动作（design §12 错误分类表 + fork 实际错误码）。
func Classify(err error) Action {
	if err == nil {
		return ActionDone
	}
	pe, ok := baidu.AsPCSError(err)
	if !ok {
		return ActionFail // 本地 IO/哈希错误
	}
	switch pe.GetErrType() {
	case pcserror.ErrTypeNetError:
		if strings.Contains(pe.Error(), "413") {
			return ActionFail // 请求体过大：重试无意义
		}
		return ActionRetry
	case pcserror.ErrTypeRemoteError:
		switch pe.GetRemoteErrCode() {
		case 31045:
			return ActionFatal // 登录过期
		case 31112:
			return ActionFatal // 网盘空间超额
		case 6:
			return ActionRetry // 请求过于频繁
		case 31363:
			return ActionRetry // superfile2 block miss：上传状态失效
		case 31079, 2:
			return ActionDone // 秒传未命中/边缘输入 → 调用方转分片（不是错误）
		case 31061:
			return ActionDone // 文件已存在
		case 31066, -9:
			return ActionFail // 路径不存在
		case 114514, 1919810:
			return ActionDone // fork 的 skip/rsync 语义：远端已满足
		default:
			return ActionFail
		}
	default:
		return ActionFail
	}
}

// Stats — 一次 Drain 的统计（并发安全）。
type Stats struct {
	mu         sync.Mutex
	Claimed    int
	RapidOK    int // 秒传命中
	Uploaded   int // 分片上传成功
	Archived   int // 镜像归档（服务器端 Move：历史+主树删除一步完成）
	Renamed    int // 改名远端同步（mv；含 history 策略的降级计数）
	Deferred   int // 推迟（不算失败）
	Failed     int
	RemoteHave int   // 远端已存在/未变化，跳过
	Bytes      int64 // 实际传输字节（不含秒传）
	Elapsed    time.Duration
	FatalError error
}

func (s *Stats) apply(f func(*Stats)) {
	s.mu.Lock()
	f(s)
	s.mu.Unlock()
}

// Snapshot — 取一份无锁快照（逐字段拷贝，避免复制 mutex）。
func (s *Stats) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Claimed:    s.Claimed,
		RapidOK:    s.RapidOK,
		Uploaded:   s.Uploaded,
		Archived:   s.Archived,
		Renamed:    s.Renamed,
		Deferred:   s.Deferred,
		Failed:     s.Failed,
		RemoteHave: s.RemoteHave,
		Bytes:      s.Bytes,
		Elapsed:    s.Elapsed,
		FatalError: s.FatalError,
	}
}

// Sender — 上传器（Phase 3 并发；参数支持看板在线调整）。
type Sender struct {
	st           *index.Store
	bc           *baidu.Client
	localRoot    string
	remoteRoot   string
	maxAttempt   int // 单文件在一次 Drain 内的最大尝试次数（启动后不变）
	remoteDirs   map[string]bool
	remoteDirsMu sync.Mutex
	logf         func(format string, args ...any)
	lim          *limiter

	// 在线可调参数（SetLive 更新；读取方用 getter，避免数据竞争）
	prmu    sync.RWMutex
	settleD time.Duration
	policyS string

	// 镜像模式（启动后不变）
	mirrorOn    bool
	historyDir  string
	historyDays int
	onRename    string // mv（默认）| history（降级：归档+重传）
}

// Options — Sender 参数。
type Options struct {
	LocalRoot   string
	RemoteRoot  string
	Policy      string // overwrite | skip
	Settle      time.Duration
	MaxAttempt  int
	Concurrency int    // 最大并发（AIMD 上限；起步取 min(12, 上限)，design §12）
	MirrorOn    bool   // 镜像模式：覆盖/删除前先归档旧版（config.Mirror.Enabled）
	HistoryDir  string // 远端历史目录（remoteRoot 内的单级目录名）
	HistoryDays int    // 历史保留天数（0 = 永久）
	OnRename    string // mv（远端直接改名）| history（归档+重传）
	Logf        func(format string, args ...any)
}

// New — 构造 Sender。
func New(st *index.Store, bc *baidu.Client, opts Options) *Sender {
	if opts.Policy == "" {
		opts.Policy = baidu.PolicyOverwrite
	}
	if opts.Settle <= 0 {
		opts.Settle = 10 * time.Second
	}
	if opts.MaxAttempt <= 0 {
		opts.MaxAttempt = 3
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 12 // design §12：起步给足，SVIP 实测无感
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Sender{
		st:          st,
		bc:          bc,
		localRoot:   opts.LocalRoot,
		remoteRoot:  strings.TrimRight(opts.RemoteRoot, "/"),
		policyS:     opts.Policy,
		settleD:     opts.Settle,
		maxAttempt:  opts.MaxAttempt,
		mirrorOn:    opts.MirrorOn,
		historyDir:  strings.Trim(opts.HistoryDir, "/"),
		historyDays: opts.HistoryDays,
		onRename:    opts.OnRename,
		remoteDirs:  map[string]bool{},
		logf:        opts.Logf,
		lim:         newLimiter(min(12, opts.Concurrency), opts.Concurrency, opts.Logf),
	}
}

// SetLive — 看板在线调整：settle / 并发上限 / 同名策略（design §13 基本配置）。
func (s *Sender) SetLive(settle time.Duration, concurrency int, policy string) {
	if settle > 0 {
		s.prmu.Lock()
		s.settleD = settle
		s.prmu.Unlock()
	}
	if concurrency > 0 {
		s.lim.setMax(concurrency)
	}
	switch policy {
	case "overwrite", "skip", "rsync":
		s.prmu.Lock()
		s.policyS = policy
		s.prmu.Unlock()
	}
}

func (s *Sender) getSettle() time.Duration {
	s.prmu.RLock()
	defer s.prmu.RUnlock()
	return s.settleD
}

func (s *Sender) getPolicy() string {
	s.prmu.RLock()
	defer s.prmu.RUnlock()
	return s.policyS
}

// Drain — 领取并上传 pending 文件，直到队列排空或达到 maxFiles（0=不限）。
// 并发模型：每轮领取 ≤ 2×并发额度，worker 池处理（design §7 + Phase 3）。
func (s *Sender) Drain(ctx context.Context, maxFiles int) (*Stats, error) {
	start := time.Now()
	stats := &Stats{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 复位已过 settle 期的 UNSTABLE（design §9.1）
	if _, err := s.st.RequeueSettled(ctx, s.getSettle()); err != nil {
		return stats, err
	}

	// ── 镜像模式：历史过期清理（kv 守卫，最多每日一次；内部自行记日志）──
	if s.mirrorOn && s.historyDays > 0 {
		s.pruneIfDue(ctx)
	}

	// ── 镜像模式：改名阶段（远端直接 mv：零污染零带宽；design镜像 Q1）──
	if s.mirrorOn {
		if err := s.drainRenames(ctx, stats); err != nil {
			stats.Elapsed = time.Since(start)
			return stats.Result(), err
		}
	}

	// ── 镜像模式：历史归档阶段（本地已删 → 远端旧版移入 .history）──
	// 归档 = 服务器端 Move：一次调用同时完成"历史写入 + 主树删除"，零带宽。
	if s.mirrorOn {
		if err := s.drainHistory(ctx, stats); err != nil {
			stats.Elapsed = time.Since(start)
			return stats.Result(), err
		}
	}

	var (
		failedMu sync.Mutex
		failed   = map[string]bool{} // 本轮已失败的文件不重复领取
	)
	markFailed := func(rel string) {
		failedMu.Lock()
		failed[rel] = true
		failedMu.Unlock()
	}
	isFailed := func(rel string) bool {
		failedMu.Lock()
		defer failedMu.Unlock()
		return failed[rel]
	}

	for {
		if stats.FatalError != nil {
			break
		}
		if err := ctx.Err(); err != nil {
			stats.Elapsed = time.Since(start)
			return stats.Result(), err
		}
		claimSize := s.lim.limit() * 2
		if claimSize < 8 {
			claimSize = 8
		}
		if maxFiles > 0 {
			remain := maxFiles - stats.Claimed
			if remain <= 0 {
				break
			}
			if claimSize > remain {
				claimSize = remain
			}
		}

		batch, err := s.st.ClaimBatch(ctx, claimSize)
		if err != nil {
			stats.Elapsed = time.Since(start)
			return stats.Result(), err
		}
		if len(batch) == 0 {
			break
		}

		if err := s.ensureDirs(ctx, batch); err != nil {
			for _, e := range batch {
				s.st.DeferEntry(ctx, e.RelPath)
			}
			stats.Elapsed = time.Since(start)
			return stats.Result(), fmt.Errorf("创建远端目录失败: %w", err)
		}

		var wg sync.WaitGroup
		i := 0
		for ; i < len(batch); i++ {
			if ctx.Err() != nil {
				break
			}
			if isFailed(batch[i].RelPath) {
				// 本轮已失败过：转 UNSTABLE，下次 drain 再试
				s.st.DeferEntry(ctx, batch[i].RelPath)
				continue
			}
			e := batch[i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				if !s.lim.wait(ctx) {
					return // ctx 取消：IN_PROGRESS 由下次 ClaimBatch 复位（design §9.3）
				}
				defer s.lim.release()
				if ctx.Err() != nil {
					return
				}
				a := s.uploadOne(ctx, e, stats)
				switch a {
				case ActionFatal:
					cancel()
				case ActionFail:
					markFailed(e.RelPath)
				}
			}()
		}
		wg.Wait()
		// 未处理的退回队列（UNSTABLE，本轮不再领取）
		for _, rest := range batch[i:] {
			s.st.DeferEntry(ctx, rest.RelPath)
		}
	}

	stats.Elapsed = time.Since(start)
	return stats.Result(), nil
}

// Result — 结束时取快照（供调用方持有的最终统计）。
func (s *Stats) Result() *Stats {
	out := s.Snapshot()
	return &out
}

// ensureDirs — 为批次内文件创建远端父目录（深度排序，父先于子）。串行调用。
func (s *Sender) ensureDirs(ctx context.Context, batch []index.PendingEntry) error {
	s.remoteDirsMu.Lock()
	defer s.remoteDirsMu.Unlock()
	dirs := map[string]bool{"/": true, s.remoteRoot: true}
	for _, e := range batch {
		d := path.Dir(path.Join(s.remoteRoot, e.RelPath))
		for d != "/" && d != "." && d != "" && !dirs[d] {
			dirs[d] = true
			d = path.Dir(d)
		}
	}
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		if d == "/" || s.remoteDirs[d] {
			continue
		}
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool {
		return strings.Count(list[i], "/") < strings.Count(list[j], "/")
	})
	for _, d := range list {
		if err := s.bc.Mkdir(d); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
		s.remoteDirs[d] = true
	}
	return nil
}

// historySrcRel — 归档源路径：改名后又删除的行，远端副本还在改名前的位置。
func historySrcRel(it index.HistoryItem) string {
	if it.OldPath != "" && it.OldPath != it.RelPath {
		return it.OldPath
	}
	return it.RelPath
}

// drainHistory — 归档队列排空（串行阶段；限流走 AIMD 钩子）。
// 注意：归档失败**不改行状态**（保持 REMOVED_LOCAL + r_fsid，下轮自动重领），
// 也绝不 FailUpload —— 那会把"本地已删"翻成 ERROR 并被当成待传文件重传。
func (s *Sender) drainHistory(ctx context.Context, stats *Stats) error {
	tried := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		items, err := s.st.HistoryQueue(ctx, 200)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		newAny := false
		for _, it := range items {
			if tried[it.RelPath] {
				continue // 本轮已失败过：毒丸条目不打转
			}
			newAny = true
			tried[it.RelPath] = true

			archived, aerr := s.archiveRemote(ctx, it.RelPath, historySrcRel(it), it.RMtime)
			if aerr != nil {
				if isRateLimited(aerr) {
					s.lim.onRateLimit()
				}
				stats.apply(func(st *Stats) { st.Failed++ })
				s.logf("[!] 归档失败（保持队列，下轮重试）%s: %v", it.RelPath, aerr)
				continue
			}
			if archived {
				if err := s.st.ClearRemote(ctx, it.RelPath); err != nil {
					return err // 记账失败：下轮幂等重放（目标已存在 → 补账）
				}
				stats.apply(func(st *Stats) { st.Archived++ })
				s.logf("[·] %s → .history/%s/", it.RelPath, historyDateBucket())
			} else {
				// 源已不在 → 补记账
				if err := s.st.ClearRemote(ctx, it.RelPath); err != nil {
					return err
				}
			}
		}
		if !newAny {
			return nil
		}
	}
}

// uploadOne — 单文件上传全流程（可并发调用）。
func (s *Sender) uploadOne(ctx context.Context, e index.PendingEntry, stats *Stats) Action {
	settleD := s.getSettle() // 捕获本轮参数（看板可在线调整）
	policy := s.getPolicy()
	stats.apply(func(st *Stats) { st.Claimed++ })
	full := filepath.Join(s.localRoot, filepath.FromSlash(e.RelPath))
	remotePath := path.Join(s.remoteRoot, e.RelPath)

	finish := func(fi *baidu.FileInfo, rapid bool) Action {
		if err := s.st.FinishUpload(ctx, e.RelPath, fi.FsID, fi.Size, fi.Mtime, fi.MD5); err != nil {
			s.logf("[!] 回写索引失败 %s: %v", e.RelPath, err)
		}
		if rapid {
			stats.apply(func(st *Stats) { st.RapidOK++ })
			s.logf("[+] %s  (秒传)", e.RelPath)
		} else {
			stats.apply(func(st *Stats) { st.Uploaded++; st.Bytes += e.Size })
			s.logf("[+] %s  (上传)", e.RelPath)
		}
		s.lim.onSuccess()
		return ActionDone
	}
	fail := func(err error, action Action) Action {
		if isRateLimited(err) {
			s.lim.onRateLimit()
		}
		stats.apply(func(st *Stats) { st.Failed++ })
		s.st.FailUpload(ctx, e.RelPath, err.Error())
		s.logf("[-] %s: %v", e.RelPath, err)
		return action
	}
	deferEntry := func(reason string) Action {
		stats.apply(func(st *Stats) { st.Deferred++ })
		s.st.DeferEntry(ctx, e.RelPath)
		s.logf("[~] %s 推迟（%s）", e.RelPath, reason)
		return ActionDeferred
	}

	// ── 1. 稳定性闸门（design §9.1）：发送前重新 stat ──
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			s.st.MarkMissing(ctx, 0, []string{e.RelPath})
			stats.apply(func(st *Stats) { st.RemoteHave++ })
			s.logf("[x] %s 本地已删除，跳过（远端旧版本保留）", e.RelPath)
			return ActionDone
		}
		return deferEntry("stat 失败: " + err.Error())
	}
	if info.Size() != e.Size || info.ModTime().UnixNano() != e.MtimeNs {
		return deferEntry("仍在写入（size/mtime 变化）")
	}
	if age := time.Since(time.Unix(0, e.SettledNs)); age < settleD {
		return deferEntry(fmt.Sprintf("settle 未到（%.0fs < %.0fs）", age.Seconds(), settleD.Seconds()))
	}

	// ── 1.5 镜像模式：覆盖前先归档远端旧版（服务器端 Move，零带宽）──
	// 归档失败绝不继续上传 —— 旧版还在主树时不能用新内容覆盖（历史优先）。
	if s.mirrorOn && e.HasRemote {
		archived, aerr := s.archiveRemote(ctx, e.RelPath, e.RelPath, e.RMtime)
		if aerr != nil {
			if isRateLimited(aerr) {
				s.lim.onRateLimit()
			}
			return fail(fmt.Errorf("覆盖前归档旧版失败: %w", aerr), ActionFail)
		}
		if archived {
			if err := s.st.ClearRemote(ctx, e.RelPath); err != nil {
				s.logf("[!] %s 归档记账失败（下轮幂等补账）: %v", e.RelPath, err)
			}
			e.HasRemote = false
			stats.apply(func(st *Stats) { st.Archived++ })
			s.logf("[·] %s 旧版本已归档 → .history/%s/", e.RelPath, historyDateBucket())
		}
	}

	// ── 1.6 0 字节文件：无法秒传（无内容匹配）也无法分片（无 block），单请求直传 ──
	if e.Size == 0 {
		f, oerr := os.Open(full)
		if oerr != nil {
			return deferEntry("打开失败: " + oerr.Error())
		}
		derr := s.bc.UploadSingle(remotePath, policy, f, 0)
		f.Close()
		if derr != nil {
			if isRateLimited(derr) {
				s.lim.onRateLimit()
			}
			return fail(fmt.Errorf("直传失败: %w", derr), ActionFail)
		}
		fi, merr := s.bc.Meta(remotePath)
		if merr != nil {
			return fail(fmt.Errorf("上传成功但取元信息失败: %w", merr), ActionFail)
		}
		return finish(fi, false)
	}

	// ── 2. 哈希（design §7 步骤2）──
	lfc := checksum.NewLocalFileChecksum(full, int(baidupcs.SliceMD5Size))
	if err := lfc.OpenPath(); err != nil {
		return deferEntry("打开失败: " + err.Error()) // 含占用中：defer 不算失败
	}
	defer lfc.Close()
	if err := lfc.Sum(checksum.CHECKSUM_MD5 | checksum.CHECKSUM_SLICE_MD5 | checksum.CHECKSUM_CRC32); err != nil {
		return fail(fmt.Errorf("哈希失败: %w", err), ActionFail)
	}
	if lfc.Length != e.Size {
		return deferEntry("哈希期间文件大小变化")
	}
	blockSize := BlockSize(lfc.Length)
	if err := lfc.CalculateChunkedSum(blockSize); err != nil {
		return fail(fmt.Errorf("分块哈希失败: %w", err), ActionFail)
	}
	contentMD5 := hex.EncodeToString(lfc.MD5)
	sliceMD5 := hex.EncodeToString(lfc.SliceMD5)
	if contentMD5 == "" && lfc.Length == 0 {
		// 空文件：checksum 包对 0 字节可能不产出 MD5，用标准空内容 md5 兜底
		contentMD5 = baidupcs.EmptyContentMD5
	}
	s.st.SetLocalMD5(ctx, e.RelPath, contentMD5)

	// ── 3. 秒传V2（design §7 步骤3）──
	uk, err := s.bc.UK()
	if err != nil {
		stats.apply(func(st *Stats) { st.FatalError = err })
		s.st.FailUpload(ctx, e.RelPath, err.Error())
		return ActionFatal
	}
	dataTime := time.Now().Unix()
	offset, err := createDataOffset(contentMD5, uk, dataTime, lfc.Length, defaultContentSize)
	if err != nil {
		return fail(fmt.Errorf("计算防伪偏移失败: %w", err), ActionFail)
	}
	var dataContent []byte
	var dataLength int64
	if lfc.Length > 0 {
		dataContent, dataLength, err = lfc.GetSliceDataContent(offset, defaultContentSize)
		if err != nil {
			return fail(fmt.Errorf("读取防伪切片失败: %w", err), ActionFail)
		}
	}
	b64Content := strings.TrimRight(base64.StdEncoding.EncodeToString(dataContent[:dataLength]), "=")
	crc32Str := fmt.Sprint(lfc.CRC32) // SDK 在 Sum 时已算好

	for attempt := 1; attempt <= s.maxAttempt; attempt++ {
		if err := ctx.Err(); err != nil {
			s.st.DeferEntry(ctx, e.RelPath)
			return ActionDeferred
		}

		rapidRes, rerr := s.bc.RapidUploadV2(remotePath, policy, "", contentMD5, sliceMD5, b64Content, crc32Str,
			offset, dataLength, lfc.Length, dataTime, lfc.BlocksList)

		switch {
		case rerr == nil && rapidRes.ReturnType == 2:
			return finish(rapidInfo(rapidRes, contentMD5, lfc.Length), true)

		case rerr == nil:
			// ReturnType==1：需要真实上传
			s.logf("[~] %s 秒传未命中，转分片上传", e.RelPath)

		default:
			code := 0
			if pe, ok := baidu.AsPCSError(rerr); ok {
				code = pe.GetRemoteErrCode()
			}
			switch code {
			case 31079, 2:
				// 31079=秒传未命中；2=服务端对边缘输入的通用报错 → 都降级分片
				s.logf("[~] %s 秒传未命中(code=%d)，转分片上传", e.RelPath, code)
			case 31061, 1919810:
				// 远端已存在/内容未变化 → 按成功处理
				stats.apply(func(st *Stats) { st.RemoteHave++ })
				fi, merr := s.bc.Meta(remotePath)
				if merr != nil {
					return fail(fmt.Errorf("远端已存在但取元信息失败: %w", merr), ActionFail)
				}
				s.logf("[=] %s 远端已存在/未变化", e.RelPath)
				return finish(fi, true)
			default:
				a := Classify(rerr)
				if a == ActionFatal {
					stats.apply(func(st *Stats) { st.FatalError = rerr })
					s.st.FailUpload(ctx, e.RelPath, rerr.Error())
					return ActionFatal
				}
				if a == ActionRetry {
					if isRateLimited(rerr) {
						s.lim.onRateLimit()
					}
					if backoffSleep(ctx, attempt) {
						continue
					}
					s.st.DeferEntry(ctx, e.RelPath)
					return ActionDeferred
				}
				return fail(rerr, ActionFail)
			}
		}

		// ── 4. 分片上传（design §7 步骤4，含断点续传）──
		uploadID := rapidRes.UploadID
		if uploadID == "" {
			fid, ferr := s.bc.FakeRapidUpload(remotePath, policy, lfc.Length)
			if ferr != nil {
				s.logf("[!] %s 获取 uploadid 失败(第%d次): %v", e.RelPath, attempt, ferr)
				if backoffSleep(ctx, attempt) {
					continue
				}
				return fail(fmt.Errorf("获取 uploadid 失败: %w", ferr), ActionFail)
			}
			uploadID = fid
		}

		// 断点续传：同一上传会话中已完成的分片直接复用（design §7）
		checksumMap := make(map[int]string, len(lfc.BlocksList))
		doneSet := map[int]bool{}
		if e.UploadID != "" && uploadID == e.UploadID && e.DoneBlocks != "" {
			var seqs []int
			if json.Unmarshal([]byte(e.DoneBlocks), &seqs) == nil {
				for _, q := range seqs {
					if q >= 0 && q < len(lfc.BlocksList) {
						doneSet[q] = true
						checksumMap[q] = lfc.BlocksList[q]
					}
				}
				if len(doneSet) > 0 {
					s.logf("[.] %s 断点续传：跳过 %d/%d 个已完成分片", e.RelPath, len(doneSet), len(lfc.BlocksList))
				}
			}
		}

		var blockErr error
		for seq := 0; seq < len(lfc.BlocksList); seq++ {
			if doneSet[seq] {
				continue
			}
			off := int64(seq) * blockSize
			length := min64(blockSize, lfc.Length-off)
			section := io.NewSectionReader(lfc.GetFile(), off, length)
			md5sum, berr := s.bc.UploadBlock(remotePath, uploadID, seq, off, section, length)
			if berr != nil {
				blockErr = fmt.Errorf("分片 %d/%d 失败: %w", seq+1, len(lfc.BlocksList), berr)
				break
			}
			if md5sum != "" && md5sum != lfc.BlocksList[seq] {
				blockErr = fmt.Errorf("分片 %d md5 不匹配（远端 %s，本地 %s）", seq+1, md5sum, lfc.BlocksList[seq])
				break
			}
			checksumMap[seq] = lfc.BlocksList[seq]
			// 断点进度持久化（design §7）：崩溃后同会话可续
			s.persistProgress(ctx, e.RelPath, uploadID, checksumMap)
		}
		if blockErr != nil {
			s.logf("[!] %s 分片上传失败(第%d次): %v", e.RelPath, attempt, blockErr)
			if isRateLimited(blockErr) {
				s.lim.onRateLimit()
			}
			if backoffSleep(ctx, attempt) {
				continue
			}
			return fail(blockErr, ActionFail)
		}

		// ── 5. 合并（design §7 步骤5）──
		if cerr := s.bc.CreateSuperFile(remotePath, uploadID, policy, lfc.Length, checksumMap); cerr != nil {
			s.logf("[!] %s 合并失败(第%d次): %v", e.RelPath, attempt, cerr)
			if a := Classify(cerr); a == ActionRetry && backoffSleep(ctx, attempt) {
				continue
			}
			return fail(fmt.Errorf("合并失败: %w", cerr), ActionFail)
		}

		// ── 5.5 修正服务端 md5 元数据（分片上传的已知服务端问题，best-effort）──
		if ferr := s.bc.FixMD5(remotePath); ferr != nil {
			s.logf("[!] %s 服务端 md5 修正失败（数据已上传，check 阶段可对账）: %v", e.RelPath, ferr)
		}

		// ── 6. 取远端元信息并回写（design §7 步骤6）──
		fi, merr := s.bc.Meta(remotePath)
		if merr != nil {
			// 已上传成功但取 meta 失败：偏向重传（design §9.3）
			if backoffSleep(ctx, attempt) {
				continue
			}
			return fail(fmt.Errorf("上传成功但取元信息失败: %w", merr), ActionFail)
		}
		return finish(fi, false)
	}

	// 单文件重试耗尽
	s.st.FailUpload(ctx, e.RelPath, fmt.Sprintf("重试耗尽（%d 次）", s.maxAttempt))
	stats.apply(func(st *Stats) { st.Failed++ })
	s.logf("[-] %s: 重试耗尽", e.RelPath)
	return ActionFail
}

// persistProgress — 把已完成分片序号（JSON 数组）连同 upload_id 落库。
func (s *Sender) persistProgress(ctx context.Context, relPath, uploadID string, done map[int]string) {
	seqs := make([]int, 0, len(done))
	for q := range done {
		seqs = append(seqs, q)
	}
	sort.Ints(seqs)
	b, err := json.Marshal(seqs)
	if err != nil {
		return
	}
	s.st.SaveProgress(ctx, relPath, uploadID, string(b))
}

// rapidInfo — 秒传响应信息 → 写回用 FileInfo；缺失字段用本地值兜底。
func rapidInfo(r *baidu.RapidResult, localMD5 string, localSize int64) *baidu.FileInfo {
	fi := &baidu.FileInfo{
		FsID:  r.FsID,
		Path:  r.Path,
		MD5:   r.MD5,
		Size:  r.Size,
		Mtime: r.Mtime,
	}
	if fi.MD5 == "" {
		fi.MD5 = localMD5
	}
	if fi.Size == 0 {
		fi.Size = localSize
	}
	if fi.Mtime == 0 {
		fi.Mtime = time.Now().Unix()
	}
	return fi
}

// createDataOffset — 秒传V2 防伪偏移（上游 creaetDataOffset 复刻）：
// offset = md5(uk + contentMD5 + ts) 前 8 位 hex % (fileSize - subSize + 1)。
func createDataOffset(contentMD5 string, uk, dataTime, fileSize, subSize int64) (int64, error) {
	h := md5.New()
	fmt.Fprintf(h, "%d%s%d", uk, contentMD5, dataTime)
	mixed := fmt.Sprintf("%x", h.Sum(nil))
	raw, err := strconv.ParseInt(mixed[0:8], 16, 64)
	if err != nil {
		return 0, err
	}
	if fileSize-subSize+1 <= 1 {
		return 0, nil
	}
	return raw % (fileSize - subSize + 1), nil
}

// backoffSleep — full jitter 退避（design §12：rand(0, min(30s, 1s×2^attempt))）。
// 返回 true=继续重试；false=ctx 已结束。
func backoffSleep(ctx context.Context, attempt int) bool {
	d := time.Second << (attempt - 1)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	sleep := time.Duration(rand.Int63n(int64(d) + 1))
	t := time.NewTimer(sleep)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
