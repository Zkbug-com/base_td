package web

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"exploit/core"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
	"go.uber.org/zap"
)

// LogEntry 日志条目
type LogEntry struct {
	Time     string `json:"time"`
	Level    string `json:"level"`
	Category string `json:"category"`
	Message  string `json:"message"`
	Details  string `json:"details"`
}

// ServerConfig Web服务器配置
type ServerConfig struct {
	SecretPath string // 秘密访问路径
	Password   string // 访问密码
}

// Server Web服务器
type Server struct {
	stats      *core.Stats
	logger     *zap.Logger
	logs       []LogEntry
	logsMu     sync.RWMutex
	maxLogs    int
	config     ServerConfig
	authedSess sync.Map // 已认证的session
}

// NewServer 创建Web服务器
func NewServer(stats *core.Stats, logger *zap.Logger, config ServerConfig) *Server {
	return &Server{
		stats:   stats,
		logger:  logger,
		logs:    make([]LogEntry, 0, 1000),
		maxLogs: 1000,
		config:  config,
	}
}

// AddLog 添加日志
func (s *Server) AddLog(level, category, message, details string) {
	s.logsMu.Lock()
	defer s.logsMu.Unlock()

	entry := LogEntry{
		Time:     time.Now().Format("15:04:05"),
		Level:    level,
		Category: category,
		Message:  message,
		Details:  details,
	}

	s.logs = append(s.logs, entry)
	if len(s.logs) > s.maxLogs {
		s.logs = s.logs[len(s.logs)-s.maxLogs:]
	}
}

// Start 启动服务器
func (s *Server) Start(port int) error {
	mux := http.NewServeMux()

	// 秘密路径下的所有路由
	secretBase := "/" + s.config.SecretPath
	mux.HandleFunc(secretBase, s.authMiddleware(s.handleLogin))
	mux.HandleFunc(secretBase+"/", s.authMiddleware(s.handleIndex))
	mux.HandleFunc(secretBase+"/api/stats", s.authMiddleware(s.handleStats))
	mux.HandleFunc(secretBase+"/api/logs", s.authMiddleware(s.handleLogs))
	mux.HandleFunc(secretBase+"/api/system", s.authMiddleware(s.handleSystemStats))

	// 其他路径返回404
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	s.logger.Info("🌐 Web监控面板启动",
		zap.Int("port", port),
		zap.String("secret_path", "/"+s.config.SecretPath[:8]+"..."))
	return http.ListenAndServe(fmt.Sprintf(":%d", port), mux)
}

// authMiddleware 认证中间件
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 检查cookie中的session
		cookie, err := r.Cookie("auth_token")
		if err == nil {
			if _, ok := s.authedSess.Load(cookie.Value); ok {
				next(w, r)
				return
			}
		}

		// 检查密码参数
		password := r.URL.Query().Get("pwd")
		if password == "" {
			password = r.FormValue("password")
		}

		if subtle.ConstantTimeCompare([]byte(password), []byte(s.config.Password)) == 1 {
			// 生成session token
			token := fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().Unix())
			s.authedSess.Store(token, true)

			// 设置cookie
			http.SetCookie(w, &http.Cookie{
				Name:     "auth_token",
				Value:    token,
				Path:     "/" + s.config.SecretPath,
				MaxAge:   86400 * 7, // 7天
				HttpOnly: true,
				Secure:   false, // 生产环境应设为true
				SameSite: http.SameSiteStrictMode,
			})

			next(w, r)
			return
		}

		// 未认证，显示登录页面
		s.handleLoginPage(w, r)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(loginHTML))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// 已通过认证中间件，重定向到主页
	http.Redirect(w, r, "/"+s.config.SecretPath+"/", http.StatusFound)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	now := time.Now()
	stats := map[string]interface{}{
		// 时间信息
		"current_time": now.Format("2006-01-02 15:04:05"),
		"current_date": now.Format("2006年01月02日"),
		"uptime":       time.Since(s.stats.StartTime).Round(time.Second).String(),

		// 总计统计
		"transfers_detected": s.stats.TransfersDetected.Load(),
		"transfers_filtered": s.stats.TransfersFiltered.Load(),
		"matches_found":      s.stats.MatchesFound.Load(),
		"matches_pending":    s.stats.MatchesPending.Load(),
		"batches_executed":   s.stats.BatchesExecuted.Load(),
		"transfers_sent":     s.stats.TransfersSent.Load(),
		"transfers_success":  s.stats.TransfersSuccess.Load(),
		"transfers_failed":   s.stats.TransfersFailed.Load(),
		"contract_calls":     s.stats.ContractCalls.Load(),
		"gas_used":           s.stats.GasUsed.Load(),

		// 今日统计
		"today_detected": s.stats.TodayDetected.Load(),
		"today_filtered": s.stats.TodayFiltered.Load(),
		"today_matches":  s.stats.TodayMatches.Load(),
		"today_sent":     s.stats.TodaySent.Load(),
		"today_success":  s.stats.TodaySuccess.Load(),
		"today_failed":   s.stats.TodayFailed.Load(),
		"today_batches":  s.stats.TodayBatches.Load(),
	}

	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	category := r.URL.Query().Get("category")

	s.logsMu.RLock()
	defer s.logsMu.RUnlock()

	var filtered []LogEntry
	if category == "" || category == "all" {
		filtered = s.logs
	} else {
		for _, log := range s.logs {
			if log.Category == category {
				filtered = append(filtered, log)
			}
		}
	}

	// 返回最新100条
	start := 0
	if len(filtered) > 100 {
		start = len(filtered) - 100
	}

	json.NewEncoder(w).Encode(filtered[start:])
}

// handleSystemStats 返回服务器系统监控数据
func (s *Server) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	result := map[string]interface{}{}

	// CPU使用率
	cpuPercent, err := cpu.Percent(0, false)
	if err == nil && len(cpuPercent) > 0 {
		result["cpu_percent"] = cpuPercent[0]
	}
	result["cpu_cores"] = runtime.NumCPU()
	result["goroutines"] = runtime.NumGoroutine()

	// 内存信息
	if memInfo, err := mem.VirtualMemory(); err == nil {
		result["mem_total"] = memInfo.Total
		result["mem_used"] = memInfo.Used
		result["mem_available"] = memInfo.Available
		result["mem_percent"] = memInfo.UsedPercent
	}

	// 磁盘信息
	if diskInfo, err := disk.Usage("/"); err == nil {
		result["disk_total"] = diskInfo.Total
		result["disk_used"] = diskInfo.Used
		result["disk_free"] = diskInfo.Free
		result["disk_percent"] = diskInfo.UsedPercent
	}

	// 网络IO
	if netIO, err := psnet.IOCounters(false); err == nil && len(netIO) > 0 {
		result["net_bytes_sent"] = netIO[0].BytesSent
		result["net_bytes_recv"] = netIO[0].BytesRecv
		result["net_packets_sent"] = netIO[0].PacketsSent
		result["net_packets_recv"] = netIO[0].PacketsRecv
	}

	// 进程信息
	result["pid"] = os.Getpid()
	result["hostname"], _ = os.Hostname()

	// Go运行时内存
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	result["go_alloc"] = memStats.Alloc
	result["go_sys"] = memStats.Sys
	result["go_heap_alloc"] = memStats.HeapAlloc
	result["go_heap_sys"] = memStats.HeapSys
	result["go_gc_num"] = memStats.NumGC

	json.NewEncoder(w).Encode(result)
}
