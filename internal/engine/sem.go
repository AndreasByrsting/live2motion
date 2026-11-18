package engine

import (
	"context"
	"sync"
	"time"
)

// dynSem 是可动态调整上限的并发信号量。
// worker 池固定大小常驻，真正限制并发的是这里的 limit，
// 这样 Web 改「并发数」可以立即生效，不需要重启进程。
type dynSem struct {
	mu     sync.Mutex
	limit  int
	active int
	notify chan struct{}
}

func newDynSem(limit int) *dynSem {
	if limit < 1 {
		limit = 1
	}
	return &dynSem{limit: limit, notify: make(chan struct{}, 1)}
}

// Acquire 获取一个执行名额，ctx 取消时返回 false。
func (s *dynSem) Acquire(ctx context.Context) bool {
	for {
		s.mu.Lock()
		if s.active < s.limit {
			s.active++
			s.mu.Unlock()
			return true
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-s.notify:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Release 释放名额。
func (s *dynSem) Release() {
	s.mu.Lock()
	if s.active > 0 {
		s.active--
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// SetLimit 调整并发上限。
func (s *dynSem) SetLimit(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.limit = n
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Active 当前正在执行的任务数。
func (s *dynSem) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}
