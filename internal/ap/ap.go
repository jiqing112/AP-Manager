// Package ap 无线发射模块：hostapd 配置渲染、进程生命周期、控制接口（ctrl_interface）客户端。
package ap

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"apmanager/internal/bus"
	"apmanager/internal/config"
	"apmanager/internal/iface"
	"apmanager/internal/netctrl"
	"apmanager/internal/priv"
	"apmanager/internal/supervisor"
)

// APState AP 生命周期状态
type APState string

const (
	StateOff       APState = "off"
	StateStarting  APState = "starting"
	StateRunning   APState = "running"
	StateFailed    APState = "failed"
)

// ClientInfo 一个已连接的无线客户端（hostapd STA 信息）
type ClientInfo struct {
	MAC          string `json:"mac"`
	Signal       int    `json:"signal"`        // dBm
	ConnectedSec int    `json:"connectedSec"`
	RxBytes      uint64 `json:"rxBytes"`
	TxBytes      uint64 `json:"txBytes"`
}

// Status AP 状态视图（API/SSE 用）
type Status struct {
	State        APState        `json:"state"`
	Detail       string         `json:"detail,omitempty"`
	SSID         string         `json:"ssid"`
	Band         string         `json:"band"`
	Channel      int            `json:"channel"`
	Security     string         `json:"security"`
	Clients      []ClientInfo   `json:"clients"`
	HostapdState string         `json:"hostapdState,omitempty"` // ctrl STATUS 的 state 字段
}

// Paths 运行时路径集合（由 main 注入，dev 模式指到临时目录）
type Paths struct {
	RunDir string // /var/lib/apmanager/run —— hostapd.conf 与 ctrl 目录都放这
}

// Manager AP 模块
type Manager struct {
	mu     sync.Mutex
	cfg    *config.Config
	sup    *supervisor.Supervisor
	ifaces *iface.Manager
	evbus  *bus.Bus
	log    *slog.Logger
	paths  Paths

	state   APState
	detail  string
	ctrl    *netctrl.Client
	clients map[string]*ClientInfo
	stopPoll chan struct{}

	// OnNetworkChanged AP 起停后通知 netrt 重刷 dnsmasq/nft（main 注入，避免循环依赖）
	OnNetworkChanged func()
	// StopRelay 网卡不支持 AP+STA 并存时，启动 AP 前先断开中继（main 注入）
	StopRelay func() error
}

func New(cfg *config.Config, sup *supervisor.Supervisor, ifaces *iface.Manager, evbus *bus.Bus, log *slog.Logger, paths Paths) *Manager {
	return &Manager{
		cfg: cfg, sup: sup, ifaces: ifaces, evbus: evbus, log: log, paths: paths,
		state: StateOff, clients: map[string]*ClientInfo{},
	}
}

// SetConfig 热更新配置引用（config 变更后由 main 调用；运行中变更需 Restart 生效）
func (m *Manager) SetConfig(c *config.Config) {
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
}

// StateDescription 检查配置相对硬件能力的合法性，返回阻止启动的原因列表
func (m *Manager) Validate() []string {
	var errs []string
	m.mu.Lock()
	apc := m.cfg.AP
	m.mu.Unlock()
	if apc.Interface == "" {
		errs = append(errs, "未指定无线接口")
		return errs
	}
	wi := m.ifaces.WirelessCaps(apc.Interface)
	if wi == nil {
		errs = append(errs, fmt.Sprintf("接口 %s 不是无线网卡", apc.Interface))
		return errs
	}
	if !wi.CapAP {
		errs = append(errs, fmt.Sprintf("网卡 %s 不支持 AP 模式（驱动能力受限）", apc.Interface))
	}
	band := apc.Band
	if band == "" {
		band = "2g"
	}
	chs := wi.ValidChannels(band)
	if len(chs) == 0 {
		errs = append(errs, fmt.Sprintf("网卡不支持 %s 频段", band))
	} else if apc.Channel != 0 {
		ok := false
		for _, c := range chs {
			if c == apc.Channel {
				ok = true
				break
			}
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("信道 %d 在 %s 频段不可用（合法：%v）", apc.Channel, band, chs))
		}
	}
	if apc.Security == "wpa3" || apc.Security == "wpa2wpa3" {
		// SAE 需要 hostapd ≥ 2.9；版本探测在 priv，简化为 2.10 已确认
		if len(apc.Password) < 8 {
			errs = append(errs, "WPA3(SAE) 密码同样须 8–63 位")
		}
	}
	errs = append(errs, m.cfg.Validate()...)
	return errs
}

