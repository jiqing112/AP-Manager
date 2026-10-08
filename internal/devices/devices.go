// Package devices 设备管理模块：多源按 MAC 聚合设备表。
// 阶段一来源：dnsmasq 租约（IP/主机名）+ hostapd STA（信号/在线时长/流量）+ 邻居表（兜底）。
// 阶段四扩展：nft 按设备计数、限速、踢/拉黑。
package devices

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"apmanager/internal/ap"
	"apmanager/internal/bus"
	"apmanager/internal/config"
	"apmanager/internal/netrt"
	"apmanager/internal/priv"
	"apmanager/internal/system"
)

// Device 一台终端设备
type Device struct {
	MAC          string `json:"mac"`
	IP           string `json:"ip,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	Alias        string `json:"alias,omitempty"`   // 用户起的备注名
	Source       string `json:"source"`            // wifi | wired | unknown
	Signal       int    `json:"signal,omitempty"`  // dBm（无线）
	ConnectedSec int    `json:"connectedSec,omitempty"`
	RxBytes      uint64 `json:"rxBytes"`
	TxBytes      uint64 `json:"txBytes"`
	Online       bool   `json:"online"`
	Blocked      bool   `json:"blocked"`
	RateDown     int    `json:"rateDown,omitempty"` // Mbps，0=不限
	RateUp       int    `json:"rateUp,omitempty"`
}

// Manager 设备模块
type Manager struct {
	mu       sync.Mutex
	sys      *system.Manager
	apm      *ap.Manager
	evbus    *bus.Bus
	log      *slog.Logger
	leasesOf func() []netrt.Lease
	lanIfaces func() map[string]bool // LAN 侧接口集合（邻居表过滤，防把上游设备算进来）
	devices  map[string]*Device
	counters func() map[string][2]uint64 // netrt 按设备计数注入
	applyRates func()                     // 重建 tc 树（main 注入）
	refreshNet func()                     // 重刷 nft/dnsmasq（黑名单后）
	denyFile  string                      // hostapd 关联黑名单文件（拉黑时重写）
	cfg      *config.Config
}

// SetDenyFile 注入 deny_mac 文件路径
func (m *Manager) SetDenyFile(path string) { m.denyFile = path }

// New 构造；leasesOf/lanIfaces/counters 由 main 注入
func New(sys *system.Manager, apm *ap.Manager, cfg *config.Config, evbus *bus.Bus, log *slog.Logger,
	leasesOf func() []netrt.Lease, lanIfaces func() map[string]bool) *Manager {
	return &Manager{
		sys: sys, apm: apm, cfg: cfg, evbus: evbus, log: log,
		leasesOf: leasesOf, lanIfaces: lanIfaces,
		devices: map[string]*Device{},
	}
}

// SetHooks 注入计数与限速/网络刷新钩子（main 装配时）
func (m *Manager) SetHooks(counters func() map[string][2]uint64, applyRates func(), refreshNet func()) {
	m.counters = counters
	m.applyRates = applyRates
	m.refreshNet = refreshNet
}

// Start 启动 5s 聚合循环（变化或 5s 全量推送，SSE devices topic）
func (m *Manager) Start(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.refresh()
				m.evbus.Publish("devices", m.List())
			}
		}
	}()
}

// refresh 多源聚合
func (m *Manager) refresh() {
	now := time.Now()
	byMAC := map[string]*Device{}

	// 1. 租约（有线+无线共同的 IP/主机名来源）
	for _, l := range m.leasesOf() {
		d := byMAC[l.MAC]
		if d == nil {
			d = &Device{MAC: l.MAC}
			byMAC[l.MAC] = d
		}
		d.IP = l.IP
		if l.Hostname != "" {
			d.Hostname = l.Hostname
		}
		d.Online = l.Expiry > now.Unix() || l.Expiry == 0
	}

	// 2. hostapd 在线 STA（无线来源 + 实时信号）
	st := m.apm.Status()
	wifiMACs := map[string]bool{}
	for _, c := range st.Clients {
		wifiMACs[c.MAC] = true
		d := byMAC[c.MAC]
		if d == nil {
			d = &Device{MAC: c.MAC}
			byMAC[c.MAC] = d
		}
		d.Source = "wifi"
		d.Signal = c.Signal
		d.ConnectedSec = c.ConnectedSec
		d.RxBytes = c.RxBytes
		d.TxBytes = c.TxBytes
		d.Online = true
	}

	// 3. 邻居表兜底（有线设备：有 IP 无租约的场景）——只认 LAN 侧接口，
	//    上行口(enx)的邻居是上游局域网设备，不属于本路由的客户端
		if ne, err := priv.NeighDump(); err == nil {
		lanSet := m.lanIfaces()
		for _, n := range ne {
			if n.MAC == "" || wifiMACs[n.MAC] {
				continue
			}
			if lanSet != nil && !lanSet[n.Iface] {
				continue
			}
			if isMulticast(n.IP) || isMulticastMAC(n.MAC) {
				continue // mDNS/IGMP 组播邻居不是设备
			}
			d := byMAC[n.MAC]
			if d == nil && n.IP != "" {
				d = &Device{MAC: n.MAC, IP: n.IP, Source: "wired", Online: n.State == "reachable"}
				byMAC[n.MAC] = d
			} else if d != nil && d.Source == "" {
				d.Source = "wired"
			}
		}
	}

	// 4. 叠加持久化状态：别名 / 拉黑 / 限速
	snap := m.sys.Snapshot()
	blockedSet := map[string]bool{}
	for _, b := range snap.BlockedMACs {
		blockedSet[b] = true
	}
	for mac, d := range byMAC {
		if alias, ok := snap.Aliases[mac]; ok {
			d.Alias = alias
		}
		d.Blocked = blockedSet[mac]
		if r, ok := snap.Rates[mac]; ok {
			d.RateDown = r[0]
			d.RateUp = r[1]
		}
		if d.Source == "" {
			d.Source = "unknown"
		}
	}

	// 5. 按设备流量计数（nft 动态集合，从表下发起累计；与 hostapd 的 STA
	//    计数取较大值——hostapd 重启会清零，nft 元素也会随表重建清零，互为补充）
	if m.counters != nil {
		for ip, bytes := range m.counters() {
			for _, d := range byMAC {
				if d.IP != "" && d.IP == ip {
					if bytes[0] > d.RxBytes {
						d.RxBytes = bytes[0]
					}
					if bytes[1] > d.TxBytes {
						d.TxBytes = bytes[1]
					}
				}
			}
		}
	}

	m.mu.Lock()
	m.devices = byMAC
	m.mu.Unlock()
}

// List 设备列表（按在线优先、MAC 排序）
func (m *Manager) List() []Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].MAC < out[j].MAC
	})
	return out
}

// SetAlias 设置别名（持久化到 state.json）
func (m *Manager) SetAlias(mac, alias string) error {
	err := m.sys.Mutate(func(s *system.State) {
		if alias == "" {
			delete(s.Aliases, mac)
		} else {
			if s.Aliases == nil {
				s.Aliases = map[string]string{}
			}
			s.Aliases[mac] = alias
		}
	})
	if err == nil {
		m.refresh()
		m.evbus.Publish("devices", m.List())
	}
	return err
}

// SetRate 设置限速（Mbps，0=不限）并立即重建 tc 树
func (m *Manager) SetRate(mac string, down, up int) error {
	if down < 0 || up < 0 || down > 1000 || up > 1000 {
		return fmt.Errorf("限速值须在 0–1000 Mbps")
	}
	err := m.sys.Mutate(func(s *system.State) {
		if s.Rates == nil {
			s.Rates = map[string][2]int{}
		}
		if down == 0 && up == 0 {
			delete(s.Rates, mac)
		} else {
			s.Rates[mac] = [2]int{down, up}
		}
	})
	if err == nil {
		m.ApplyRates()
		m.refresh()
		m.evbus.Publish("devices", m.List())
	}
	return err
}

// ApplyRates 把 state 里的限速表落到 tc（需要设备当前 IP）
func (m *Manager) ApplyRates() {
	if m.applyRates == nil {
		return
	}
	m.applyRates()
}

// RateLimits 当前限速表（IP 化，给 tc 用）
func (m *Manager) RateLimits() []priv.RateLimit {
	snap := m.sys.Snapshot()
	var out []priv.RateLimit
	for _, d := range m.List() {
		if r, ok := snap.Rates[d.MAC]; ok && d.IP != "" {
			out = append(out, priv.RateLimit{IP: d.IP, DownMbps: r[0], UpMbps: r[1]})
		}
	}
	return out
}

// Kick 踢下线：无线走 hostapd DEAUTH；有线无法去认证 → 等价转为拉黑并说明
func (m *Manager) Kick(mac string) (string, error) {
	d, ok := m.Find(mac)
	if !ok {
		return "", fmt.Errorf("设备不存在")
	}
	if d.Source == "wifi" {
		if err := m.apm.Kick(mac); err != nil {
			return "", err
		}
		return "", nil
	}
	// 有线：阻断其流量（明确告知用户这是等价实现）
	if err := m.block(mac); err != nil {
		return "", err
	}
	return "有线设备无法无线去认证，已改为防火墙阻断（等效踢下线）；可在设备列表解除", nil
}

// Block 拉黑：hostapd 拒绝关联 + dnsmasq 拒发地址 + nft drop（有线同样生效）
func (m *Manager) Block(mac string) error {
	return m.block(mac)
}

func (m *Manager) block(mac string) error {
	err := m.sys.Mutate(func(s *system.State) {
		for _, b := range s.BlockedMACs {
			if b == mac {
				return
			}
		}
		s.BlockedMACs = append(s.BlockedMACs, mac)
	})
	if err != nil {
		return err
	}
	m.rewriteDenyFile()
	// hostapd 侧即时生效（deny 列表）；重连的会被拒
	m.apm.DenyMAC(mac)
	if m.refreshNet != nil {
		m.refreshNet() // nft drop + dnsmasq dhcp-host ignore
	}
	m.refresh()
	m.evbus.Publish("devices", m.List())
	return nil
}

// rewriteDenyFile 把 state 的黑名单全量写进 hostapd deny_mac 文件（一行一个 MAC）
func (m *Manager) rewriteDenyFile() {
	if m.denyFile == "" {
		return
	}
	snap := m.sys.Snapshot()
	content := ""
	for _, b := range snap.BlockedMACs {
		content += b + "\n"
	}
	if err := priv.WriteFileAtomic(m.denyFile, []byte(content), 0o600); err != nil {
		m.log.Warn("写 deny_mac 失败", "err", err)
	}
}

// Unblock 解除拉黑
func (m *Manager) Unblock(mac string) error {
	err := m.sys.Mutate(func(s *system.State) {
		out := s.BlockedMACs[:0]
		for _, b := range s.BlockedMACs {
			if b != mac {
				out = append(out, b)
			}
		}
		s.BlockedMACs = out
	})
	if err != nil {
		return err
	}
	m.rewriteDenyFile()
	if m.refreshNet != nil {
		m.refreshNet()
	}
	m.refresh()
	m.evbus.Publish("devices", m.List())
	return nil
}

// BlockedIPs 黑名单设备的当前 IP（netrt 渲染 drop 用）
func (m *Manager) BlockedIPs() []string {
	snap := m.sys.Snapshot()
	blocked := map[string]bool{}
	for _, b := range snap.BlockedMACs {
		blocked[b] = true
	}
	var ips []string
	for _, d := range m.List() {
		if blocked[d.MAC] && d.IP != "" {
			ips = append(ips, d.IP)
		}
	}
	return ips
}

// Find 按 MAC 找设备
func (m *Manager) Find(mac string) (Device, bool) {
	for _, d := range m.List() {
		if d.MAC == mac {
			return d, true
		}
	}
	return Device{}, false
}

// CountOnline 在线设备数（概览页）
func (m *Manager) CountOnline() int {
	n := 0
	for _, d := range m.List() {
		if d.Online {
			n++
		}
	}
	return n
}

// isMulticast IPv4 组播段 224.0.0.0/4
func isMulticast(ip string) bool {
	return strings.HasPrefix(ip, "224.") || strings.HasPrefix(ip, "239.") ||
		strings.HasPrefix(ip, "ff") // IPv6 组播 ff00::/8
}

// isMulticastMAC 组播 MAC（IPv4 映射 01:00:5e，IPv6 映射 33:33）
func isMulticastMAC(mac string) bool {
	return strings.HasPrefix(mac, "01:00:5e") || strings.HasPrefix(mac, "33:33")
}
