package security

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	KeyDailySpent  = "cost:daily:"
	KeyHourlySpent = "cost:hourly:"
)

// CostControlConfig 成本控制配置
type CostControlConfig struct {
	DailyBudgetUSD    float64
	HourlyLimitUSD    float64
	MaxGasPriceGwei   float64
	AlertThresholdPct float64
	PauseOnExceed     bool
}

// CostController 成本控制器
type CostController struct {
	config   CostControlConfig
	redis    *redis.Client
	logger   *zap.Logger
	paused   bool
	mu       sync.RWMutex
	ethPrice float64
}

// NewCostController 创建成本控制器
func NewCostController(config CostControlConfig, redisClient *redis.Client, logger *zap.Logger) *CostController {
	return &CostController{
		config:   config,
		redis:    redisClient,
		logger:   logger,
		ethPrice: 3500.0, // 默认ETH价格，需要定期更新
	}
}

// SetETHPrice 更新ETH价格
func (c *CostController) SetETHPrice(price float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ethPrice = price
}

// IsPaused 检查是否暂停
func (c *CostController) IsPaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused
}

// RecordCost 记录成本 (Base链使用ETH)
func (c *CostController) RecordCost(ctx context.Context, gasETH float64) error {
	c.mu.RLock()
	ethPrice := c.ethPrice
	c.mu.RUnlock()

	costUSD := gasETH * ethPrice

	now := time.Now()
	dailyKey := KeyDailySpent + now.Format("2006-01-02")
	hourlyKey := KeyHourlySpent + now.Format("2006-01-02-15")

	pipe := c.redis.Pipeline()
	pipe.IncrByFloat(ctx, dailyKey, costUSD)
	pipe.IncrByFloat(ctx, hourlyKey, costUSD)
	pipe.Expire(ctx, dailyKey, 48*time.Hour)
	pipe.Expire(ctx, hourlyKey, 2*time.Hour)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return err
	}

	// 检查是否超限
	return c.checkLimits(ctx, dailyKey, hourlyKey)
}

// checkLimits 检查限制
func (c *CostController) checkLimits(ctx context.Context, dailyKey, hourlyKey string) error {
	dailySpent, err := c.redis.Get(ctx, dailyKey).Float64()
	if err != nil && err != redis.Nil {
		return err
	}

	hourlySpent, err := c.redis.Get(ctx, hourlyKey).Float64()
	if err != nil && err != redis.Nil {
		return err
	}

	// 检查每日预算
	if dailySpent >= c.config.DailyBudgetUSD {
		c.logger.Warn("Daily budget exceeded",
			zap.Float64("spent", dailySpent),
			zap.Float64("budget", c.config.DailyBudgetUSD))
		if c.config.PauseOnExceed {
			c.pause()
		}
		return fmt.Errorf("daily budget exceeded: %.2f >= %.2f", dailySpent, c.config.DailyBudgetUSD)
	}

	// 检查每小时限制
	if hourlySpent >= c.config.HourlyLimitUSD {
		c.logger.Warn("Hourly limit exceeded",
			zap.Float64("spent", hourlySpent),
			zap.Float64("limit", c.config.HourlyLimitUSD))
		if c.config.PauseOnExceed {
			c.pause()
		}
		return fmt.Errorf("hourly limit exceeded: %.2f >= %.2f", hourlySpent, c.config.HourlyLimitUSD)
	}

	// 检查警告阈值
	dailyThreshold := c.config.DailyBudgetUSD * c.config.AlertThresholdPct / 100
	if dailySpent >= dailyThreshold {
		c.logger.Warn("Approaching daily budget",
			zap.Float64("spent", dailySpent),
			zap.Float64("threshold", dailyThreshold))
	}

	return nil
}

// pause 暂停执行
func (c *CostController) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = true
	c.logger.Error("Cost control: PAUSED due to budget exceeded")
}

// Resume 恢复执行
func (c *CostController) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = false
	c.logger.Info("Cost control: RESUMED")
}

// GetStats 获取统计信息
func (c *CostController) GetStats(ctx context.Context) (dailySpent, hourlySpent float64, err error) {
	now := time.Now()
	dailyKey := KeyDailySpent + now.Format("2006-01-02")
	hourlyKey := KeyHourlySpent + now.Format("2006-01-02-15")

	dailySpent, _ = c.redis.Get(ctx, dailyKey).Float64()
	hourlySpent, _ = c.redis.Get(ctx, hourlyKey).Float64()
	return dailySpent, hourlySpent, nil
}