// Start 启动 AP：渲染配置 → 接口 up → 起 hostapd → ctrl 就绪 → 刷新 dnsmasq/nft
func (m *Manager) Start() error {
	if errs := m.Validate(); len(errs) > 0 {
		return fmt.Errorf("配置不合法: %s", strings.Join(errs, "; "))
	}
	// 单卡互斥：网卡不支持并存且中继占着接口 → 先断中继
	m.mu.Lock()
	ifaceName := m.cfg.AP.Interface
	m.mu.Unlock()
	if wi := m.ifaces.WirelessCaps(ifaceName); wi != nil && !wi.ComboSupported && m.StopRelay != nil {
		if err := m.StopRelay(); err != nil {
			return fmt.Errorf("停止中继失败（本机网卡不支持 AP 与中继并存）: %w", err)
		}
	}
	m.mu.Lock()
	if m.state == StateRunning || m.state == StateStarting {
		m.mu.Unlock()
		return nil
	}
	m.state = StateStarting
	apc := m.cfg.AP
	country := m.cfg.Country
	m.mu.Unlock()
	m.publish()

	// 1. 渲染并写 hostapd.conf（AP 段有桥时 hostapd 直接把无线口挂进桥）
	bridge := ""
	if seg := m.cfg.LANForAP(); seg != nil {
		bridge = seg.Bridge
	}
	conf, err := RenderHostapdConf(apc, country, m.paths.RunDir, bridge)
	if err != nil {
		m.fail("渲染 hostapd 配置失败: " + err.Error())
		return err
	}
	confPath := m.paths.RunDir + "/hostapd.conf"
	if err := writeFile(confPath, conf, 0o600); err != nil {
		m.fail("写 hostapd.conf 失败: " + err.Error())
		return err
	}
	// deny_mac 文件必须存在（内容由 devices 模块维护，这里只兜底建空文件）
	denyFile := m.paths.RunDir + "/deny_mac"
	if _, err := os.Stat(denyFile); os.IsNotExist(err) {
		_ = os.WriteFile(denyFile, nil, 0o600)
	}

	// 2. 接口 up（hostapd 自己会把接口切到 AP 类型）
	if err := linkUp(apc.Interface); err != nil {
		m.fail("拉起接口失败: " + err.Error())
		return err
	}
	// 桥模式：无线口地址必须清掉（地址只能挂桥上；直挂模式遗留地址会冲突）
	if bridge != "" {
		_ = priv.AddrFlush(apc.Interface)
	}

	// 3. supervisor 拉起 hostapd
	spec := &supervisor.Spec{
		Name: "hostapd",
		Argv: []string{"hostapd", "-t", confPath},
		LogRing: supervisor.NewRing(500),
	}
	ready := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			if c, err := netctrl.Dial(m.paths.RunDir + "/hostapd", apc.Interface); err == nil {
				if _, err := c.Request("PING"); err == nil {
					ready <- nil
					c.Close()
					return
				}
				c.Close()
			}
			time.Sleep(300 * time.Millisecond)
		}
		ready <- fmt.Errorf("hostapd 控制接口 12s 内未就绪")
	}()
	// ReadyFn 供 supervisor 判定 running
	spec.ReadyFn = func() error { return nil } // 就绪由下方统一等待
	if err := m.sup.Start(spec); err != nil {
		m.fail("启动 hostapd 失败: " + err.Error())
		return err
	}

	// 4. 等控制接口就绪
	if err := <-ready; err != nil {
		m.sup.Stop("hostapd", 5*time.Second)
		m.fail(err.Error())
		return err
	}

	// 5. 挂事件流 + 启动 STA 轮询
	ctrl, err := netctrl.Dial(m.paths.RunDir+"/hostapd", apc.Interface)
	if err != nil {
		m.fail("连接 hostapd 控制接口失败: " + err.Error())
		return err
	}
	m.mu.Lock()
	m.ctrl = ctrl
	m.state = StateRunning
	m.detail = ""
	m.stopPoll = make(chan struct{})
	stop := m.stopPoll
	m.mu.Unlock()
	go m.watchEvents(ctrl)
	go m.pollClients(stop, ctrl)
	m.publish()
	m.log.Info("AP 已启动", "ssid", apc.SSID, "if", apc.Interface, "band", apc.Band, "chan", apc.Channel)

	// 6. 通知 netrt 重刷 dnsmasq/nft（新 LAN 生效）
	if m.OnNetworkChanged != nil {
		m.OnNetworkChanged()
	}
	return nil
}

