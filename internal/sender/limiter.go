package sender

import (
	"context"
	"sync"
	"time"
)

// limiter — AIMD 自适应并发（design §12）：起步给足，连续成功缓慢上探（防抖），
// 遇限流立即减半。退避抖动由调用方负责，这里只管并发额度。
type limiter struct {
	mu        sync.Mutex
	cur       int
	max       int
	successes int
	lastInc   time.Time
	active    int
	logf      func(string, ...any)
}

func newLimiter(start, max int, logf func(string, ...any)) *limiter {
	if max < 1 {
		max = 1
	}
	if start < 1 {
		start = 1
	}
	if start > max {
		start = max
	}
	return &limiter{cur: start, max: max, logf: logf, lastInc: time.Now()}
}

// wait — 占一个并发名额；ctx 结束返回 false。
func (l *limiter) wait(ctx context.Context) bool {
	for {
		l.mu.Lock()
		if l.active < l.cur {
			l.active++
			l.mu.Unlock()
			return true
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.mu.Unlock()
}

// onSuccess — 每 4 次连续成功、且距上次提升 ≥10s → +1（design：每 30s 最多 +1 的防抖语义）。
func (l *limiter) onSuccess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.successes++
	if l.successes >= 4 && l.cur < l.max && time.Since(l.lastInc) >= 10*time.Second {
		l.cur++
		l.successes = 0
		l.lastInc = time.Now()
		l.logf("[≈] 并发提升 → %d", l.cur)
	}
}

// onRateLimit — 遇限流（errno 6）→ 减半，立即生效。
func (l *limiter) onRateLimit() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur > 1 {
		next := l.cur / 2
		if next < 1 {
			next = 1
		}
		l.cur = next
		l.successes = 0
		l.logf("[≈] 检测到限流，并发降至 %d", l.cur)
	}
}

// setMax — 在线调整并发上限（看板配置）。
func (l *limiter) setMax(n int) {
	if n < 1 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.max = n
	if l.cur > n {
		l.cur = n
	}
}

func (l *limiter) limit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur
}
