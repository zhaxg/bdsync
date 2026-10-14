// Package web — 本地看板（design §13）。
//
// 硬性约束：
//   - 只监听回环地址（config.Validate 强制校验）
//   - SSE 推送：无客户端时零推送；心跳 15s 仅在有连接时；日志事件即时投递
//   - 服务端不做周期性数据库轮询 —— 状态由 /api/status 按需查询，
//     客户端收到 SSE 事件后自行节流拉取（≤1Hz，符合 design §13）
//   - 控制接口（暂停/继续/停止）只对本进程生效，Origin 同源校验防跨站调用
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var pageHTML []byte

const (
	ringCap     = 500 // 日志环形缓冲（design §13 低开销）
	clientBuf   = 128 // 每连接缓冲（满了丢弃，重连可 backfill）
	heartbeat   = 15 * time.Second
	maxBackfill = 200 // 单次回填上限
)

// LogLine — 一条日志（带全局递增序号，客户端用 ?after= 断点续传）。
type LogLine struct {
	Seq int64     `json:"seq"`
	T   time.Time `json:"t"`
	Msg string    `json:"msg"`
}

// Hub — SSE 广播 + 日志环形缓冲。
type Hub struct {
	mu      sync.Mutex
	clients map[int]chan LogLine
	nextID  int
	seq     int64
	ring    []LogLine
	dropped int64 // 全局丢弃计数（客户端跟不上时）
}

func NewHub() *Hub {
	return &Hub{clients: map[int]chan LogLine{}}
}

// Append — 追加一条日志并即时广播（非阻塞：慢客户端丢消息，可 backfill 补）。
func (h *Hub) Append(msg string) {
	h.mu.Lock()
	line := LogLine{Seq: h.seq, T: time.Now(), Msg: msg}
	h.seq++
	h.ring = append(h.ring, line)
	if len(h.ring) > ringCap {
		h.ring = h.ring[len(h.ring)-ringCap:]
	}
	for _, ch := range h.clients {
		select {
		case ch <- line:
		default:
			h.dropped++
		}
	}
	h.mu.Unlock()
}

// Snapshot — after 序号之后的日志（断点续传/首屏回填）。
func (h *Hub) Snapshot(after int64, limit int) []LogLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []LogLine
	for _, l := range h.ring {
		if l.Seq > after {
			out = append(out, l)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Seq — 当前最新序号。
func (h *Hub) Seq() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

func (h *Hub) register() (int, chan LogLine) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextID
	h.nextID++
	ch := make(chan LogLine, clientBuf)
	h.clients[id] = ch
	return id, ch
}

func (h *Hub) unregister(id int, ch chan LogLine) {
	h.mu.Lock()
	delete(h.clients, id)
	h.mu.Unlock()
	close(ch)
}

func (h *Hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Control — daemon 控制面（web 只调用接口，不感知实现）。
type Control interface {
	Pause() bool  // 返回是否确实进入了暂停
	Resume() bool // 返回是否确实恢复了
	Stop()        // 优雅停止（进程退出）
	Paused() bool
}

// StatusFunc — 按需构建状态快照（web 不轮询，只在请求时调用）。
type StatusFunc func() any

// ConfigIO — 配置读写。
// Get 返回：当前 YAML 全文、基本表单值、结构性字段提示（改动需重启）。
// Save 载荷二选一：{"yaml": "..."} 全文覆盖，或 {"settings": {...}} 基本字段。
type ConfigIO struct {
	Get  func() (yamlText string, basic map[string]any, restartHints []string)
	Save func(payload map[string]any) (applied, needsRestart []string, err error)
}

// SetupIO — 首次配置向导（零配置双击场景：看板先起，配置后补）。
// Apply 返回提示语；返回错误时页面停留在向导并显示原因。
type SetupIO struct {
	State func() any
	Apply func(payload map[string]any) (string, error)
	Probe func(root string) (files, dirs int, err error) // 检测本地目录（帮助填对路径）
}

// Server — 看板 HTTP 服务。
type Server struct {
	hub    *Hub
	status StatusFunc
	ctrl   Control
	cfgIO  ConfigIO
	setup  *SetupIO
	srv    *http.Server
	addr   string
}

func New(addr string, hub *Hub, status StatusFunc, ctrl Control, cfgIO ConfigIO) *Server {
	return &Server{hub: hub, status: status, ctrl: ctrl, cfgIO: cfgIO, addr: addr}
}

// WithSetup — 挂载首次配置向导（daemon 不需要）。
func (s *Server) WithSetup(io SetupIO) *Server {
	s.setup = &io
	return s
}

// Start — 非阻塞启动。
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.withLocal(s.handlePage))
	mux.HandleFunc("/api/status", s.withLocal(s.handleStatus))
	mux.HandleFunc("/api/logs", s.withLocal(s.handleLogs))
	mux.HandleFunc("/api/events", s.withLocal(s.handleSSE))
	mux.HandleFunc("/api/control", s.withLocal(s.handleControl))
	mux.HandleFunc("/api/config", s.withLocal(s.handleConfig))
	mux.HandleFunc("/api/setup", s.withLocal(s.handleSetup))
	mux.HandleFunc("/api/setup/apply", s.withLocal(s.handleSetupApply))
	mux.HandleFunc("/api/setup/probe", s.withLocal(s.handleSetupProbe))

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("看板监听 %s 失败: %w", s.addr, err)
	}
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Stop — 关闭服务。
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// URL — 展示用地址。
func (s *Server) URL() string { return "http://" + s.addr + "/" }