// Stop 停止 AP
func (m *Manager) Stop() error {
	m.mu.Lock()
	if m.state == StateOff {
		m.mu.Unlock()
		return nil
	}
	ctrl := m.ctrl
	stop := m.stopPoll
	m.ctrl = nil
	m.stopPoll = nil
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if ctrl != nil {
		ctrl.Close()
	}
	m.sup.Stop("hostapd", 10*time.Second)
	m.mu.Lock()
	m.state = StateOff
	m.detail = ""
	m.clients = map[string]*ClientInfo{}
	m.mu.Unlock()
	m.publish()
	if m.OnNetworkChanged != nil {
		m.OnNetworkChanged()
	}
	return nil
}

// Restart 重启（新配置生效）
func (m *Manager) Restart() error {
	_ = m.Stop()
	return m.Start()
}

// Status 当前状态快照
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		State: m.state, Detail: m.detail,
		SSID: m.cfg.AP.SSID, Band: m.cfg.AP.Band, Security: m.cfg.AP.Security,
		Channel: m.cfg.AP.Channel, Clients: []ClientInfo{},
	}
	for _, c := range m.clients {
		st.Clients = append(st.Clients, *c)
	}
	return st
}

func (m *Manager) fail(msg string) {
	m.mu.Lock()
	m.state = StateFailed
	m.detail = msg
	m.mu.Unlock()
	m.log.Error("AP 启动失败", "err", msg)
	m.publish()
}

func (m *Manager) publish() {
	m.evbus.Publish("status", map[string]any{"ap": m.Status()})
}

// watchEvents 读 hostapd 事件（ATTACH 模式）：AP-STA-CONNECTED / AP-STA-DISCONNECTED
func (m *Manager) watchEvents(c *netctrl.Client) {
	for ev := range c.Events() {
		switch {
		case strings.HasPrefix(ev, "AP-STA-CONNECTED "):
			mac := strings.TrimSpace(strings.TrimPrefix(ev, "AP-STA-CONNECTED "))
			m.log.Info("无线客户端接入", "mac", mac)
			m.evbus.Publish("status", map[string]any{"device_event": map[string]string{"type": "wifi_join", "mac": mac}})
		case strings.HasPrefix(ev, "AP-STA-DISCONNECTED "):
			mac := strings.TrimSpace(strings.TrimPrefix(ev, "AP-STA-DISCONNECTED "))
			m.log.Info("无线客户端断开", "mac", mac)
			m.evbus.Publish("status", map[string]any{"device_event": map[string]string{"type": "wifi_leave", "mac": mac}})
		}
	}
}

// pollClients 每 2s 拉一次 STA 列表与详情，更新 clients。
// hostapd 2.10 没有 STA-LIST：遍历用 STA-FIRST → STA-NEXT <mac> 协议。
func (m *Manager) pollClients(stop <-chan struct{}, c *netctrl.Client) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		m.updateClients(c, listStations(c))
	}
}

