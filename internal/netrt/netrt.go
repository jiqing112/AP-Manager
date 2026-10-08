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

	lans := m.ActiveLANs(cfg, apRunning)
	if len(lans) > 0 || apRunning {
		// nft：区域隔离矩阵 + 黑名单 drop + 按设备计数 + masquerade
		blocked := []string{}
		if m.BlockedIPsFn != nil {
			blocked = m.BlockedIPsFn()
		}
		ruleset, err := RenderNftFull(lans, m.currentUplinkIface(), cfg.ZonePolicy, cfg.AP.Interface, blocked, m.CountersEnabled)
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
		conf := RenderDnsmasqWithIface(lans, m.dnsUpstreams(cfg), m.paths, apIface, blockedMACs)
		confPath := m.paths.RunDir + "/dnsmasq.conf"
		if err := priv.WriteFileAtomic(confPath, []byte(conf), 0o644); err != nil {
			m.log.Error("写 dnsmasq.conf 失败", "err", err)
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
		// AP 停：nft 只清自家表，dnsmasq 停止
		priv.NftDelete()
		if m.dnsRunning {
			m.sup.Stop("dnsmasq", 5*time.Second)
			m.mu.Lock()
			m.dnsRunning = false
			m.mu.Unlock()
		}
	}
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

// dnsUpstreams 收集 DNS 上游：手动配置优先，否则取宿主机 resolv.conf 的非本机地址
func (m *Manager) dnsUpstreams(cfg *config.Config) []string {
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
