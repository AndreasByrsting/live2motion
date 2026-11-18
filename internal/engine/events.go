package engine

import (
	"sync"
	"time"
)

// 事件类型
const (
	EventLog      = "log"      // 一条运行日志
	EventJob      = "job"      // 任务进度/状态变化
	EventStats    = "stats"    // 统计数字变化
	EventScan     = "scan"     // 扫描开始/结束
	EventSnapshot = "snapshot" // 建立连接时的全量快照
)

// Event 广播给所有 SSE 订阅者的消息。
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// EventBus 极简发布订阅，用于把后台进度推到 Web 页面。
// 订阅者缓冲满时直接丢弃事件，绝不允许慢客户端阻塞转换流程。
type EventBus struct {
	mu     sync.RWMutex
	subs   map[int64]chan Event
	nextID int64
}

// NewEventBus 构造事件总线。
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[int64]chan Event)}
}

// Subscribe 订阅事件，返回订阅 id 与只读通道。
func (b *EventBus) Subscribe() (int64, <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := b.nextID
	ch := make(chan Event, 128)
	b.subs[id] = ch
	return id, ch
}

// Unsubscribe 取消订阅并关闭通道。
func (b *EventBus) Unsubscribe(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Publish 广播事件（非阻塞）。
func (b *EventBus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// 慢客户端：丢弃本条，保持主流程不被阻塞
		}
	}
}

// SubscriberCount 当前订阅数。
func (b *EventBus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// completionRecord 单个任务的完成记录，用于估算速度与剩余时间。
type completionRecord struct {
	finishedAt time.Time
	bytes      int64
}

// throughput 基于最近完成记录计算速度（字节/秒）与平均单任务耗时。
type throughput struct {
	mu      sync.Mutex
	records []completionRecord
}

const throughputWindow = 20

func (t *throughput) add(bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.records = append(t.records, completionRecord{finishedAt: time.Now(), bytes: bytes})
	if len(t.records) > throughputWindow {
		t.records = t.records[len(t.records)-throughputWindow:]
	}
}

// snapshot 返回平均单任务耗时（秒）与字节吞吐（字节/秒）。
func (t *throughput) snapshot() (avgSeconds float64, bytesPerSec float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.records)
	if n < 2 {
		return 0, 0
	}
	span := t.records[n-1].finishedAt.Sub(t.records[0].finishedAt).Seconds()
	var totalBytes int64
	for _, r := range t.records {
		totalBytes += r.bytes
	}
	if span <= 0 {
		return 0, 0
	}
	return span / float64(n-1), float64(totalBytes) / span
}
