//go:build !windows

package main

import (
	"context"
	"log/slog"
)

func runTray(ctx context.Context, _ context.CancelFunc, procName string, logger *slog.Logger) {
	logger.Info("非 Windows 平台守护运行中（按 Ctrl+C 或发送 SIGTERM 彻底退出）",
		"process", procName,
		"url", getTrayLocalURL(),
	)
	<-ctx.Done()
}
