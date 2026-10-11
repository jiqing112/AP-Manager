// Package netrt 网络与路由模块：dnsmasq（DHCP/DNS）渲染与管理、nftables NAT/转发、
// 上游健康探测与切换事件。
package netrt

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"apmanager/internal/bus"
	"apmanager/internal/config"
	"apmanager/internal/priv"
	"apmanager/internal/supervisor"
)

// Paths 运行时路径
type Paths struct {
	RunDir   string // 生成配置放这里
	StateDir string // 租约文件放这里
}

// UpstreamStatus 上游状态
type UpstreamStatus struct {
	Kind    string `json:"kind"`    // eth-wan | wifi-relay | none
	Iface   string `json:"iface"`   // 上游接口
	GW      string `json:"gw"`      // 网关
	Healthy bool   `json:"healthy"` // 互联网可达
	Detail  string `json:"detail,omitempty"`
	Since   time.Time `json:"since"`
}

// Manager netrt 模块
type Manager struct {
	mu      sync.Mutex
	cfg     *config.Config
	sup     *supervisor.Supervisor
	evbus   *bus.Bus
	log     *slog.Logger
	paths   Paths

	upstream    UpstreamStatus
	failCount   int
	okCount     int
	dnsRunning  bool
	v6Iface     string // IPv6 DHCPv6 客户端当前运行的上行口

	// RefreshFn 上游变化时重刷网络面（main 注入，携带真实 AP 运行态，避免循环依赖）
	RefreshFn func()
	// RelayIfaceFn 判断某接口是否为中继 STA 口（main 注入 relay.IsConnected 判定）
	RelayIfaceFn func(ifaceName string) bool
	// BlockedIPsFn 黑名单设备的 IP 列表（devices 模块注入；渲染 drop 规则用）
	BlockedIPsFn func() []string
	// BlockedMACsFn 黑名单 MAC 列表（dnsmasq 拒发地址用）
	BlockedMACsFn func() []string
	// CountersEnabled 是否下发按设备计数集合
	CountersEnabled bool
}

// DeviceCounters 读取 nft 动态集合的按设备计数：IP -> [rxBytes(下发方向合计), txBytes]
// 注：dev_down 按 daddr 计 = 该 IP 的下行流量；dev_up 按 saddr 计 = 上行。
func (m *Manager) DeviceCounters() map[string][2]uint64 {
	out := map[string][2]uint64{}
	for _, set := range []struct{ name, key string }{
		{"dev_down", "rx"}, {"dev_up", "tx"},
	} {
		// 2s 级轮询：走 RunQuiet 不写审计日志，避免刷屏（排障时临时改回 Run）
		r := priv.RunQuiet(5*time.Second, "nft", "-j", "list", "set", "inet", "apmanager", set.name)
		if r.Err != nil {
			continue
		}
		parseNftSetJSON(r.Stdout, set.key, out)
	}
	return out
}

