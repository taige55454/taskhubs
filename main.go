package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
)

//go:embed web/*
var webFS embed.FS

type RotatingWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File
	size    int64
}

func NewRotatingWriter(path string, maxMB int64) (*RotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &RotatingWriter{
		path:    path,
		maxSize: maxMB * 1024 * 1024,
		file:    f,
		size:    info.Size(),
	}, nil
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.maxSize {
		_ = w.file.Close()
		_ = os.Rename(w.path, w.path+".1")
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err == nil {
			w.file = f
			w.size = 0
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

type HTTPManager struct {
	mu            sync.RWMutex
	srv           *http.Server
	port          string
	activeURLFile string
	handler       http.Handler
	logger        *slog.Logger
	onFatal       func()
}

func NewHTTPManager(handler http.Handler, activeURLFile string, logger *slog.Logger, onFatal func()) *HTTPManager {
	return &HTTPManager{
		handler:       handler,
		activeURLFile: activeURLFile,
		logger:        logger,
		onFatal:       onFatal,
	}
}

func (m *HTTPManager) StartOnPort(port string) error {
	pNum, err := strconv.Atoi(port)
	if err != nil || pNum < 1 || pNum > 65535 {
		return fmt.Errorf("端口号必须在 1~65535 之间")
	}
	// 默认监听全网卡以供局域网手机访问；若因极端组策略失败，提示明确原因
	ln, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		return fmt.Errorf("无法绑定端口 %s（可能已被其他软件占用）: %w", port, err)
	}

	newSrv := &http.Server{
		Handler:           m.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	m.mu.Lock()
	oldSrv := m.srv
	m.srv = newSrv
	m.port = port
	m.mu.Unlock()

	localURL := fmt.Sprintf("http://127.0.0.1:%s", port)
	setTrayLocalURL(localURL)
	if m.activeURLFile != "" {
		_ = os.WriteFile(m.activeURLFile, []byte(localURL), 0600)
	}

	go func() {
		if err := newSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			m.logger.Error("HTTP 监听异常退出", "port", port, "err", err)
			if m.onFatal != nil {
				m.onFatal()
			}
		}
	}()

	if oldSrv != nil {
		go func(s *http.Server) {
			time.Sleep(600 * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Shutdown(ctx)
		}(oldSrv)
	}
	return nil
}

func (m *HTTPManager) CurrentPort() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.port
}

func (m *HTTPManager) Shutdown(ctx context.Context) error {
	m.mu.RLock()
	srv := m.srv
	m.mu.RUnlock()
	if m.activeURLFile != "" {
		_ = os.Remove(m.activeURLFile)
	}
	if srv != nil {
		return srv.Shutdown(ctx)
	}
	return nil
}

var (
	trayURLMu      sync.RWMutex
	currentTrayURL = "http://127.0.0.1:8080"
)

func setTrayLocalURL(u string) {
	trayURLMu.Lock()
	currentTrayURL = u
	trayURLMu.Unlock()
}

func getTrayLocalURL() string {
	trayURLMu.RLock()
	defer trayURLMu.RUnlock()
	return currentTrayURL
}

func resolveExistingInstanceURL(activeURLFile string) string {
	if raw, err := os.ReadFile(activeURLFile); err == nil {
		u := strings.TrimSpace(string(raw))
		if strings.HasPrefix(u, "http://") {
			return u
		}
	}
	return "http://127.0.0.1:8080"
}

func main() {
	debug.SetMemoryLimit(256 * 1024 * 1024)

	exePath, err := os.Executable()
	if err != nil {
		exePath = "."
	}
	baseDir := filepath.Dir(exePath)

	portFlag := flag.String("port", "", "Override HTTP port")
	dataDir := flag.String("data", filepath.Join(baseDir, "data"), "Local database directory")
	backupDir := flag.String("backup", filepath.Join(baseDir, "backups"), "Backup directory (separated from data)")
	tzName := flag.String("tz", "Asia/Shanghai", "Host business timezone")
	noTray := flag.Bool("no-tray", false, "Disable system tray integration")
	noOpen := flag.Bool("no-open", false, "Do not auto-open browser on startup")
	autoStartMode := flag.Bool("autostart", false, "Launched silently by OS startup")
	resetAdminPwd := flag.Bool("reset-admin-pwd", false, "Reset all admin accounts password to admin123 and exit")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "创建数据目录失败: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*backupDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "创建备份目录失败: %v\n", err)
		os.Exit(1)
	}
	if filepath.Clean(*dataDir) == filepath.Clean(*backupDir) {
		fmt.Fprintln(os.Stderr, "拒绝启动: 备份目录必须与数据库目录分离")
		os.Exit(1)
	}

	activeURLFile := filepath.Join(*dataDir, "active.url")

	// 命令行直接执行管理员密码重置模式（无需启动 HTTP，即使主实例在跑也可通过 SQLite WAL 安全写入）
	if *resetAdminPwd {
		loc, _ := time.LoadLocation(*tzName)
		if loc == nil {
			loc = time.Local
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		st, err := NewStore(filepath.Join(*dataDir, "taskhub.db"), *backupDir, loc, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
			os.Exit(1)
		}
		err = st.ResetAdminPasswords(context.Background())
		_ = st.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "重置管理员密码失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("管理员密码已成功重置为初始密码: admin123")
		return
	}

	// 单实例运行检测：若已有实例正在运行，直接打开对应网页并立即退出当前重复进程
	releaseLock, alreadyRunning := acquireSingleInstance(*dataDir, activeURLFile)
	if alreadyRunning {
		openBrowser(resolveExistingInstanceURL(activeURLFile))
		return
	}
	defer releaseLock()

	logWriter, err := NewRotatingWriter(filepath.Join(*dataDir, "taskhub.log"), 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		os.Exit(1)
	}
	defer logWriter.Close()

	var logOut io.Writer = logWriter
	if runtime.GOOS != "windows" {
		logOut = io.MultiWriter(os.Stdout, logWriter)
	}
	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	loc, err := time.LoadLocation(*tzName)
	if err != nil {
		slog.Error("加载业务时区失败", "tz", *tzName, "err", err)
		os.Exit(1)
	}

	dbPath := filepath.Join(*dataDir, "taskhub.db")
	store, err := NewStore(dbPath, *backupDir, loc, logger)
	if err != nil {
		slog.Error("初始化存储与自检失败，拒绝启动", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 支持物理文件触发重置密码：若 data/ 目录下存在 RESET_ADMIN_PASSWORD 文件，自动重置并删除该文件
	resetTriggerFile := filepath.Join(*dataDir, "RESET_ADMIN_PASSWORD")
	if _, err := os.Stat(resetTriggerFile); err == nil {
		if rErr := store.ResetAdminPasswords(ctx); rErr == nil {
			_ = os.Remove(resetTriggerFile)
			slog.Warn("检测到物理重置文件 RESET_ADMIN_PASSWORD，已将管理员密码重置为 admin123")
		}
	}
	for _, ext := range []string{".txt", ".flag"} {
		if _, err := os.Stat(resetTriggerFile + ext); err == nil {
			if rErr := store.ResetAdminPasswords(ctx); rErr == nil {
				_ = os.Remove(resetTriggerFile + ext)
				slog.Warn("检测到物理重置文件，已将管理员密码重置为 admin123")
			}
		}
	}

	port := store.GetSavedPort(ctx)
	if *portFlag != "" {
		port = *portFlag
		_ = store.SetSavedPort(ctx, port)
	}

	// 启动补偿：立即检查补齐今日周期任务及昨日遗留的“次日接班复核”任务
	now := time.Now().In(loc)
	if genCount, err := store.GeneratePeriodicTasks(ctx, now); err != nil {
		slog.Error("启动补偿生成任务失败", "err", err)
	} else if genCount > 0 {
		slog.Info("启动补偿生成周期与复核任务完成", "generated", genCount)
	}

	app := NewServer(store, port, logger)
	httpMgr := NewHTTPManager(app.Routes(), activeURLFile, logger, cancel)
	app.SetPortChanger(func(newPort string) error {
		if newPort == httpMgr.CurrentPort() {
			return nil
		}
		if err := httpMgr.StartOnPort(newPort); err != nil {
			return err
		}
		if err := store.SetSavedPort(context.Background(), newPort); err != nil {
			return err
		}
		app.SetPort(newPort)
		logger.Info("管理员在线修改了监听端口", "new_port", newPort)
		return nil
	})

	if err := httpMgr.StartOnPort(port); err != nil {
		slog.Error("HTTP 服务启动失败", "port", port, "err", err)
		_ = store.Close()
		os.Exit(1)
	}

	slog.Info("服务已启动",
		"process", filepath.Base(exePath),
		"pid", os.Getpid(),
		"lan_url", fmt.Sprintf("http://%s:%s", detectLANIP(), port),
	)

	// 手动双击启动时，默认自动弹出浏览器打开管理页（开机静默自启或显式 -no-open 时不打扰）
	if !*noOpen && !*autoStartMode {
		go func() {
			time.Sleep(220 * time.Millisecond)
			openBrowser(getTrayLocalURL())
		}()
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runScheduler(ctx, store, loc, logger)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			slog.Info("捕获退出信号，开始优雅关闭", "signal", sig.String())
			cancel()
		case <-ctx.Done():
		}
	}()

	procName := fmt.Sprintf("%s (PID: %d)", filepath.Base(exePath), os.Getpid())
	if *noTray {
		<-ctx.Done()
	} else {
		runTray(ctx, cancel, procName, store, logger)
	}

	slog.Info("正在关闭 HTTP 服务与后台协程...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()

	if err := httpMgr.Shutdown(shutCtx); err != nil {
		slog.Error("HTTP 优雅关闭超时", "err", err)
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		slog.Info("所有后台协程已安全退出")
	case <-shutCtx.Done():
		slog.Warn("等待后台协程超时")
	}

	if err := store.Close(); err != nil {
		slog.Error("数据库关闭异常", "err", err)
	} else {
		slog.Info("数据库连接已安全关闭，进程彻底退出")
	}
}

func runScheduler(ctx context.Context, store *Store, loc *time.Location, logger *slog.Logger) {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()

	lastGenDate := ""
	lastMaintDate := ""

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			now := time.Now().In(loc)
			today := now.Format("2006-01-02")

			if (now.Hour() > 0 || (now.Hour() == 0 && now.Minute() >= 5)) && lastGenDate != today {
				n, err := store.GeneratePeriodicTasks(ctx, now)
				if err != nil {
					logger.Error("定时生成周期及复核任务失败", "date", today, "err", err)
				} else {
					lastGenDate = today
					logger.Info("定时周期与次日复核任务检查完成", "date", today, "generated", n)
				}
			}

			if now.Hour() == 3 && lastMaintDate != today {
				_ = store.RunDailyMaintenance(ctx, now)
				backupFile, err := store.PerformBackup(ctx, now)
				if err != nil {
					logger.Error("每日自动备份失败", "date", today, "err", err)
				} else {
					lastMaintDate = today
					logger.Info("每日自动维护与备份完整性校验成功", "file", backupFile)
				}
			}
		}
	}
}

func detectLANIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	var fallback string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			if ip4[0] == 192 && ip4[1] == 168 {
				return ip4.String()
			}
			if ip4[0] == 10 || (ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) {
				if fallback == "" {
					fallback = ip4.String()
				}
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return "127.0.0.1"
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}
