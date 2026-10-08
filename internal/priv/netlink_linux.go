//go:build linux

// netlink_linux.go —— 接口/地址/路由/桥的 netlink 封装（仅真机模式生效）。
package priv

import (
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

// LinkInfo 一个网络接口的快照
type LinkInfo struct {
	Name      string
	MAC       string
	Type      string // bridge / vlan / device / loopback / dummy …
	IsWireless bool
	OperState string // up / down / lowerlayerdown / unknown …
	Master    string // 所属桥名（非桥成员为空）
	MTU       int
	Addrs     []string // CIDR 列表
	Stats     LinkStats
	TxQLen    int
}

// LinkStats 收发统计
type LinkStats struct {
	RxBytes, TxBytes           uint64
	RxPackets, TxPackets       uint64
	RxErrors, TxErrors         uint64
	RxDropped, TxDropped       uint64
}

// DefaultRoute 当前默认路由（接口名 + 网关）
type DefaultRoute struct {
	Iface string
	GW    net.IP
}

// AllLinks dump 全部接口（dev 模式返回假数据）
func AllLinks() ([]LinkInfo, error) {
	if Mode == "dev" {
		return devLinks(), nil
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	var out []LinkInfo
	for _, l := range links {
		attrs := l.Attrs()
		li := LinkInfo{
			Name:      attrs.Name,
			MAC:       attrs.HardwareAddr.String(),
			Type:      l.Type(),
			OperState: attrs.OperState.String(),
			Master:    bridgeNameOf(attrs.MasterIndex),
			MTU:       attrs.MTU,
			TxQLen:    attrs.TxQLen,
			IsWireless: isWireless(attrs.Name),
		}
		if st := attrs.Statistics; st != nil {
			li.Stats = LinkStats{
				RxBytes: st.RxBytes, TxBytes: st.TxBytes,
				RxPackets: st.RxPackets, TxPackets: st.TxPackets,
				RxErrors: st.RxErrors, TxErrors: st.TxErrors,
				RxDropped: st.RxDropped, TxDropped: st.TxDropped,
			}
		}
		addrs, err := netlink.AddrList(l, netlink.FAMILY_ALL)
		if err == nil {
			for _, a := range addrs {
				if a.IPNet != nil {
					li.Addrs = append(li.Addrs, a.IPNet.String())
				}
			}
		}
		out = append(out, li)
	}
	return out, nil
}

// LinkByName 取单个接口
func LinkByName(name string) (LinkInfo, error) {
	links, err := AllLinks()
	if err != nil {
		return LinkInfo{}, err
	}
	for _, l := range links {
		if l.Name == name {
			return l, nil
		}
	}
	return LinkInfo{}, fmt.Errorf("接口 %s 不存在", name)
}

func bridgeNameOf(idx int) string {
	if idx == 0 {
		return ""
	}
	if l, err := netlink.LinkByIndex(idx); err == nil {
		return l.Attrs().Name
	}
	return ""
}

func isWireless(name string) bool {
	if _, err := os.Stat("/sys/class/net/" + name + "/phy80211"); err == nil {
		return true
	}
	_, err := os.Stat("/sys/class/net/" + name + "/wireless")
	return err == nil
}

// SetUp 拉起接口
func SetUp(name string) error {
	if Mode == "dev" {
		return nil
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(l)
}

// SetDown 关闭接口
func SetDown(name string) error {
	if Mode == "dev" {
		return nil
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetDown(l)
}

// AddrReplace 设置地址（替换同前缀旧地址语义：先 flush 同 family 再 add，简化对账）
func AddrReplace(name, cidr string) error {
	if Mode == "dev" {
		return nil
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return fmt.Errorf("非法地址 %s: %w", cidr, err)
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	// 清掉该接口上同 family 的旧地址，保证幂等（回滚快照恢复时另行处理）
	old, _ := netlink.AddrList(l, netlink.FAMILY_V4)
	for _, a := range old {
		if !a.IP.IsLoopback() {
			_ = netlink.AddrDel(l, &a)
		}
	}
	return netlink.AddrAdd(l, addr)
}

// AddrFlush 清空接口上的 IPv4 地址（不动 IPv6 链路本地）
func AddrFlush(name string) error {
	if Mode == "dev" {
		return nil
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	old, _ := netlink.AddrList(l, netlink.FAMILY_V4)
	for _, a := range old {
		_ = netlink.AddrDel(l, &a)
	}
	return nil
}

// BridgeCreate 建桥（已存在则 no-op）
func BridgeCreate(name string) error {
	if Mode == "dev" {
		return nil
	}
	if _, err := netlink.LinkByName(name); err == nil {
		return nil // 已存在
	}
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(br); err != nil {
		return err
	}
	return netlink.LinkSetUp(br)
}

// BridgeDelete 删桥
func BridgeDelete(name string) error {
	if Mode == "dev" {
		return nil
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil
	}
	return netlink.LinkDel(l)
}

// BridgeAddPort 把 port 挂进 bridge（ enslaved）
func BridgeAddPort(bridge, port string) error {
	if Mode == "dev" {
		return nil
	}
	brl, err := netlink.LinkByName(bridge)
	if err != nil {
		return err
	}
	pl, err := netlink.LinkByName(port)
	if err != nil {
		return err
	}
	return netlink.LinkSetMaster(pl, brl)
}

// BridgeDelPort 把 port 从桥里摘出
func BridgeDelPort(port string) error {
	if Mode == "dev" {
		return nil
	}
	pl, err := netlink.LinkByName(port)
	if err != nil {
		return err
	}
	return netlink.LinkSetNoMaster(pl)
}

// DefaultRoute4 取 IPv4 默认路由（metric 最小者优先）
func DefaultRoute4() (DefaultRoute, error) {
	if Mode == "dev" {
		return DefaultRoute{Iface: "eth-mock", GW: net.ParseIP("192.168.68.1")}, nil
	}
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return DefaultRoute{}, err
	}
	best := -1
	var dr DefaultRoute
	for i, r := range routes {
		if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" {
			if best == -1 || r.Priority < routes[best].Priority {
				best = i
			}
		}
	}
	if best == -1 {
		return DefaultRoute{}, fmt.Errorf("无默认路由")
	}
	dr.Iface = ifName(routes[best].LinkIndex)
	dr.GW = routes[best].Gw
	return dr, nil
}

// NeighEntry 邻居表项（设备来源探测用）
type NeighEntry struct {
	IP    string
	MAC   string
	Iface string
	State string
}

// NeighDump 读 ARP/邻居表
func NeighDump() ([]NeighEntry, error) {
	if Mode == "dev" {
		return nil, nil
	}
	nl, err := netlink.NeighList(0, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	var out []NeighEntry
	for _, n := range nl {
		if n.IP == nil || n.HardwareAddr == nil {
			continue
		}
		out = append(out, NeighEntry{
			IP: n.IP.String(), MAC: n.HardwareAddr.String(),
			Iface: ifName(n.LinkIndex), State: int2state(n.State),
		})
	}
	return out, nil
}

func ifName(idx int) string {
	if l, err := netlink.LinkByIndex(idx); err == nil {
		return l.Attrs().Name
	}
	return fmt.Sprintf("if%d", idx)
}

func int2state(s int) string {
	switch s {
	case netlink.NUD_REACHABLE, netlink.NUD_STALE, netlink.NUD_DELAY, netlink.NUD_PROBE:
		return "reachable"
	case netlink.NUD_INCOMPLETE:
		return "incomplete"
	case netlink.NUD_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}

// EnableIPForward 打开内核转发（幂等；每次对账循环确保）
func EnableIPForward() error {
	if Mode == "dev" {
		return nil
	}
	return sysctlWrite("/proc/sys/net/ipv4/ip_forward", "1")
}

func sysctlWrite(path, val string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) == val {
		return nil
	}
	return os.WriteFile(path, []byte(val), 0o644)
}

// SendSignal 给 pid 发信号（supervisor 用；此处集中以便审计）
func SendSignal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// DetectHostapdVersion 探测 hostapd 版本号（整数形式 2.10 → 210），探测失败返回 0
func DetectHostapdVersion() int {
	r := Run(5*time.Second, "hostapd", "-v")
	// hostapd -v 输出到 stderr 且退出码非 0，属正常
	line := r.Stdout + r.Stderr
	var a, b int
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "hostapd v%d.%d", &a, &b); err == nil {
		return a*100 + b
	}
	return 0
}