// parseNftSetJSON 解析 nft -j list set 的元素计数
func parseNftSetJSON(js, dir string, out map[string][2]uint64) {
	// 元素形如 {"elem": {"val": "192.168.50.123", "counter": {"packets": 10, "bytes": 2048}}}
	type elemWrap struct {
		Elem struct {
			Val     string `json:"val"`
			Counter *struct {
				Packets uint64 `json:"packets"`
				Bytes   uint64 `json:"bytes"`
			} `json:"counter"`
		} `json:"elem"`
	}
	var doc struct {
		Nftables []struct {
			Set struct {
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		return
	}
	for _, top := range doc.Nftables {
		for _, raw := range top.Set.Elem {
			var e elemWrap
			if err := json.Unmarshal(raw, &e); err != nil || e.Elem.Val == "" || e.Elem.Counter == nil {
				continue
			}
			cur := out[e.Elem.Val]
			if dir == "rx" {
				cur[0] = e.Elem.Counter.Bytes
			} else {
				cur[1] = e.Elem.Counter.Bytes
			}
			out[e.Elem.Val] = cur
		}
	}
}

func New(cfg *config.Config, sup *supervisor.Supervisor, evbus *bus.Bus, log *slog.Logger, paths Paths) *Manager {
	return &Manager{
		cfg: cfg, sup: sup, evbus: evbus, log: log, paths: paths,
		upstream: UpstreamStatus{Kind: "none", Since: time.Now()},
	}
}

// SetConfig 热更新配置引用
func (m *Manager) SetConfig(c *config.Config) {
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
}

// ActiveLANs 返回当前实际生效的 LAN 段：
// - AP 段：AP 运行中才算（bridge 形态或直挂形态都行）
// - 非 AP 段：桥存在即算（ethport 已落地桥），AP 关了有线 LAN 也照常发 DHCP
func (m *Manager) ActiveLANs(cfg *config.Config, apRunning bool) []config.LANSeg {
	var out []config.LANSeg
	for _, seg := range cfg.LAN {
		if seg.AP {
			if apRunning {
				out = append(out, seg)
			}
			continue
		}
		if seg.Bridge != "" {
			if li, err := priv.LinkByName(seg.Bridge); err == nil && len(li.Addrs) > 0 {
				out = append(out, seg)
			}
		}
	}
	return out
}

// Refresh 重刷网络面：ip_forward → nft 表 → dnsmasq（有 LAN 生效时确保运行）
// 由 AP 起停、上游切换、对账循环调用。
func (m *Manager) Refresh(apRunning bool) {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	if err := priv.EnableIPForward(); err != nil {
		m.log.Warn("打开 ip_forward 失败", "err", err)
	}

	// AP 模式：本机是二层接入点，NAT/DHCP/DNS 全部交给上级网络，
	// 撤掉 nft 表并停 dnsmasq（上级 DHCP 经桥直接服务客户端）
	if cfg.NetworkMode == "ap" {
		priv.NftDelete()
		if m.dnsRunning {
			m.sup.Stop("dnsmasq", 5*time.Second)
			m.mu.Lock()
			m.dnsRunning = false
			m.mu.Unlock()
		}
		return
	}

	lans := m.ActiveLANs(cfg, apRunning)

	// IPv6（默认关闭）：开启时对账，关闭时撤场
	if cfg.NetworkMode == "ap" {
		m.stopIPv6(nil) // AP 模式：v6 同样交给上级网络
	} else if cfg.IPv6.Enabled {
		priv.EnableIPv6Forward()
		m.reconcileIPv6(cfg, m.currentUplinkIface(), lans)
	} else {
		m.stopIPv6(lans)
	}
	// 纯 WAN 形态（AP 停、无 LAN）但配了防火墙规则时也要渲染 nft 表，
	// 否则端口转发/DMZ 在"只当路由器不当 AP"时不生效
	if len(lans) > 0 || apRunning || firewallActive(&cfg.Firewall) {
		// nft：区域隔离矩阵 + 黑名单 drop + 按设备计数 + masquerade
		blocked := []string{}
		if m.BlockedIPsFn != nil {
			blocked = m.BlockedIPsFn()
		}
		ruleset, err := RenderNftFull(lans, m.currentUplinkIface(), cfg.ZonePolicy, cfg.AP.Interface, blocked, m.CountersEnabled, &cfg.Firewall, &cfg.IPv6)
		if err != nil {
			m.log.Error("生成 nft 规则失败", "err", err)
		} else if err := priv.NftApply(ruleset); err != nil {
			m.log.Error("nft 下发失败", "err", err)
		}
		// dnsmasq：渲染 + 校验 + （重）启动（AP 无桥时直接监听无线口）
		apIface := ""
		if seg := cfg.LANForAP(); seg != nil && seg.Bridge == "" {
			apIface = cfg.AP.Interface
		}
		blockedMACs := []string{}
		if m.BlockedMACsFn != nil {
			blockedMACs = m.BlockedMACsFn()
		}
		conf := RenderDnsmasqWithIface(lans, m.dnsUpstreams(cfg), m.paths, apIface, blockedMACs, &cfg.IPv6)
		confPath := m.paths.RunDir + "/dnsmasq.conf"
		if err := priv.WriteFileAtomic(confPath, []byte(conf), 0o644); err != nil {
			m.log.Error("写 dnsmasq.conf 失败", "err", err)
			return
		}
		// dev 模式：dnsmasq 由 supervisor 直启（绕过 priv 打桩），会真占系统端口，跳过
		if priv.Mode == "dev" {
			return
		}
		if out, ok := priv.RunOK(5*time.Second, "dnsmasq", "--test", "--conf-file="+confPath); !ok {
			m.log.Error("dnsmasq 配置校验失败", "out", out)
			return
		}
		spec := &supervisor.Spec{
			Name: "dnsmasq",
			Argv: []string{"dnsmasq", "--keep-in-foreground", "--conf-file=" + confPath},
			LogRing: supervisor.NewRing(300),
		}
		if m.dnsRunning {
			// 已在跑：SIGHUP 不足以改 dhcp-range，直接重启换配置
			m.sup.Stop("dnsmasq", 5*time.Second)
		}
		if err := m.sup.Start(spec); err != nil {
			m.log.Error("启动 dnsmasq 失败", "err", err)
			return
		}
		m.mu.Lock()
		m.dnsRunning = true
		m.mu.Unlock()
	} else {
		// AP 停且无 LAN 且无防火墙规则：nft 只清自家表，dnsmasq 停止
		priv.NftDelete()
		if m.dnsRunning {
			m.sup.Stop("dnsmasq", 5*time.Second)
			m.mu.Lock()
			m.dnsRunning = false
			m.mu.Unlock()
		}
	}
}

// reconcileIPv6 IPv6 期望态对账：
//   - WAN 侧：上行口 accept_ra=2（SLAAC）+ dhclient -6（DHCPv6 地址），上行口变化则重启
//   - LAN 侧：每个生效段桥配 ULA 子网网关地址（ULA/48 内按段序递增），dnsmasq
//     渲染 RA/SLAAC 通告（renderDnsmasq 内由 v6 开关驱动）
//   - NAT66：renderNft 内由 v6 开关渲染 masquerade
func (m *Manager) reconcileIPv6(cfg *config.Config, uplink string, lans []config.LANSeg) {
	ula := cfg.IPv6.ULAPrefix
	// 1) LAN 段 ULA 网关地址（fdXX:YY:0:N::1/64，N 为段序）
	for i, seg := range lans {
		iface := seg.Bridge
		if iface == "" {
			iface = cfg.AP.Interface
		}
		if iface == "" || ula == "" {
			continue
		}
		ula6 := ulaSubnet(ula, i+1)
		// 已有该地址则跳过（幂等）
		li, err := priv.LinkByName(iface)
		if err == nil && hasV6(li.Addrs, ula6) {
			continue
		}
		if err := priv.AddrReplace(iface, ula6); err != nil {
			m.log.Warn("IPv6: 配置 LAN 段 ULA 地址失败", "iface", iface, "addr", ula6, "err", err)
		} else {
			m.log.Info("IPv6: LAN 段 ULA 网关已配置", "iface", iface, "addr", ula6)
		}
	}
	// 2) WAN 侧 DHCPv6 客户端（上行口变化或未运行则重启）
	if uplink == "" || priv.Mode == "dev" {
		return
	}
	m.mu.Lock()
	cur := m.v6Iface
	m.mu.Unlock()
	if cur == uplink {
		return
	}
	if cur != "" {
		m.sup.Stop("dhclient6", 4*time.Second)
	}
	spec := &supervisor.Spec{
		Name:    "dhclient6",
		Argv:    []string{"dhclient", "-6", "-v", "-pf", m.paths.RunDir + "/dhclient6.pid", "-lf", m.paths.RunDir + "/dhclient6.leases", uplink},
		LogRing: supervisor.NewRing(200),
	}
	if err := m.sup.Start(spec); err != nil {
		m.log.Warn("IPv6: 启动 dhclient -6 失败（上级可能只提供 SLAAC，不影响）", "err", err)
		return
	}
	m.mu.Lock()
	m.v6Iface = uplink
	m.mu.Unlock()
	m.log.Info("IPv6: DHCPv6 客户端已启动", "iface", uplink)
}

// stopIPv6 撤场：停 DHCPv6 客户端，清 LAN 段桥上的 ULA 地址（保留 fe80）
func (m *Manager) stopIPv6(lans []config.LANSeg) {
	m.mu.Lock()
	ran := m.v6Iface != ""
	m.v6Iface = ""
	m.mu.Unlock()
	if ran {
		m.sup.Stop("dhclient6", 4*time.Second)
		m.log.Info("IPv6: DHCPv6 客户端已停止")
	}
	for _, seg := range lans {
		iface := seg.Bridge
		if iface != "" {
			_ = priv.AddrFlushV6(iface)
		}
	}
}

// ulaSubnet 从 /48 ULA 前缀取第 subnetIndex 个 /64 的网关地址
func ulaSubnet(ula string, idx int) string {
	base := strings.TrimSuffix(strings.SplitN(ula, "/", 2)[0], "::") // fd52:3a91
	if strings.Count(base, ":") == 1 {
		return fmt.Sprintf("%s:0:%d::1/64", base, idx)
	}
	return fmt.Sprintf("%s:%d::1/64", base, idx)
}

// hasV6 地址列表里是否含目标 v6 地址（忽略前缀长度）
func hasV6(addrs []string, want string) bool {
	w := strings.SplitN(want, "/", 2)[0]
	for _, a := range addrs {
		if strings.SplitN(a, "/", 2)[0] == w && !strings.HasPrefix(a, "fe80") {
			return true
		}
	}
	return false
}

// firewallActive 是否存在任何启用中的防火墙规则（端口转发/开放端口/DMZ）
func firewallActive(fw *config.Firewall) bool {
	for _, r := range fw.PortForwards {
		if r.Enabled {
			return true
		}
	}
	for _, r := range fw.OpenPorts {
		if r.Enabled {
			return true
		}
	}
	return fw.DMZ.Enabled
}

// currentUplinkIface 当前默认路由出口（relay 接口优先，阶段二扩展）
func (m *Manager) currentUplinkIface() string {
	if m.upstream.Kind == "wifi-relay" && m.upstream.Iface != "" {
		return m.upstream.Iface
	}
	dr, err := priv.DefaultRoute4()
	if err != nil {
		return "" // 无默认路由：只渲染 forward，不出 masquerade
	}
	return dr.Iface
}

// StartLoops 启动上游探测循环（5s 间隔、3 连败判 down、3 连胜恢复）
func (m *Manager) StartLoops(stop <-chan struct{}) {
	go func() {
		m.probeOnce()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.probeOnce()
			}
		}
	}()
}

