package core

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// MatchQueue 内存匹配队列 (替代Redis)
type MatchQueue struct {
	ch       chan MatchedTarget
	logger   *zap.Logger
	stats    *Stats
	mu       sync.RWMutex
	pending  int // 当前待处理数量
	total    int // 总入队数量
	consumed int // 已消费数量
}

// NewMatchQueue 创建内存队列
func NewMatchQueue(bufferSize int, logger *zap.Logger, stats *Stats) *MatchQueue {
	if bufferSize <= 0 {
		bufferSize = 1000 // 默认缓冲1000条
	}
	return &MatchQueue{
		ch:     make(chan MatchedTarget, bufferSize),
		logger: logger,
		stats:  stats,
	}
}

// Push 推送匹配结果到队列
func (q *MatchQueue) Push(matched MatchedTarget) bool {
	select {
	case q.ch <- matched:
		q.mu.Lock()
		q.pending++
		q.total++
		q.mu.Unlock()
		return true
	default:
		// 队列满了，丢弃
		q.logger.Warn("队列已满，丢弃匹配",
			zap.String("target", matched.Target.Address[:10]+"..."))
		return false
	}
}

// Pop 从队列中取出匹配结果 (阻塞)
func (q *MatchQueue) Pop(ctx context.Context) (MatchedTarget, bool) {
	select {
	case <-ctx.Done():
		return MatchedTarget{}, false
	case matched := <-q.ch:
		q.mu.Lock()
		q.pending--
		q.consumed++
		q.mu.Unlock()
		return matched, true
	}
}

// PopBatch 批量取出 (带超时)
func (q *MatchQueue) PopBatch(ctx context.Context, maxCount int, timeout time.Duration) []MatchedTarget {
	results := make([]MatchedTarget, 0, maxCount)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for len(results) < maxCount {
		select {
		case <-ctx.Done():
			return results
		case <-timer.C:
			return results
		case matched := <-q.ch:
			q.mu.Lock()
			q.pending--
			q.consumed++
			q.mu.Unlock()
			results = append(results, matched)
		}
	}
	return results
}

// TryPop 非阻塞取出
func (q *MatchQueue) TryPop() (MatchedTarget, bool) {
	select {
	case matched := <-q.ch:
		q.mu.Lock()
		q.pending--
		q.consumed++
		q.mu.Unlock()
		return matched, true
	default:
		return MatchedTarget{}, false
	}
}

// Len 当前队列长度
func (q *MatchQueue) Len() int {
	return len(q.ch)
}

// Stats 获取统计
func (q *MatchQueue) GetStats() (pending, total, consumed int) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.pending, q.total, q.consumed
}

// Close 关闭队列
func (q *MatchQueue) Close() {
	close(q.ch)
}

