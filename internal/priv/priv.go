// Package priv 是全项目唯一允许执行特权操作（exec / netlink / nft / sysctl）的包。
// 业务模块一律通过这里的导出函数间接操作，便于审计与 --dry-run 演练。
package priv

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Mode 运行形态："real" 真机操作；"dev" 提供假数据（UI 开发，Windows/非 root 可用）
var Mode = "real"

// DryRun 演练开关：true 时命令只打印不执行
var DryRun = false

var logger *slog.Logger

// SetLogger 注入审计日志器（main 装配时调用一次）
func SetLogger(l *slog.Logger) { logger = l }

func audit(msg string, args ...any) {
	if logger != nil {
		logger.Info("PRIV "+msg, args...)
	}
}

// CmdResult 一次命令执行的结果
type CmdResult struct {
	Stdout   string
	Stderr   string
	Err      error // 启动失败/超时；非零退出码也是 Err（*exec.ExitError）
	Duration time.Duration
}

// Run 带超时执行外部命令，捕获 stdout/stderr 并写审计日志。
// dev 模式下返回空结果；DryRun 下只打印不执行。
func Run(timeout time.Duration, name string, args ...string) CmdResult {
	if Mode == "dev" {
		return CmdResult{Stdout: devExec(name, args)}
	}
	if DryRun {
		line := strings.TrimSpace(name + " " + strings.Join(args, " "))
		audit("dry-run", "cmd", line)
		return CmdResult{}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	d := time.Since(start)
	audit("exec", "cmd", name+" "+strings.Join(args, " "), "err", err, "dur", d.String())
	return CmdResult{Stdout: out.String(), Stderr: errb.String(), Err: err, Duration: d}
}

// RunOK 便捷封装：Err 为 nil 时返回 true
func RunOK(timeout time.Duration, name string, args ...string) (string, bool) {
	r := Run(timeout, name, args...)
	return r.Stdout, r.Err == nil
}

// RunQuiet 与 Run 相同，但不写审计日志——专用于秒级轮询命令
// （ethtool/iw dev/nft 计数等），避免巡检流量淹没日志页。
// 排障时可临时改回 Run，或直接在终端手工执行同命令观察。
func RunQuiet(timeout time.Duration, name string, args ...string) CmdResult {
	if Mode == "dev" {
		return CmdResult{Stdout: devExec(name, args)}
	}
	if DryRun {
		return CmdResult{}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return CmdResult{Stdout: out.String(), Stderr: errb.String(), Err: err, Duration: time.Since(start)}
}