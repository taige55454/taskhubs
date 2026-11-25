//go:build !windows

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func acquireSingleInstance(dataDir, activeURLFile string) (func(), bool) {
	lockFile := filepath.Join(dataDir, "taskhub.pid")
	if raw, err := os.ReadFile(activeURLFile); err == nil {
		u := strings.TrimSpace(string(raw))
		if strings.HasPrefix(u, "http://") {
			client := &http.Client{Timeout: 400 * time.Millisecond}
			if resp, gErr := client.Get(u + "/healthz"); gErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return func() {}, true
				}
			}
		}
	}
	_ = os.WriteFile(lockFile, []byte(strconv.Itoa(os.Getpid())), 0600)
	return func() {
		_ = os.Remove(lockFile)
	}, false
}

func runTray(ctx context.Context, _ context.CancelFunc, procName string, _ *Store, logger *slog.Logger) {
	logger.Info("非 Windows 平台守护运行中（按 Ctrl+C 或发送 SIGTERM 彻底退出）",
		"process", procName,
		"url", getTrayLocalURL(),
	)
	<-ctx.Done()
}
