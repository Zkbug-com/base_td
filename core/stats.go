package core

import (
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// WebLogFunc Web日志回调函数类型
type WebLogFunc func(level, category, message, details string)

// Stats 全局统计信息
type Stats struct {
	// 监控统计 (总计)
	TransfersDetected atomic.Int64 // 检测到的转账总数
	TransfersFiltered atomic.Int64 // 过滤后的转账数
	MatchesFound      atomic.Int64 // 匹配成功数
	MatchesPending    atomic.Int64 // 待处理匹配数

	// 执行统计 (总计)
	BatchesExecuted  atomic.Int64 // 执行的批次数
	TransfersSent    atomic.Int64 // 发送的转账数
	TransfersSuccess atomic.Int64 // 成功的转账数
	TransfersFailed  atomic.Int64 // 失败的转账数

	// 费用统计
	GasUsed       atomic.Int64 // 使用的Gas (wei)
	ContractCalls atomic.Int64 // 合约调用次数

	// 今日统计 (每日00:00重置)
	TodayDetected atomic.Int64 // 今日检测转账
	TodayFiltered atomic.Int64 // 今日过滤后
	TodayMatches  atomic.Int64 // 今日匹配成功
	TodaySent     atomic.Int64 // 今日发送
	TodaySuccess  atomic.Int64 // 今日成功
	TodayFailed   atomic.Int64 // 今日失败
	TodayBatches  atomic.Int64 // 今日批次
	currentDay    int          // 当前是哪一天 (day of year)

	// 时间
	StartTime        time.Time
	LastActivityTime time.Time
	mu               sync.Mutex

	logger   *zap.Logger
	stopChan chan struct{}

	// Web日志回调
	webLogFunc WebLogFunc
}

// NewStats 创建统计实例
func NewStats(logger *zap.Logger) *Stats {
	now := time.Now()
	return &Stats{
		StartTime:        now,
		LastActivityTime: now,
		currentDay:       now.YearDay(),
		logger:           logger,
		stopChan:         make(chan struct{}),
	}
}

// checkDayReset 检查是否需要重置今日统计
func (s *Stats) checkDayReset() {
	today := time.Now().YearDay()
	s.mu.Lock()
	if s.currentDay != today {
		s.currentDay = today
		s.TodayDetected.Store(0)
		s.TodayFiltered.Store(0)
		s.TodayMatches.Store(0)
		s.TodaySent.Store(0)
		s.TodaySuccess.Store(0)
		s.TodayFailed.Store(0)
		s.TodayBatches.Store(0)
	}
	s.mu.Unlock()
}

// IncrDetected 增加检测计数 (同时更新总计和今日)
func (s *Stats) IncrDetected() {
	s.checkDayReset()
	s.TransfersDetected.Add(1)
	s.TodayDetected.Add(1)
}

// IncrFiltered 增加过滤后计数
func (s *Stats) IncrFiltered() {
	s.checkDayReset()
	s.TransfersFiltered.Add(1)
	s.TodayFiltered.Add(1)
}

// IncrMatch 增加匹配成功计数
func (s *Stats) IncrMatch() {
	s.checkDayReset()
	s.MatchesFound.Add(1)
	s.TodayMatches.Add(1)
}

// IncrSent 增加发送计数
func (s *Stats) IncrSent() {
	s.checkDayReset()
	s.TransfersSent.Add(1)
	s.TodaySent.Add(1)
}

// IncrSuccess 增加成功计数
func (s *Stats) IncrSuccess() {
	s.checkDayReset()
	s.TransfersSuccess.Add(1)
	s.TodaySuccess.Add(1)
}

// IncrFailed 增加失败计数
func (s *Stats) IncrFailed() {
	s.checkDayReset()
	s.TransfersFailed.Add(1)
	s.TodayFailed.Add(1)
}

// IncrBatch 增加批次计数
func (s *Stats) IncrBatch() {
	s.checkDayReset()
	s.BatchesExecuted.Add(1)
	s.TodayBatches.Add(1)
}

// UpdateActivity 更新最后活动时间
func (s *Stats) UpdateActivity() {
	s.mu.Lock()
	s.LastActivityTime = time.Now()
	s.mu.Unlock()
}

// SetWebLogFunc 设置Web日志回调
func (s *Stats) SetWebLogFunc(f WebLogFunc) {
	s.mu.Lock()
	s.webLogFunc = f
	s.mu.Unlock()
}

// AddWebLog 添加Web日志
func (s *Stats) AddWebLog(level, category, message, details string) {
	s.mu.Lock()
	f := s.webLogFunc
	s.mu.Unlock()
	if f != nil {
		f(level, category, message, details)
	}
}

// StartReporter 启动定期报告 (静默模式，仅更新内部状态)
func (s *Stats) StartReporter(interval time.Duration) {
	// 静默模式: 不输出日志到Docker，仅Web界面展示
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// 静默模式: 不打印日志
				// 仅更新活动时间供Web界面获取
				s.UpdateActivity()
			case <-s.stopChan:
				return
			}
		}
	}()
}

// Stop 停止报告
func (s *Stats) Stop() {
	close(s.stopChan)
}

// PrintStats 打印统计信息
func (s *Stats) PrintStats() {
	uptime := time.Since(s.StartTime).Round(time.Second)

	s.mu.Lock()
	lastActivity := time.Since(s.LastActivityTime).Round(time.Second)
	s.mu.Unlock()

	// 计算费用 (Base L2 gas极低)
	gasUsedWei := big.NewInt(s.GasUsed.Load())
	gasUsedETH := new(big.Float).Quo(
		new(big.Float).SetInt(gasUsedWei),
		new(big.Float).SetInt(big.NewInt(1e18)),
	)
	gasCostStr, _ := gasUsedETH.Float64()

	successRate := float64(0)
	sent := s.TransfersSent.Load()
	if sent > 0 {
		successRate = float64(s.TransfersSuccess.Load()) / float64(sent) * 100
	}

	s.logger.Info("📊 ═══════════ 系统状态 ═══════════",
		zap.String("运行时间", uptime.String()),
		zap.String("最后活动", fmt.Sprintf("%s前", lastActivity.String())),
	)

	s.logger.Info("📡 监控统计",
		zap.Int64("检测转账", s.TransfersDetected.Load()),
		zap.Int64("过滤后", s.TransfersFiltered.Load()),
		zap.Int64("匹配成功", s.MatchesFound.Load()),
		zap.Int64("待处理", s.MatchesPending.Load()),
	)

	s.logger.Info("🚀 执行统计",
		zap.Int64("批次数", s.BatchesExecuted.Load()),
		zap.Int64("发送", s.TransfersSent.Load()),
		zap.Int64("成功", s.TransfersSuccess.Load()),
		zap.Int64("失败", s.TransfersFailed.Load()),
		zap.String("成功率", fmt.Sprintf("%.1f%%", successRate)),
	)

	s.logger.Info("💰 费用统计",
		zap.Int64("合约调用", s.ContractCalls.Load()),
		zap.String("预估Gas费", fmt.Sprintf("%.8f ETH", gasCostStr)),
	)

	s.logger.Info("═══════════════════════════════════")
}

// GetSummary 获取摘要字符串
func (s *Stats) GetSummary() string {
	return fmt.Sprintf(
		"检测:%d 匹配:%d 发送:%d 成功:%d 失败:%d",
		s.TransfersDetected.Load(),
		s.MatchesFound.Load(),
		s.TransfersSent.Load(),
		s.TransfersSuccess.Load(),
		s.TransfersFailed.Load(),
	)
}
