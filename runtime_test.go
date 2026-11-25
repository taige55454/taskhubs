package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func getFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配空闲测试端口失败: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	return port
}

// 1. 测试日志滚动写入器 RotatingWriter 自动切分 .1 备份文件
func TestRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "logs", "app.log")

	rw, err := NewRotatingWriter(logPath, 1) // 1MB 上限
	if err != nil {
		t.Fatalf("创建 RotatingWriter 失败: %v", err)
	}

	chunk := bytes.Repeat([]byte("A"), 600*1024) // 600KB
	if _, err := rw.Write(chunk); err != nil {
		t.Fatal(err)
	}
	// 第二次写入超过 1MB，触发滚动重命名为 app.log.1
	if _, err := rw.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("超出体积上限后应生成滚动日志文件 .1: %v", err)
	}
	info, err := os.Stat(logPath)
	if err != nil || info.Size() != int64(len(chunk)) {
		t.Fatalf("新日志文件大小应为 %d，实际 %v", len(chunk), info)
	}
}

// 2. 测试 HTTPManager 端口真实监听、在线无缝热切换端口、单实例探测与优雅关闭
func TestHTTPManagerHotPortSwitchAndSingleInstance(t *testing.T) {
	st, loc := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(st, "8080", logger)

	dataDir := t.TempDir()
	activeURLFile := filepath.Join(dataDir, "active.url")

	// 默认不存在 active.url 时应回退至 127.0.0.1:8080
	if got := resolveExistingInstanceURL(activeURLFile); got != "http://127.0.0.1:8080" {
		t.Fatalf("默认回退地址异常: %s", got)
	}

	mgr := NewHTTPManager(srv.Routes(), activeURLFile, logger, nil)

	// 非法端口号拦截
	if err := mgr.StartOnPort("0"); err == nil {
		t.Fatal("端口 0 应当报错拦截")
	}
	if err := mgr.StartOnPort("99999"); err == nil {
		t.Fatal("超范围端口应当报错拦截")
	}

	// 启动第一个真实端口
	port1 := getFreePort(t)
	if err := mgr.StartOnPort(port1); err != nil {
		t.Fatalf("启动端口 %s 失败: %v", port1, err)
	}
	if mgr.CurrentPort() != port1 || !strings.HasSuffix(getTrayLocalURL(), ":"+port1) {
		t.Fatal("当前监听端口状态未同步")
	}
	if got := resolveExistingInstanceURL(activeURLFile); got != "http://127.0.0.1:"+port1 {
		t.Fatalf("active.url 内容不符合预期: %s", got)
	}

	// 验证真实 HTTP 请求可达
	resp1, err := http.Get("http://127.0.0.1:" + port1 + "/healthz")
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("访问端口1 /healthz 失败: %v", err)
	}
	_ = resp1.Body.Close()

	// 验证单实例互斥锁检测
	rel1, running1 := acquireSingleInstance(dataDir, activeURLFile)
	defer rel1()
	// 在非 Windows 下由于上方 HTTPManager 已写 active.url 且 /healthz 存活，会直接测出 alreadyRunning=true；
	// 在 Windows 下若本机无其他实例则第1次获取锁(running1=false)，再调第2次必返回 alreadyRunning=true
	if !running1 {
		rel2, running2 := acquireSingleInstance(dataDir, activeURLFile)
		defer rel2()
		if !running2 {
			t.Fatal("重复获取单实例锁应当返回 alreadyRunning = true")
		}
	}

	// 在线热切换至第二个真实端口，旧端口自动优雅排空关闭
	port2 := getFreePort(t)
	if err := mgr.StartOnPort(port2); err != nil {
		t.Fatalf("热切换至新端口 %s 失败: %v", port2, err)
	}
	resp2, err := http.Get("http://127.0.0.1:" + port2 + "/healthz")
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("热切换后访问新端口 /healthz 失败: %v", err)
	}
	_ = resp2.Body.Close()

	// 测试后台调度器协程可随 Context 立即干净退出，无协程泄漏
	schedCtx, schedCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runScheduler(schedCtx, st, loc, logger)
		close(done)
	}()
	schedCancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runScheduler 未能响应 Context 取消及时退出")
	}

	// 验证局域网 IP 探测返回值合法
	if ip := detectLANIP(); ip == "" {
		t.Fatal("detectLANIP 不应返回空字符串")
	}

	// 优雅关闭 HTTPManager 并确认清理 active.url
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	if err := mgr.Shutdown(shutCtx); err != nil {
		t.Fatalf("HTTPManager 优雅关闭失败: %v", err)
	}
	if _, err := os.Stat(activeURLFile); !os.IsNotExist(err) {
		t.Fatal("关闭后应自动清理 active.url 文件")
	}
}
