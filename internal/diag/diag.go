// Package diag 网络诊断工具：从路由器本机执行 ping / 路由跟踪 / DNS 解析 / TCP 连通测试。
// 全部走直接 exec（无 shell 拼接）+ 目标字符白名单 + 严格超时，输出截断防爆内存。
package diag

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// 目标白名单：域名/IP:端口/IPv6 冒号形式
var targetRe = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,253}$`)

const (
	runTimeout = 15 * time.Second
	maxOutput  = 8 * 1024 // 8KB
)

// Tools 支持的诊断工具（UI 下拉与此一致）
func Tools() []string { return []string{"ping", "traceroute", "dns", "tcp"} }

// Run 执行一项诊断，返回给 UI 展示的文本输出。
// dev 模式（无真实网络面/非 root）返回演示输出，保证 UI 全流程可走。
func Run(dev bool, tool, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("目标不能为空")
	}
	if !targetRe.MatchString(target) {
		return "", fmt.Errorf("目标含非法字符（仅允许字母数字 . _ : [ ] - ）")
	}
	if dev {
		return fake(tool, target), nil
	}
	switch tool {
	case "ping":
		return run("ping", "-c", "4", "-W", "2", "-n", target)
	case "traceroute":
		// traceroute 未安装时回退 tracepath（iproute2 自带，Debian 基本都有）
		if _, err := exec.LookPath("traceroute"); err != nil {
			return run("tracepath", "-n", target)
		}
		return run("traceroute", "-n", "-m", "15", "-w", "1", target)
	case "dns":
		return dnsLookup(target)
	case "tcp":
		return tcpProbe(target)
	default:
		return "", fmt.Errorf("未知工具 %q（支持 ping/traceroute/dns/tcp）", tool)
	}
}

// run 直接 exec（不经 shell），合并 stdout/stderr，限时，截断
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return trim(string(out)), fmt.Errorf("%s 超时（>%ds）", name, int(runTimeout.Seconds()))
	}
	if err != nil {
		// ping 不通、traceroute 到不了目标都属正常诊断结果，把输出交还 UI 而非报错
		if len(out) > 0 {
			return trim(string(out)), nil
		}
		return "", fmt.Errorf("%s 执行失败: %v", name, err)
	}
	return trim(string(out)), nil
}

// dnsLookup nslookup → host → getent 三级回退（不同发行版工具集不同）
func dnsLookup(target string) (string, error) {
	if _, err := exec.LookPath("nslookup"); err == nil {
		return run("nslookup", target)
	}
	if _, err := exec.LookPath("host"); err == nil {
		return run("host", target)
	}
	return run("getent", "hosts", target)
}

// tcpProbe 纯 Go TCP 连通测试（无需 nc），输出 ping 风格
func tcpProbe(addr string) (string, error) {
	if !strings.Contains(addr, ":") {
		// 没给端口则无意义，提示而非猜
		return "", fmt.Errorf("TCP 测试目标需带端口，如 %s:22", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("地址格式: %v", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "TCP 连接测试 %s\n", addr)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		fmt.Fprintf(&b, "连接失败: %v（耗时 %dms）\n", err, time.Since(start).Milliseconds())
		return b.String(), nil
	}
	_ = conn.Close()
	fmt.Fprintf(&b, "连接成功（耗时 %dms）\n", time.Since(start).Milliseconds())
	return b.String(), nil
}

func trim(s string) string {
	if len(s) > maxOutput {
		return s[:maxOutput] + "\n…（输出过长已截断）"
	}
	return s
}

// fake dev 模式演示输出
func fake(tool, target string) string {
	switch tool {
	case "ping":
		return fmt.Sprintf("PING %s (93.184.216.34) 56(84) bytes of data.\n64 bytes from %s: icmp_seq=1 ttl=55 time=8.42 ms\n64 bytes from %s: icmp_seq=2 ttl=55 time=8.11 ms\n64 bytes from %s: icmp_seq=3 ttl=55 time=8.35 ms\n64 bytes from %s: icmp_seq=4 ttl=55 time=8.27 ms\n\n--- %s ping statistics ---\n4 packets transmitted, 4 received, 0%% packet loss\n（开发模式演示数据）", target, target, target, target, target, target)
	case "traceroute":
		return fmt.Sprintf("traceroute to %s, 15 hops max\n 1  192.168.50.1  0.5 ms\n 2  10.0.0.1  2.1 ms\n 3  * * *\n 4  %s  8.4 ms\n（开发模式演示数据）", target, target)
	case "dns":
		return fmt.Sprintf("Server:\t\t223.5.5.5\nAddress:\t223.5.5.5#53\n\nName: %s\nAddress: 93.184.216.34\n（开发模式演示数据）", target)
	case "tcp":
		return fmt.Sprintf("TCP 连接测试 %s\n连接成功（耗时 8ms）\n（开发模式演示数据）", target)
	}
	return ""
}
