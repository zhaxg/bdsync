package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"syncer/internal/detector"
	"syncer/internal/index"
)

func msSince(t time.Time) int64 { return int64(time.Since(t) / time.Millisecond) }

func dbFileSize(path string) int64 {
	var total int64
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if st, err := os.Stat(f); err == nil {
			total += st.Size()
		}
	}
	return total
}

// commas — 千分位。
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := s[0] == '-'
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// humanBytes — 人类可读字节数。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func msHuman(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	}
	return fmt.Sprintf("%.2f s", float64(ms)/1000)
}

func printScanReport(res *detector.ScanResult, dbPath string, dbSize int64, jsonOut bool) {
	if jsonOut {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return
	}

	status := "✓ 扫描完成"
	if !res.WalkOK {
		status = "⚠ 扫描未完整（部分结果，未做删除判定）"
	}
	if res.WalkOnly {
		status += "  [walk-only：未写库]"
	}
	fmt.Printf("%s  run=%d\n", status, res.RunID)
	fmt.Printf("  根目录        %s\n", res.Root)
	fmt.Printf("  枚举          %s 目录 + %s 文件", commas(res.Dirs), commas(res.Files))
	if res.Symlinks > 0 {
		fmt.Printf("（跳过符号链接 %d）", res.Symlinks)
	}
	if res.Skipped > 0 {
		fmt.Printf("（跳过不可读 %d）", res.Skipped)
	}
	fmt.Println()
	fmt.Printf("  数据量        %s\n", humanBytes(res.Bytes))
	fmt.Printf("  变化          新增 %s 文件 + %s 目录 / 修改 %s / 复活 %s / 删除 %s / 未变 %s\n",
		commas(res.AddedFiles), commas(res.AddedDirs), commas(res.Modified),
		commas(res.Resurrected), commas(res.Removed), commas(res.Unchanged))
	if res.Conflicted > 0 {
		fmt.Printf("  文件名拦截    %s 个（网盘不接受的路径，已标 CONFLICT 不上传，见 status）\n", commas(res.Conflicted))
	}
	fmt.Printf("  阶段耗时      快照 %s | 遍历+比对 %s | 写库 %s (%d 批) | checkpoint %s\n",
		msHuman(res.LoadMs),
		msHuman(res.WallMs-res.LoadMs-res.CommitMs-res.CheckpointMs),
		msHuman(res.CommitMs), res.CommitCount,
		msHuman(res.CheckpointMs))
	wallSec := float64(res.WallMs) / 1000
	if wallSec <= 0 {
		wallSec = 0.001
	}
	fmt.Printf("  总耗时        %s   吞吐 %s 项/s\n",
		msHuman(res.WallMs),
		commas(int64(float64(res.Files+res.Dirs)/wallSec)))
	if res.CpuMs > 0 {
		fmt.Printf("  进程 CPU      %s（占墙钟 %.1f%%，单核口径）\n",
			msHuman(res.CpuMs), float64(res.CpuMs)/float64(res.WallMs)*100)
	}
	if res.RssBytes > 0 {
		fmt.Printf("  内存          RSS %s（峰值 %s）\n", humanBytes(res.RssBytes), humanBytes(res.PeakRssBytes))
		fmt.Printf("                Go Sys %s（堆 sys %s，已归还 %s，存活堆 %s）| 非 Go（SQLite/系统 DLL）≈ %s\n",
			humanBytes(res.GoSysBytes), humanBytes(res.HeapSysBytes), humanBytes(res.HeapRelBytes),
			humanBytes(res.HeapBytes), humanBytes(res.RssBytes-res.GoSysBytes))
	}
	if dbPath != "" {
		fmt.Printf("  数据库        %s (%s)\n", humanBytes(dbSize), dbPath)
	}
	for _, e := range res.ErrSamples {
		fmt.Printf("  跳过原因      %s\n", e)
	}
}

func printStatus(dbPath string, dbSize int64, root, remoteRoot, mode string, schemaVer int, pairID int64,
	counts *index.Counts, pend *index.Pending, last *detector.ScanResult, haveStats bool) {

	fmt.Printf("syncer status — %s (%s)\n", dbPath, humanBytes(dbSize))
	fmt.Printf("  根目录        %s\n", root)
	if remoteRoot != "" {
		fmt.Printf("  远端目录      %s\n", remoteRoot)
	} else {
		fmt.Printf("  远端目录      （未绑定，send --remote /目录 绑定）\n")
	}
	fmt.Printf("  配对          pair_id=%d  检测模式=%s  schema=%d\n", pairID, mode, schemaVer)

	if haveStats {
		fmt.Printf("  上次扫描      %s  (run %d, 耗时 %s, CPU %s%s)\n",
			last.At.Format("2006-01-02 15:04:05"), last.RunID,
			msHuman(last.WallMs), msHuman(last.CpuMs),
			map[bool]string{true: ", 未完整", false: ""}[!last.WalkOK])
		fmt.Printf("                文件 %s / 目录 %s | 新增 %s / 修改 %s / 删除 %s / 未变 %s\n",
			commas(last.Files), commas(last.Dirs), commas(last.AddedFiles),
			commas(last.Modified), commas(last.Removed), commas(last.Unchanged))
	} else {
		fmt.Printf("  上次扫描      （无记录）\n")
	}

	fmt.Printf("  索引规模      本地现存 %s 项（文件 %s + 目录 %s），%s\n",
		commas(counts.Files+counts.Dirs), commas(counts.Files), commas(counts.Dirs), humanBytes(counts.Bytes))
	fmt.Printf("  待传          %s 个文件 / %s", commas(pend.Count), humanBytes(pend.Bytes))
	if pend.Count > 0 {
		fmt.Printf("，最老等待 %s", time.Since(time.Unix(0, pend.OldestNs)).Round(time.Second))
	}
	fmt.Println()
	if pend.Deferred > 0 {
		fmt.Printf("  其中推迟      %s 个（settle 未到 / 写入中 / 临时失败，drain 会自动重试）\n", commas(pend.Deferred))
	}
	if appCfg.Mirror.Enabled {
		fmt.Printf("  镜像          归档目录 %s / 保留 %d 天", appCfg.Mirror.HistoryDir, appCfg.Mirror.HistoryDays)
		if pend.History > 0 {
			fmt.Printf("，待归档 %s 项", commas(pend.History))
		}
		fmt.Println()
	}

	// 按状态值排序输出
	var parts []string
	for st := 0; st <= 9; st++ {
		if n, ok := counts.ByState[st]; ok {
			parts = append(parts, fmt.Sprintf("%s %s", index.StateName(st), commas(n)))
		}
	}
	fmt.Printf("  状态分布      %s\n", strings.Join(parts, " | "))
	fmt.Printf("  注：目录条目保持 LOCAL_NEW（远端目录由 send 按需创建）；\n" +
		"      IDENTICAL = 内容已上传且与索引一致。\n")
}
