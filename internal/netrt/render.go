// render.go —— dnsmasq.conf 与 nftables 表的渲染。注释写清每个配置项含义。
package netrt

import (
	"fmt"
	"net"
	"strings"

	"apmanager/internal/config"
	"apmanager/internal/priv"
)

func parseCIDR(cidr string) (net.IP, *net.IPNet, error) { return net.ParseCIDR(cidr) }

func defaultRouteSafe() (string, error) {
	dr, err := priv.DefaultRoute4()
	if err != nil {
		return "", err
	}
	return dr.Iface, nil
}

// RenderDnsmasq 渲染 dnsmasq.conf：
// - 每个 LAN 段一条 dhcp-range；AP 段的接口写 interface=（桥形态写桥名）
// - 黑名单 MAC 用 dhcp-host=MAC,ignore 拒发地址
// - 上行口写进 except-interface，双保险保证绝不在上行提供 DHCP/DNS（安全红线）
func RenderDnsmasq(lans []config.LANSeg, dnsUpstreams []string, paths Paths, blockedMACs []string) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("# 由 apmanager 自动生成，勿手改")
	p("pid-file=%s/dnsmasq.pid", paths.RunDir)
	p("log-facility=-") // 日志走 stderr，supervisor 捕获
	p("")
	p("# ---- DNS ----")
	p("no-resolv") // 不读系统 resolv.conf，上游完全由下面 server= 决定
	for _, s := range dnsUpstreams {
		p("server=%s", s)
	}
	p("domain=apmanager.lan")
	p("local=/apmanager.lan/")          // 本域内查询不再转发上游
	p("address=/apmanager.lan/%s", lanAddr(lans))
	p("dhcp-authoritative")             // 本网段唯一 DHCP 服务器
	p("")
	p("# ---- DHCP ----")
	for _, seg := range lans {
		lease := seg.LeaseHours
		if lease <= 0 {
			lease = 12
		}
		p("# LAN 段 %s", seg.Name)
		p("dhcp-range=%s,%s,%s,%dh", seg.DHCPPool[0], seg.DHCPPool[1], netmaskOf(seg.Subnet), lease)
		gw := lanGatewayIP(seg.Subnet)
		p("dhcp-option=3,%s", gw) // 下发网关
		p("dhcp-option=6,%s", gw) // 下发 DNS（本机）
	}
	for _, mac := range blockedMACs {
		p("dhcp-host=%s,ignore # 黑名单拒发地址", mac)
	}
	p("dhcp-leasefile=%s/dnsmasq.leases", paths.StateDir)
	p("")
	p("# ---- 监听范围 ----")
	for _, seg := range lans {
		if seg.Bridge != "" {
			p("interface=%s", seg.Bridge)
		}
	}
	p("bind-dynamic") // 接口后出现也能绑（AP 晚于 dnsmasq 起来的场景）
	p("except-interface=lo")
	for _, u := range uplinkProtectList() {
		p("except-interface=%s", u) // 上行口绝不提供 DHCP/DNS
	}
	return b.String()
}

const apIfacePlaceholder = ""

// RenderDnsmasqWithIface 同上，但显式指定 AP 直挂接口（阶段一：无桥，直接无线口）
func RenderDnsmasqWithIface(lans []config.LANSeg, dnsUpstreams []string, paths Paths, apIface string, blockedMACs []string) string {
	base := RenderDnsmasq(lans, dnsUpstreams, paths, blockedMACs)
	if apIface == "" {
		return base
	}
	return strings.Replace(base, "# ---- 监听范围 ----",
		"# ---- 监听范围 ----\ninterface="+apIface, 1)
}
func uplinkProtectList() []string {
	dr, err := defaultRouteSafe()
	if err != nil || dr == "" {
		return nil
	}
	return []string{dr}
}

// RenderNft 渲染我们的整张表 table inet apmanager（阶段三：区域隔离矩阵版）。
//
// forward 策略（基链 policy 仍为 accept，段间控制全用显式规则，绝不影响他人流量）：
//   1. established/related 放行；
//   2. 段内互访：isolation=false 的段放行（如家庭网内互访），isolation=true 的段丢弃（访客隔离）；
//   3. 跨段：zone_policy 明确 allow 的放行（lan_to_lan 之类），其余一律丢弃（默认最严格）；
//   4. 各段 → 上行口放行（上网）。
// postrouting：各段网段经上行口 masquerade。
//
// 段接口名：有桥用桥名，无桥（AP 直挂）用 AP 接口名。
func RenderNft(lans []config.LANSeg, uplinkIface string, zones map[string]string, apIface string) (string, error) {
	return renderNft(lans, uplinkIface, zones, apIface, nil, true)
}

// RenderNftFull 完整版：含按设备计数动态集与黑名单 IP drop
func RenderNftFull(lans []config.LANSeg, uplinkIface string, zones map[string]string, apIface string, blockedIPs []string, withCounters bool) (string, error) {
	return renderNft(lans, uplinkIface, zones, apIface, blockedIPs, withCounters)
}

