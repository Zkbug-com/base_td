package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/proxy"
)

// ProxyConfig 代理配置
type ProxyConfig struct {
	// 格式: host:port:username:password
	// 例如: 05bc447b92b63b2e.xji.na.novada.pro:7777:user:pass
	StickyProxy   string        // 粘性代理 (2分钟固定IP)
	RotatingProxy string        // 轮换代理 (每次请求换IP)
	StickyTTL     time.Duration // 粘性代理有效期 (默认2分钟)
}

// ProxyManager 代理管理器
type ProxyManager struct {
	config         ProxyConfig
	stickyURL      *url.URL
	rotatingURL    *url.URL
	stickyClient   *http.Client
	rotatingClient *http.Client
	stickyExpireAt time.Time
	logger         *zap.Logger
	mu             sync.RWMutex
	requestCount   uint64
}

// NewProxyManager 创建代理管理器
func NewProxyManager(config ProxyConfig, logger *zap.Logger) (*ProxyManager, error) {
	pm := &ProxyManager{
		config: config,
		logger: logger,
	}

	if config.StickyTTL == 0 {
		pm.config.StickyTTL = 2 * time.Minute
	}

	// 解析粘性代理
	if config.StickyProxy != "" {
		proxyURL, err := parseProxyString(config.StickyProxy)
		if err != nil {
			return nil, fmt.Errorf("解析粘性代理失败: %w", err)
		}
		pm.stickyURL = proxyURL
		pm.stickyClient = pm.createHTTPClient(proxyURL)
		pm.stickyExpireAt = time.Now().Add(pm.config.StickyTTL)
		logger.Info("✅ 粘性代理配置成功", zap.String("host", proxyURL.Host), zap.Duration("TTL", pm.config.StickyTTL))
	}

	// 解析轮换代理
	if config.RotatingProxy != "" {
		proxyURL, err := parseProxyString(config.RotatingProxy)
		if err != nil {
			return nil, fmt.Errorf("解析轮换代理失败: %w", err)
		}
		pm.rotatingURL = proxyURL
		pm.rotatingClient = pm.createHTTPClient(proxyURL)
		logger.Info("✅ 轮换代理配置成功", zap.String("host", proxyURL.Host))
	}

	return pm, nil
}

// parseProxyString 解析代理字符串: host:port:username:password
func parseProxyString(s string) (*url.URL, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid proxy format, expected host:port[:user:pass]")
	}

	host := parts[0]
	port := parts[1]

	proxyURL := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
	}

	if len(parts) >= 4 {
		username := parts[2]
		password := strings.Join(parts[3:], ":") // 密码可能包含冒号
		proxyURL.User = url.UserPassword(username, password)
	}

	return proxyURL, nil
}

// createHTTPClient 创建带代理的HTTP客户端
func (pm *ProxyManager) createHTTPClient(proxyURL *url.URL) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

// GetHTTPClient 获取HTTP客户端 (轮换策略)
// 优先使用轮换代理，如果不可用则使用粘性代理
func (pm *ProxyManager) GetHTTPClient() *http.Client {
	count := atomic.AddUint64(&pm.requestCount, 1)

	// 优先使用轮换代理 (每次请求换IP)
	if pm.rotatingClient != nil {
		pm.logger.Debug("使用轮换代理", zap.Uint64("请求#", count))
		return pm.rotatingClient
	}

	// 其次使用粘性代理
	if pm.stickyClient != nil {
		pm.checkStickyExpire()
		pm.logger.Debug("使用粘性代理", zap.Uint64("请求#", count))
		return pm.stickyClient
	}

	// 无代理，返回默认客户端
	return http.DefaultClient
}

// GetStickyClient 获取粘性代理客户端 (2分钟固定IP)
func (pm *ProxyManager) GetStickyClient() *http.Client {
	if pm.stickyClient == nil {
		return pm.GetHTTPClient()
	}
	pm.checkStickyExpire()
	return pm.stickyClient
}

