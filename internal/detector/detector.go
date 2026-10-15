// Package detector 负责检测本地变更（design.md §4）。
// 硬性约束（P1）：Detector 只读本地元数据、只写 SQLite，零网络 I/O。
package detector

import (
	"context"
	"errors"
)

// Kind -- 变更类型（Backend 事件用）。
type Kind uint8

const (
	KindCreated Kind = iota
	KindModified
	KindDeleted
	KindRenamed
)

// Event 是后端向上层投递的「这个路径值得看一眼」的提示。
// 硬规矩2)：判定逻辑永远读索引，不读事件流 ---- 事件只是性能优化。
type Event struct {
	Kind    Kind
	Path    string // 相对 root，统一 '/'
	OldPath string // 仅 KindRenamed
}

// Stats 后端健康指标（进 bdsync status）。
type Stats struct {
	Backend   string `json:"backend"`
	Runs      int64  `json:"runs"`
	LastError string `json:"last_error,omitempty"`
}

// ErrNotImplemented -- Phase 1 占位。
var ErrNotImplemented = errors.New("detector: 该功能尚未实现")

// Backend 对应 design.md §4.9 的检测后端接口。
// 排除网络盘后只有 usn/poll 两个实现，但接口保留：
//  1. Windows 专属 API 必须被 build tag 隔离（交叉编译）
//  2. poll 是 usn 的正确性对照组
//  3. SelfTest 机制统一在接口层强制
type Backend interface {
	Name() string

	// Start 开始监控。初始快照不归它管，由调用方先跑一次全量扫描。
	Start(ctx context.Context, root string) error

	// Events 返回变更流。poll 后端由定时器驱动，返回 nil channel。
	Events() <-chan Event

	// SelfTest 写探测文件并确认收到事件 ---- 防「调用成功但从不通知」的唯一手段。
	SelfTest(ctx context.Context, root string) error

	// Stats 后端健康指标。
	Stats() Stats

	Close() error
}