// withLocal — 回环来源校验（纵深防御）+ 同源 Origin 校验（防跨站 POST）。
func (s *Server) withLocal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !isLoopback(host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if !strings.HasPrefix(origin, "http://"+r.Host) &&
				!strings.HasPrefix(origin, "https://"+r.Host) {
				http.Error(w, "cross-origin forbidden", http.StatusForbidden)
				return
			}
		}
		// 兜底：看板是"配置失败也必须活着"的那一环，任何 handler 的 panic
		// 都不能把进程带走（否则浏览器只会看到 Failed to fetch，人完全懵）。
		defer func() {
			if rec := recover(); rec != nil {
				s.hub.Append(fmt.Sprintf("[!] 处理 %s 时内部错误：%v", r.URL.Path, rec))
				writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
					"ok": false, "error": fmt.Sprintf("服务内部错误：%v（详情见日志页）", rec),
				})
			}
		}()
		next(w, r)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" || host == "::1" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(pageHTML)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.status())
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	lines := s.hub.Snapshot(after, maxBackfill)
	if lines == nil {
		lines = []LogLine{}
	}
	writeJSON(w, map[string]any{"lines": lines, "next": s.hub.Seq()})
}

// handleSSE — 事件流（design §13：仅连接存活时推送；15s 心跳；慢客户端丢弃）。
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no sse", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	id, ch := s.hub.register()
	defer s.hub.unregister(id, ch)

	// 首帧：日志断点（客户端带 ?after=）
	if after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); err == nil {
		for _, l := range s.hub.Snapshot(after, maxBackfill) {
			writeSSE(w, "log", l)
		}
	}
	writeSSE(w, "hello", map[string]any{"clients": s.hub.clientCount()})
	flusher.Flush()

	hb := time.NewTicker(heartbeat)
	defer hb.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case l, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, "log", l)
			flusher.Flush()
		case <-hb.C:
			// 心跳：注释帧，不产生事件
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "pause":
		s.ctrl.Pause()
		s.hub.Append("[看板] 用户暂停了检测与排空")
	case "resume":
		s.ctrl.Resume()
		s.hub.Append("[看板] 用户恢复运行")
	case "stop":
		s.hub.Append("[看板] 用户请求停止（进程将优雅退出）")
		go s.ctrl.Stop()
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "paused": s.ctrl.Paused()})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		text, basic, hints := s.cfgIO.Get()
		if basic == nil {
			basic = map[string]any{}
		}
		if hints == nil {
			hints = []string{}
		}
		writeJSON(w, map[string]any{"yaml": text, "basic": basic, "restart_fields": hints})
	case http.MethodPost:
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		applied, restart, err := s.cfgIO.Save(payload)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": err.Error(),
			})
			return
		}
		s.hub.Append(fmt.Sprintf("[看板] 配置已保存（立即生效 %d 项，需重启 %d 项）", len(applied), len(restart)))
		writeJSON(w, map[string]any{"ok": true, "applied": applied, "needs_restart": restart})
	default:
		http.Error(w, "GET/POST only", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

// handleSetup — GET 返回引导状态（页面据此显示向导或控制台）。
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || s.setup == nil || s.setup.State == nil {
		writeJSON(w, map[string]any{"available": false, "running": true})
		return
	}
	writeJSON(w, s.setup.State())
}

// handleSetupProbe — 检测本地目录是否存在、可读（向导里的"检测"按钮）。
func (s *Server) handleSetupProbe(w http.ResponseWriter, r *http.Request) {
	if s.setup == nil || s.setup.Probe == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "当前模式不支持检测"})
		return
	}
	files, dirs, err := s.setup.Probe(r.URL.Query().Get("root"))
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "files": files, "dirs": dirs})
}

// handleSetupApply — POST 应用首次配置；失败即返回原因（页面留在向导）。
func (s *Server) handleSetupApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || s.setup == nil || s.setup.Apply == nil {
		http.Error(w, "setup not available", http.StatusNotFound)
		return
	}
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	msg, err := s.setup.Apply(payload)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.hub.Append("[看板] " + msg)
	writeJSON(w, map[string]any{"ok": true, "message": msg})
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

// SortedHints — 提示排序（展示用）。
func SortedHints(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