// checkStickyExpire 检查粘性代理是否过期
func (pm *ProxyManager) checkStickyExpire() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if time.Now().After(pm.stickyExpireAt) {
		pm.stickyExpireAt = time.Now().Add(pm.config.StickyTTL)
		pm.logger.Debug("粘性代理IP已刷新", zap.Time("下次刷新", pm.stickyExpireAt))
	}
}

// GetProxyURL 获取代理URL (用于WebSocket等场景)
func (pm *ProxyManager) GetProxyURL() *url.URL {
	if pm.rotatingURL != nil {
		return pm.rotatingURL
	}
	if pm.stickyURL != nil {
		return pm.stickyURL
	}
	return nil
}

// GetStickyProxyURL 获取粘性代理URL
func (pm *ProxyManager) GetStickyProxyURL() *url.URL {
	return pm.stickyURL
}

// GetRotatingProxyURL 获取轮换代理URL
func (pm *ProxyManager) GetRotatingProxyURL() *url.URL {
	return pm.rotatingURL
}

// GetSOCKS5Dialer 获取SOCKS5拨号器 (用于WebSocket)
func (pm *ProxyManager) GetSOCKS5Dialer() (proxy.Dialer, error) {
	proxyURL := pm.GetProxyURL()
	if proxyURL == nil {
		return proxy.Direct, nil
	}

	// 转换为SOCKS5地址 (如果代理支持)
	var auth *proxy.Auth
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		auth = &proxy.Auth{
			User:     proxyURL.User.Username(),
			Password: password,
		}
	}

	// 创建SOCKS5拨号器
	dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("创建SOCKS5拨号器失败: %w", err)
	}

	return dialer, nil
}

// GetHTTPDialer 获取HTTP代理拨号器 (用于RPC连接)
func (pm *ProxyManager) GetHTTPDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyURL := pm.GetProxyURL()
	if proxyURL == nil {
		return nil
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// 连接代理服务器
		proxyConn, err := net.DialTimeout("tcp", proxyURL.Host, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("连接代理失败: %w", err)
		}

		// 发送CONNECT请求
		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
		if proxyURL.User != nil {
			password, _ := proxyURL.User.Password()
			auth := proxyURL.User.Username() + ":" + password
			encoded := base64Encode(auth)
			connectReq += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", encoded)
		}
		connectReq += "\r\n"

		_, err = proxyConn.Write([]byte(connectReq))
		if err != nil {
			proxyConn.Close()
			return nil, fmt.Errorf("发送CONNECT请求失败: %w", err)
		}

		// 读取响应
		buf := make([]byte, 1024)
		n, err := proxyConn.Read(buf)
		if err != nil {
			proxyConn.Close()
			return nil, fmt.Errorf("读取代理响应失败: %w", err)
		}

		response := string(buf[:n])
		if !strings.Contains(response, "200") {
			proxyConn.Close()
			return nil, fmt.Errorf("代理连接失败: %s", response)
		}

		return proxyConn, nil
	}
}

// base64Encode Base64编码
func base64Encode(s string) string {
	const base64Table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	result := ""
	padding := 0
	data := []byte(s)

	for i := 0; i < len(data); i += 3 {
		n := int(data[i]) << 16
		if i+1 < len(data) {
			n |= int(data[i+1]) << 8
		} else {
			padding++
		}
		if i+2 < len(data) {
			n |= int(data[i+2])
		} else {
			padding++
		}

		result += string(base64Table[(n>>18)&0x3F])
		result += string(base64Table[(n>>12)&0x3F])
		if padding < 2 {
			result += string(base64Table[(n>>6)&0x3F])
		} else {
			result += "="
		}
		if padding < 1 {
			result += string(base64Table[n&0x3F])
		} else {
			result += "="
		}
	}
	return result
}

// HasProxy 检查是否配置了代理
func (pm *ProxyManager) HasProxy() bool {
	return pm.stickyURL != nil || pm.rotatingURL != nil
}

// Stats 代理统计
func (pm *ProxyManager) Stats() map[string]interface{} {
	return map[string]interface{}{
		"request_count":    atomic.LoadUint64(&pm.requestCount),
		"has_sticky":       pm.stickyURL != nil,
		"has_rotating":     pm.rotatingURL != nil,
		"sticky_expire_at": pm.stickyExpireAt,
	}
}