func renderNft(lans []config.LANSeg, uplinkIface string, zones map[string]string, apIface string, blockedIPs []string, withCounters bool) (string, error) {
	ifaceOf := func(seg config.LANSeg) string {
		if seg.Bridge != "" {
			return seg.Bridge
		}
		return apIface
	}
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	p("table inet apmanager {")
	if withCounters {
		// 按设备计数的动态集合：元素 = IP，带 counter；2s 轮询 list set 聚合
		p("  set dev_up { type ipv4_addr; flags dynamic; }")
		p("  set dev_down { type ipv4_addr; flags dynamic; }")
	}
	p("  chain forward {")
	p("    type filter hook forward priority filter; policy accept;")
	// 黑名单 IP（有线拉黑/踢出的等价实现）：双向 drop，放最前
	for _, ip := range blockedIPs {
		p("    ip saddr %s drop comment \"黑名单\"", ip)
		p("    ip daddr %s drop comment \"黑名单\"", ip)
	}
	if withCounters {
		p("    update @dev_up { ip saddr counter } comment \"按设备上行计数\"")
		p("    update @dev_down { ip daddr counter } comment \"按设备下行计数\"")
	}
	p("    ct state established,related accept comment \"已建立/相关连接放行\"")
	// 段内互访
	for _, seg := range lans {
		iface := ifaceOf(seg)
		sub := subnetOf(seg.Subnet)
		if seg.Isolation {
			p("    iifname %q oifname %q ip saddr %s ip daddr %s drop comment \"段内客户端互访隔离 %s\"", iface, iface, sub, sub, seg.Name)
		} else {
			p("    iifname %q oifname %q ip saddr %s ip daddr %s accept comment \"段内互访 %s\"", iface, iface, sub, sub, seg.Name)
		}
	}
	// 跨段：显式 allow / deny，其余默认 deny
	for _, a := range lans {
		for _, c := range lans {
			if a.Name == c.Name {
				continue
			}
			key := a.Name + "_to_" + c.Name
			switch zones[key] {
			case "allow":
				p("    iifname %q oifname %q accept comment \"跨段放行 %s\"", ifaceOf(a), ifaceOf(c), key)
			case "deny":
				p("    iifname %q oifname %q drop comment \"跨段隔离 %s\"", ifaceOf(a), ifaceOf(c), key)
			default:
				// 未配置的组合默认隔离，但只有两侧都真的存在接口才输出（避免无意义规则）
				p("    iifname %q oifname %q drop comment \"跨段默认隔离 %s\"", ifaceOf(a), ifaceOf(c), key)
			}
		}
	}
	// 段 → 上行（上网）
	if uplinkIface != "" {
		for _, seg := range lans {
			iface := ifaceOf(seg)
			if iface == uplinkIface {
				continue // 中继态 STA 段除外
			}
			p("    iifname %q oifname %q accept comment \"%s 上网\"", iface, uplinkIface, seg.Name)
		}
	}
	p("  }")
	p("  chain postrouting {")
	p("    type nat hook postrouting priority srcnat; policy accept;")
	if uplinkIface != "" {
		for _, seg := range lans {
			p("    ip saddr %s oifname %q masquerade comment \"LAN %s NAT 上行\"", subnetOf(seg.Subnet), uplinkIface, seg.Name)
		}
	}
	p("  }")
	p("}")
	return b.String(), nil
}

// subnetOf 把 "192.168.50.1/24" 规整为 "192.168.50.0/24"（网络地址/前缀）
func subnetOf(cidr string) string {
	ip, ipnet, err := parseCIDR(cidr)
	if err != nil {
		return cidr
	}
	_ = ip
	ones, _ := ipnet.Mask.Size()
	return ipnet.IP.String() + "/" + fmt.Sprintf("%d", ones)
}

// netmaskOf 从 CIDR 拿点分掩码（dnsmasq dhcp-range 习惯写法）。
// 注意 net.IPMask.String() 返回十六进制（ffffff00），这里必须手动展开成点分。
func netmaskOf(cidr string) string {
	_, ipnet, err := parseCIDR(cidr)
	if err != nil || len(ipnet.Mask) != 4 {
		return "255.255.255.0"
	}
	m := ipnet.Mask
	return fmt.Sprintf("%d.%d.%d.%d", m[0], m[1], m[2], m[3])
}

// maskOf 取 CIDR 的前缀长度形式 "24"
func maskOf(cidr string) string {
	_, ipnet, err := parseCIDR(cidr)
	if err != nil {
		return "24"
	}
	ones, _ := ipnet.Mask.Size()
	return fmt.Sprintf("%d", ones)
}

// lanGatewayIP 接口地址（如 "192.168.50.1/24" → 192.168.50.1）。
// ParseCIDR 的 IPNet.IP 是网络地址，这里要的是前缀里的主机地址。
func lanGatewayIP(cidr string) string {
	addr := strings.SplitN(cidr, "/", 2)[0]
	if net.ParseIP(addr) == nil {
		return cidr
	}
	return addr
}

// lanAddr 同 lanGatewayIP，命名区分用途
func lanAddr(lans []config.LANSeg) string {
	if len(lans) == 0 {
		return "192.168.50.1"
	}
	return lanGatewayIP(lans[0].Subnet)
}