// probeOnce 一次上游探测：读默认路由 → 判类型 → 探互联网 → 状态机去抖
func (m *Manager) probeOnce() {
	dr, err := priv.DefaultRoute4()
	newKind := "none"
	iface, gw := "", ""
	if err == nil {
		iface, gw = dr.Iface, dr.GW.String()
		newKind = "eth-wan"
		// PPPoE 拨通后默认路由走 pppN
		if strings.HasPrefix(iface, "ppp") {
			newKind = "pppoe"
		}
		// 阶段二：若该接口是 relay STA 接口，类型为 wifi-relay
		if m.isRelayIface(iface) {
			newKind = "wifi-relay"
		}
	}
	online := checkInternet()

	m.mu.Lock()
	prev := m.upstream
	cfg := m.cfg
	m.mu.Unlock()

	// 失败计数去抖
	if online {
		m.okCount++
		m.failCount = 0
	} else {
		m.failCount++
		m.okCount = 0
	}
	threshold := cfg.Upstreams.Check.FailThreshold
	if threshold <= 0 {
		threshold = 3
	}
	healthy := prev.Healthy
	if !healthy && m.okCount >= threshold {
		healthy = true
	}
	if healthy && m.failCount >= threshold {
		healthy = false
	}

	changedKind := newKind != prev.Kind || iface != prev.Iface
	if changedKind || healthy != prev.Healthy || prev.Iface == "" {
		m.mu.Lock()
		m.upstream = UpstreamStatus{
			Kind: newKind, Iface: iface, GW: gw, Healthy: healthy,
			Since: time.Now(),
		}
		if !healthy {
			m.upstream.Detail = "互联网不可达"
		}
		up := m.upstream
		m.mu.Unlock()
		m.log.Info("上游状态变化", "kind", up.Kind, "iface", up.Iface, "healthy", up.Healthy)
		m.evbus.Publish("upstream", up)
		// 上游出口变化 → masquerade 目标变了，经钩子重刷网络面（真实 AP 态由 main 提供）
		if changedKind && m.RefreshFn != nil {
			m.RefreshFn()
		}
	} else {
		m.mu.Lock()
		m.upstream.Healthy = healthy
		m.mu.Unlock()
	}
}