// macLineRe 识别 hostapd 响应中的裸 MAC 行（如 6a:23:29:a5:4b:5f）
var macLineRe = regexp.MustCompile(`^(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)

// listStations 通过 ALL_STA 枚举关联的客户端 MAC。
// 注意：Debian 的 hostapd 2.10 构建没有 STA-FIRST/STA-NEXT 命令（实测返回
// Unknown command）；ALL_STA 一次性返回全部 STA，每个条目首行为裸 MAC，
// 其后是 key=value 明细行。MAC 统一小写，与租约/邻居表键一致。
func listStations(c *netctrl.Client) []string {
	resp, err := c.Request("ALL_STA")
	if err != nil || resp == "" {
		return nil
	}
	var macs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "=") { // key=value 明细行
			continue
		}
		if !macLineRe.MatchString(line) {
			continue
		}
		mac := strings.ToLower(line)
		if seen[mac] {
			continue
		}
		seen[mac] = true
		macs = append(macs, mac)
		if len(macs) >= 128 { // 防御上限
			break
		}
	}
	return macs
}

// updateClients 解析 STA 详情并发布
func (m *Manager) updateClients(c *netctrl.Client, macs []string) {
	m.mu.Lock()
	newClients := map[string]*ClientInfo{}
	for _, mac := range macs {
		ci := m.clients[mac]
		if ci == nil {
			ci = &ClientInfo{MAC: mac}
		}
		newClients[mac] = ci
	}
	m.clients = newClients
	ctrl := m.ctrl
	apc := m.cfg.AP
	m.mu.Unlock()
	_ = ctrl
	changed := false
	for _, mac := range macs {
		resp, err := c.Request("STA " + mac)
		if err != nil {
			continue
		}
		m.mu.Lock()
		ci := m.clients[mac]
		m.mu.Unlock()
		if ci == nil {
			continue
		}
		for _, line := range strings.Split(resp, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch k {
			case "signal":
				fmt.Sscanf(v, "%d", &ci.Signal)
			case "connected_time":
				fmt.Sscanf(v, "%d", &ci.ConnectedSec)
			case "rx_bytes":
				fmt.Sscanf(v, "%d", &ci.RxBytes)
			case "tx_bytes":
				fmt.Sscanf(v, "%d", &ci.TxBytes)
			}
			changed = true
		}
	}
	_ = apc
	if changed {
		m.publish()
	}
}

// Kick 踢掉一个无线客户端（hostapd DEAUTHENTICATE）
func (m *Manager) Kick(mac string) error {
	m.mu.Lock()
	ctrl := m.ctrl
	m.mu.Unlock()
	if ctrl == nil {
		return fmt.Errorf("AP 未运行")
	}
	resp, err := ctrl.Request("DEAUTHENTICATE " + mac)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("hostapd 拒绝: %s", resp)
	}
	m.log.Info("已踢客户端", "mac", mac)
	return nil
}

// DenyMAC 把 MAC 加入 hostapd 拒绝关联列表（拉黑；AP 未运行时忽略，下次启动由 conf 生效）
func (m *Manager) DenyMAC(mac string) {
	m.mu.Lock()
	ctrl := m.ctrl
	m.mu.Unlock()
	if ctrl == nil {
		return
	}
	resp, err := ctrl.Request("DENY_MAC add " + mac)
	if err != nil {
		m.log.Warn("DENY_MAC 失败", "mac", mac, "err", err)
		return
	}
	m.log.Info("已加入关联黑名单", "mac", mac, "resp", strings.TrimSpace(resp))
}

// ---- 文件/接口小助手（特权动作走 priv）----

func writeFile(path, content string, perm os.FileMode) error {
	return priv.WriteFileAtomic(path, []byte(content), perm)
}

func linkUp(name string) error { return priv.SetUp(name) }
