// tc.go —— 按设备限速（HTB）。
// 方向约定（PLAN §3.5）：下载 = LAN 桥/AP 口 egress 按 dst IP 分类；
// 上传 = WAN 口 egress 按 src IP 分类。变更即整体重建 qdisc 树（设备量小，重建最可靠）。
package priv

import (
	"fmt"
	"strings"
	"time"
)

// RateLimit 一台设备的限速值（0 = 不限）
type RateLimit struct {
	IP        string
	DownMbps  int
	UpMbps    int
}

// ApplyRateLimits 在 lanIfaces（LAN 侧出口，可多个）与 wanIface 上（重）建限速树。
// 全量重建：先清根 qdisc，再按 limits 建类与过滤器。lanDefaultMbps 是 LAN 侧默认类速率。
func ApplyRateLimits(lanIfaces []string, wanIface string, limits []RateLimit) error {
	if Mode == "dev" || DryRun {
		audit("tc(dev)", "limits", fmt.Sprint(limits))
		return nil
	}
	byIP := map[string]RateLimit{}
	for _, l := range limits {
		if l.IP != "" && (l.DownMbps > 0 || l.UpMbps > 0) {
			byIP[l.IP] = l
		}
	}
	// LAN 侧（下载方向）
	for _, lan := range lanIfaces {
		if err := buildHTB(lan, "dst", byIP); err != nil {
			return fmt.Errorf("LAN 侧 %s: %w", lan, err)
		}
	}
	// WAN 侧（上传方向）
	if wanIface != "" {
		if err := buildHTB(wanIface, "src", byIP); err != nil {
			return fmt.Errorf("WAN 侧 %s: %w", wanIface, err)
		}
	}
	return nil
}

// ClearRateLimits 清掉指定接口上的限速树
func ClearRateLimits(ifaces ...string) {
	if Mode == "dev" || DryRun {
		return
	}
	for _, ifn := range ifaces {
		runTc("qdisc", "del", "dev", ifn, "root")
	}
}

// buildHTB 在一个接口上建 HTB 树；match 为 "dst"（下载）或 "src"（上传）
func buildHTB(iface, match string, byIP map[string]RateLimit) error {
	// 只给有对应方向限速的设备建类
	type cls struct {
		cid  string
		ip   string
		mbps int
	}
	var classes []cls
	next := 10
	for ip, l := range byIP {
		mbps := l.DownMbps
		if match == "src" {
			mbps = l.UpMbps
		}
		if mbps <= 0 {
			continue
		}
		classes = append(classes, cls{fmt.Sprintf("1:%d", next), ip, mbps})
		next++
	}
	if len(classes) == 0 {
		// 没有该方向的限速：确保接口上无残留树
		runTc("qdisc", "del", "dev", iface, "root")
		return nil
	}
	// 1. 根 qdisc（default 9999 = 未匹配流量走默认不限速类）
	runTc("qdisc", "replace", "dev", iface, "root", "handle", "1:", "htb", "default", "9999")
	// 2. 默认类（近似不限速）
	runTc("class", "replace", "dev", iface, "parent", "1:", "classid", "1:9999", "htb", "rate", "10gbit")
	// 3. 每设备一类一过滤器
	for _, c := range classes {
		if r := runTc("class", "replace", "dev", iface, "parent", "1:", "classid", c.cid,
			"htb", "rate", fmt.Sprintf("%dmbit", c.mbps), "ceil", fmt.Sprintf("%dmbit", c.mbps)); r.Err != nil {
			return fmt.Errorf("class %s: %s", c.cid, oneLine(r.Stderr))
		}
		args := []string{"filter", "replace", "dev", iface, "parent", "1:", "protocol", "ip",
			"prio", "1", "u32"}
		if match == "dst" {
			args = append(args, "match", "ip", "dst", c.ip)
		} else {
			args = append(args, "match", "ip", "src", c.ip)
		}
		args = append(args, "flowid", c.cid)
		if r := runTc(args...); r.Err != nil {
			return fmt.Errorf("filter %s: %s", c.ip, oneLine(r.Stderr))
		}
	}
	return nil
}

func runTc(args ...string) CmdResult {
	return Run(5*time.Second, "tc", args...)
}

// HasTC 检查接口是否已有我们的 qdisc 树（诊断用）
func HasTC(iface string) bool {
	r := runTc("qdisc", "show", "dev", iface)
	return strings.Contains(r.Stdout, "htb 1:")
}