func (m *Manager) isRelayIface(name string) bool {
	if m.RelayIfaceFn == nil {
		return false
	}
	return m.RelayIfaceFn(name)
}

// Upstream 当前上游快照
func (m *Manager) Upstream() UpstreamStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upstream
}

// checkInternet 轻量连通性检查：204 探测端点 + AliDNS TCP 双通道
func checkInternet() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	if resp, err := client.Head("http://connect.rom.miui.com/generate_204"); err == nil {
		resp.Body.Close()
		return true
	}
	conn, err := net.DialTimeout("tcp", "223.5.5.5:53", 1500*time.Millisecond)
	if err == nil {
		conn.Close()
		return true
	}
	return false
}

// DNSUpstreams 当前生效的 DNS 上游列表（API 展示用）
func (m *Manager) DNSUpstreams() []string {
	if priv.Mode == "dev" {
		return []string{"223.5.5.5", "119.29.29.29"} // dev 演示数据
	}
	return m.dnsUpstreams(m.cfg)
}

// dnsUpstreams 收集 DNS 上游：手动配置优先，否则取宿主机 resolv.conf 的非本机地址
func (m *Manager) dnsUpstreams(cfg *config.Config) []string {
	// 用户显式配置的 DNS 上游优先（WAN 页「DNS 服务器」卡修改的就是它）
	if len(cfg.DNS.Upstream) > 0 {
		return cfg.DNS.Upstream
	}
	var out []string
	for _, p := range cfg.Ports {
		if p.Role == "wan" && p.WAN != nil && len(p.WAN.DNS) > 0 {
			out = append(out, p.WAN.DNS...)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, l := range strings.Split(readResolvConf(), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "nameserver" {
			ip := net.ParseIP(f[1])
			if ip != nil && !ip.IsLoopback() {
				out = append(out, f[1])
			}
		}
	}
	if len(out) == 0 {
		out = []string{"223.5.5.5", "119.29.29.29"} // 兜底：AliDNS / DNSPod
	}
	return out
}

func readResolvConf() string {
	data, _ := os.ReadFile("/etc/resolv.conf")
	return string(data)
}

// IsDnsmasqRunning dnsmasq 是否在管
func (m *Manager) IsDnsmasqRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dnsRunning
}

// NftDump 当前 nft 表导出（诊断）
func (m *Manager) NftDump() string { return priv.NftExport() }

// LeasesPath 租约文件路径（devices 模块读）
func (m *Manager) LeasesPath() string { return m.paths.StateDir + "/dnsmasq.leases" }

// ReadLeases 解析 dnsmasq 租约文件：<expiry> <mac> <ip> <hostname> <clientid>
func ReadLeases(path string) []Lease {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []Lease
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		var exp int64
		fmt.Sscanf(f[0], "%d", &exp)
		l := Lease{Expiry: exp, MAC: f[1], IP: f[2]}
		if len(f) >= 4 && f[3] != "*" {
			l.Hostname = f[3]
		}
		out = append(out, l)
	}
	return out
}

// Lease 一条 DHCP 租约
type Lease struct {
	Expiry   int64  `json:"expiry"`
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
}
