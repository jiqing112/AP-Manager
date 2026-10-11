// Package relay 无线中继模块：扫描（iw）、STA 连接（wpa_supplicant 独立实例）、
// 自动重连（指数退避 + 失败重扫切换次优先级）、中继态 NAT（经 netrt 钩子）。
package relay

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"apmanager/internal/ap"
	"apmanager/internal/bus"
	"apmanager/internal/config"
	"apmanager/internal/iface"
	"apmanager/internal/netctrl"
	"apmanager/internal/priv"
	"apmanager/internal/supervisor"
)

// RelayState 中继状态机
type RelayState string

const (
	StateOff        RelayState = "off"
	StateScanning   RelayState = "scanning"
	StateConnecting RelayState = "connecting"
	StateConnected  RelayState = "connected"
	StateBackoff    RelayState = "backoff"
	StateFailed     RelayState = "failed"
)

// BSS 一个扫描到的 WiFi 热点
type BSS struct {
	BSSID    string `json:"bssid"`
	SSID     string `json:"ssid"`
	Signal   int    `json:"signal"`   // dBm（负值）
	Freq     int    `json:"freq"`     // MHz
	Band     string `json:"band"`     // 2g | 5g
	Channel  int    `json:"channel"`  // 信道号
	Security string `json:"security"` // open | wep | wpa2 | wpa3 | wpa2wpa3
}

// Status 中继状态视图
type Status struct {
	State       RelayState `json:"state"`
	Detail      string     `json:"detail,omitempty"`
	SSID        string     `json:"ssid,omitempty"`
	BSSID       string     `json:"bssid,omitempty"`
	Signal      int        `json:"signal"`
	IP          string     `json:"ip,omitempty"`
	ScannedAt   int64      `json:"scannedAt,omitempty"` // 上次扫描完成时间
	SavedCount  int        `json:"savedCount"`
	LastEvent   string     `json:"lastEvent,omitempty"`
}

// Manager 中继模块
type Manager struct {
	mu     sync.Mutex
	cfg    *config.Config
	sup    *supervisor.Supervisor
	ifaces *iface.Manager
	evbus  *bus.Bus
	log    *slog.Logger
	apm    *ap.Manager

	runDir   string
	cfgPath  string
	state    RelayState
	detail   string
	lastSSID string
	results  []BSS
	scannedAt time.Time
	ctrl     *netctrl.Client
	stopWatch chan struct{}
	fails    int      // 连续断开计数
	reconnectAt time.Time
	lastEvent string

	// StopAP 连接前停止 AP（单卡共存能力不足时由 main 决定策略并注入）
	StopAP func() error
	// OnNetworkChanged 连接/断开后通知 netrt 刷新（STA 口成为/失去 WAN）
	OnNetworkChanged func()
}

// New 构造；cfgPath 用于连接成功后回写已存网络
func New(cfg *config.Config, cfgPath string, sup *supervisor.Supervisor, ifaces *iface.Manager,
	apm *ap.Manager, evbus *bus.Bus, log *slog.Logger, runDir string) *Manager {
	return &Manager{
		cfg: cfg, cfgPath: cfgPath, sup: sup, ifaces: ifaces, apm: apm, evbus: evbus, log: log,
		runDir: runDir, state: StateOff,
	}
}

// SetConfig 热更新配置引用
func (m *Manager) SetConfig(c *config.Config) {
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
}

// Status 状态快照
func (m *Manager) Status() Status {
	m.mu.Lock()
	ip := ""
	if m.state == StateConnected || m.state == StateBackoff {
		ip = ifaceAddr(m.cfg.Relay.Interface)
	}
	sig := 0
	ctrl := m.ctrl
	saved := len(m.cfg.Relay.Networks)
	st, detail, ssid, scannedAt, lastEv := m.state, m.detail, m.lastSSID, m.scannedAt, m.lastEvent
	m.mu.Unlock()
	if ctrl != nil {
		if resp, err := ctrl.Request("STATUS"); err == nil {
			sig = parseSignal(resp)
		}
	}
	return Status{
		State: st, Detail: detail, SSID: ssid,
		ScannedAt: scannedAt.UnixMilli(), SavedCount: saved,
		LastEvent: lastEv, IP: ip, Signal: sig,
	}
}

func ifaceAddr(name string) string {
	li, err := priv.LinkByName(name)
	if err != nil {
		return ""
	}
	for _, a := range li.Addrs {
		if !strings.Contains(a, ":") && !strings.HasPrefix(a, "169.254") {
			return a
		}
	}
	return ""
}

// ---------- 扫描 ----------

// Scan 触发一次扫描并返回结果。
// AP 正在运行且有客户端时拒绝（离信道扫会打断在线设备，PLAN §7-9）。
func (m *Manager) Scan(force bool) error {
	m.mu.Lock()
	ifc := m.cfg.Relay.Interface
	aps := m.apm.Status()
	m.mu.Unlock()

	if aps.State == ap.StateRunning && len(aps.Clients) > 0 && !force {
		return fmt.Errorf("AP 正在服务 %d 台客户端，扫描会短暂打断它们；请先停止 AP 或清空客户端", len(aps.Clients))
	}
	if err := priv.SetUp(ifc); err != nil {
		return fmt.Errorf("拉起接口失败: %w", err)
	}
	m.mu.Lock()
	m.state = StateScanning
	m.mu.Unlock()
	m.publish()

	// trigger（忽略 EBUSY：可能已有扫描在跑，直接等 dump）
	priv.Run(5*time.Second, "iw", "dev", ifc, "scan", "trigger")
	results := make([]BSS, 0, 40)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		dump := priv.Run(8*time.Second, "iw", "dev", ifc, "scan", "dump").Stdout
		results = parseScanDump(dump)
		if len(results) > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	// 过滤隐藏 SSID、按信号排序
	out := results[:0]
	for _, b := range results {
		if b.SSID != "" {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Signal > out[j].Signal })

	m.mu.Lock()
	m.results = out
	m.scannedAt = time.Now()
	if m.state == StateScanning {
		m.state = StateOff
	}
	m.mu.Unlock()
	m.publish()
	m.log.Info("扫描完成", "count", len(out))
	return nil
}

// Results 最近一次扫描结果
func (m *Manager) Results() []BSS {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]BSS, len(m.results))
	copy(out, m.results)
	return out
}

// parseScanDump 解析 `iw dev X scan dump` 的 BSS 块。
// 每块以 "BSS <mac>(...)" 开始，含 freq / signal / SSID / capability(Privacy) / RSN/WPA 认证套件。
func parseScanDump(out string) []BSS {
	var list []BSS
	var cur *BSS
	sec := secAcc{}
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(t, "BSS ") {
			if cur != nil {
				cur.Security = sec.classify()
				list = append(list, *cur)
			}
			mac := strings.Fields(strings.TrimPrefix(t, "BSS "))[0]
			cur = &BSS{BSSID: strings.SplitN(strings.TrimSuffix(mac, ")"), "(", 2)[0]}
			sec = secAcc{}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(t, "freq:"):
			// freq 行带小数："freq: 2412.0"
			if fv, err := strconv.ParseFloat(strings.Fields(strings.TrimPrefix(t, "freq:"))[0], 64); err == nil {
				cur.Freq = int(fv)
			}
			if cur.Freq < 3000 {
				cur.Band = "2g"
			} else {
				cur.Band = "5g"
			}
			cur.Channel = freqToChan(cur.Freq)
		case strings.HasPrefix(t, "signal:"):
			// "signal: -47.00 dBm"
			if f := strings.Fields(strings.TrimPrefix(t, "signal:")); len(f) > 0 {
				if fv, err := strconv.ParseFloat(f[0], 64); err == nil {
					cur.Signal = int(fv)
				}
			}
		case strings.HasPrefix(t, "SSID: "):
			cur.SSID = strings.TrimPrefix(t, "SSID: ")
		case strings.HasPrefix(t, "capability:"):
			if strings.Contains(t, "Privacy") {
				sec.privacy = true
			}
		case strings.HasPrefix(t, "RSN:"):
			sec.inRSN, sec.rsn = true, true
		case strings.HasPrefix(t, "WPA:"):
			sec.inRSN, sec.wpa1 = true, true
		case sec.inRSN && strings.HasPrefix(t, "Authentication suites:"):
			if strings.Contains(t, "802.1x") {
				sec.dot1x = true
			}
			if strings.Contains(t, "SAE") && !strings.Contains(t, "PSK") {
				sec.saeOnly = true
			}
			sec.inRSN = false
		}
	}
	if cur != nil {
		cur.Security = sec.classify()
		list = append(list, *cur)
	}
	return list
}

// secAcc 一个 BSS 块内的安全信息累积器
type secAcc struct {
	privacy, rsn, wpa1, dot1x, saeOnly bool
	inRSN                              bool
}

func (s secAcc) classify() string {
	switch {
	case !s.privacy:
		return "open"
	case s.dot1x:
		return "enterprise" // 企业级 802.1x：暂不支持连接，仅展示
	case s.rsn && s.saeOnly:
		return "wpa3"
	case s.rsn:
		return "wpa2"
	case s.wpa1:
		return "wpa"
	default:
		return "wep"
	}
}

func freqToChan(mhz int) int {
	if mhz == 2484 {
		return 14
	}
	if mhz < 2484 {
		return (mhz - 2407) / 5
	}
	if mhz >= 5160 && mhz <= 5880 {
		return (mhz - 5000) / 5
	}
	return 0
}

// ---------- 连接管理 ----------

// Connect 连接上游：可选保存凭证 → 渲染 wpa_supplicant.conf → 拉起进程 → 等关联 → DHCP
func (m *Manager) Connect(ssid, password string, save bool) error {
	if ssid == "" {
		return fmt.Errorf("SSID 不能为空")
	}
	// 单卡共存能力：驱动未声明组合 → 连接前停止 AP（互斥模式，UI 已说明）
	wi := m.ifaces.WirelessCaps(m.cfg.Relay.Interface)
	if m.apm.Status().State == ap.StateRunning && wi != nil && !wi.ComboSupported && m.StopAP != nil {
		m.log.Info("网卡不支持多接口并存，连接上游前停止 AP")
		if err := m.StopAP(); err != nil {
			return fmt.Errorf("停止 AP 失败: %w", err)
		}
	}

	m.mu.Lock()
	m.state = StateConnecting
	m.detail = ""
	m.lastSSID = ssid
	cfg := m.cfg
	ifaceName := cfg.Relay.Interface
	m.mu.Unlock()
	m.publish()

	// 保存凭证（优先级 = 当前最大 +1）
	if save {
		m.mu.Lock()
		exists := false
		maxP := 0
		for _, n := range m.cfg.Relay.Networks {
			if n.SSID == ssid {
				n.Password = password
				exists = true
			}
			if n.Priority > maxP {
				maxP = n.Priority
			}
		}
		if !exists {
			m.cfg.Relay.Networks = append(m.cfg.Relay.Networks, config.UpstreamNet{
				SSID: ssid, Password: password, Priority: maxP + 1,
			})
		}
		m.cfg.Relay.Current = ssid
		cfgRef := m.cfg
		path := m.cfgPath
		m.mu.Unlock()
		if err := cfgRef.Save(path); err != nil {
			m.log.Warn("保存上游凭证失败", "err", err)
		}
	} else {
		m.mu.Lock()
		m.cfg.Relay.Current = ssid
		m.mu.Unlock()
	}

	// 渲染 wpa_supplicant.conf（目标网络放最前，已存的按优先级跟后）
	conf := RenderWpaConf(m.cfg.Relay, ssid, password, m.runDir)
	confPath := m.runDir + "/wpa_supplicant.conf"
	if err := priv.WriteFileAtomic(confPath, []byte(conf), 0o600); err != nil {
		m.fail("写 wpa_supplicant.conf 失败: " + err.Error())
		return err
	}
	if err := priv.SetUp(ifaceName); err != nil {
		m.fail("接口拉起失败: " + err.Error())
		return err
	}
	// 接口归位 managed 类型：hostapd 停止后驱动可能仍留在 AP 类型，
	// wpa_supplicant 会报 "Could not configure driver mode"
	if err := ensureManagedType(ifaceName); err != nil {
		m.fail("接口类型切换失败: " + err.Error())
		return err
	}

	// 清旧实例 → 起新
	m.sup.Stop("wpa_supplicant", 5*time.Second)
	ctrlDir := m.runDir + "/wpa_supplicant"
	_ = os.MkdirAll(ctrlDir, 0o755)
	spec := &supervisor.Spec{
		Name:    "wpa_supplicant",
		Argv:    []string{"wpa_supplicant", "-i", ifaceName, "-c", confPath, "-C", ctrlDir, "-t"},
		LogRing: supervisor.NewRing(400),
	}
	if err := m.sup.Start(spec); err != nil {
		m.fail("启动 wpa_supplicant 失败: " + err.Error())
		return err
	}

	// 等 ctrl socket 就绪并 ATTACH
	var ctrl *netctrl.Client
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := netctrl.Dial(ctrlDir, ifaceName)
		if err == nil {
			if _, err := c.Request("PING"); err == nil {
				ctrl = c
				break
			}
			c.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
	if ctrl == nil {
		m.sup.Stop("wpa_supplicant", 5*time.Second)
		m.fail("wpa_supplicant 控制接口 10s 未就绪")
		return fmt.Errorf("wpa_supplicant 未就绪")
	}
	m.mu.Lock()
	m.ctrl = ctrl
	m.stopWatch = make(chan struct{})
	stop := m.stopWatch
	m.fails = 0
	m.mu.Unlock()
	go m.watchEvents(ctrl, stop)

	// 等关联结果（事件 watchEvents 会置状态；这里同步等一段）
	if err := m.waitConnected(30 * time.Second); err != nil {
		return err
	}
	return nil
}

// waitConnected 轮询状态直到 connected 或超时
func (m *Manager) waitConnected(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		st := m.state
		m.mu.Unlock()
		if st == StateConnected {
			return nil
		}
		if st == StateFailed {
			m.mu.Lock()
			d := m.detail
			m.mu.Unlock()
			return fmt.Errorf("连接失败: %s", d)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("连接超时（30s 内未完成关联）")
}

// Disconnect 断开并停止 wpa_supplicant
func (m *Manager) Disconnect() error {
	m.mu.Lock()
	ctrl := m.ctrl
	stop := m.stopWatch
	m.ctrl = nil
	m.stopWatch = nil
	m.state = StateOff
	m.detail = ""
	m.fails = 0
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if ctrl != nil {
		ctrl.Close()
	}
	m.sup.Stop("wpa_supplicant", 5*time.Second)
	// 释放 DHCP 地址（失败可忽略——dhclient 可能已退出）
	priv.Run(10*time.Second, "dhclient", "-r", m.cfg.Relay.Interface)
	priv.AddrFlush(m.cfg.Relay.Interface)
	m.mu.Lock()
	m.cfg.Relay.Current = ""
	m.mu.Unlock()
	m.publish()
	if m.OnNetworkChanged != nil {
		m.OnNetworkChanged()
	}
	return nil
}

// ---------- 事件与重连 ----------

// watchEvents 消费 wpa_supplicant 事件，维护状态机
func (m *Manager) watchEvents(c *netctrl.Client, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-c.Events():
			if !ok {
				return
			}
			m.handleEvent(ev)
		}
	}
}

func (m *Manager) handleEvent(ev string) {
	m.log.Info("wpa事件", "ev", strings.TrimSpace(ev))
	m.mu.Lock()
	m.lastEvent = strings.TrimSpace(ev)
	switch {
	case strings.Contains(ev, "CTRL-EVENT-CONNECTED"):
		m.state = StateConnected
		m.detail = ""
		m.fails = 0
		ifaceName := m.cfg.Relay.Interface
		m.mu.Unlock()
		m.publish()
		go m.dhcpAcquire(ifaceName)
		return
	case strings.Contains(ev, "CTRL-EVENT-DISCONNECTED"):
		if m.state == StateConnected {
			m.state = StateBackoff
			m.fails++
		}
	case strings.Contains(ev, "CTRL-EVENT-ASSOC-REJECT"),
		strings.Contains(ev, "CTRL-EVENT-AUTH-REJECT"),
		strings.Contains(ev, "WRONG_KEY"), strings.Contains(ev, "4-Way Handshake failed"):
		m.fails++
		if m.state == StateConnecting {
			m.state = StateBackoff
		}
	case strings.Contains(ev, "CTRL-EVENT-NETWORK-NOT-FOUND"):
		m.state = StateFailed
		m.detail = "扫描不到该 SSID"
	case strings.Contains(ev, "CTRL-EVENT-SSID-TEMP-DISABLED"):
		// wpa_supplicant 把网络临时禁用（反复失败）：置 backoff 让 FSM 介入
		if m.state == StateConnecting {
			m.state = StateBackoff
		}
	}
	m.mu.Unlock()
	m.publish()

	// 触发重连调度（异步：退避睡眠不能阻塞事件读取）
	go m.scheduleReconnect()
}

// dhcpAcquire 关联成功后获取地址（dhclient -1 一次性）
func (m *Manager) dhcpAcquire(ifaceName string) {
	r := priv.Run(60*time.Second, "dhclient", "-1", "-q", ifaceName)
	if r.Err != nil {
		m.log.Warn("DHCP 获取地址失败", "iface", ifaceName, "err", r.Stderr)
		m.mu.Lock()
		if m.state == StateConnected {
			m.detail = "已关联但未获得 IP（上游 DHCP 未响应）"
		}
		m.mu.Unlock()
		m.publish()
		return
	}
	m.log.Info("中继获得地址", "iface", ifaceName, "addr", ifaceAddr(ifaceName))
	if m.OnNetworkChanged != nil {
		m.OnNetworkChanged()
	}
}

// scheduleReconnect 指数退避重连；连续失败超过阈值 → 重扫并切换次优先级
func (m *Manager) scheduleReconnect() {
	m.mu.Lock()
	if m.state != StateBackoff || m.ctrl == nil {
		m.mu.Unlock()
		return
	}
	fails := m.fails
	rescanAfter := m.cfg.Relay.Reconnect.RescanAfter
	if rescanAfter <= 0 {
		rescanAfter = 3
	}
	backoffMax := m.cfg.Relay.Reconnect.BackoffMaxSeconds
	if backoffMax <= 0 {
		backoffMax = 30
	}
	ctrl := m.ctrl
	m.mu.Unlock()

	backoff := 1 << uint(min(fails-1, 5))
	if backoff > backoffMax {
		backoff = backoffMax
	}

	time.Sleep(time.Duration(backoff) * time.Second)

	m.mu.Lock()
	if m.state != StateBackoff || m.ctrl == nil || m.ctrl != ctrl {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	if fails >= rescanAfter {
		m.log.Info("连续失败达到阈值，重新扫描并启用全部已存网络（切换次优先级）", "fails", fails)
		_, _ = ctrl.Request("ENABLE_NETWORK all")
		_, _ = ctrl.Request("REASSOCIATE")
	} else {
		_, _ = ctrl.Request("REASSOCIATE")
	}
}

func (m *Manager) fail(msg string) {
	m.mu.Lock()
	m.state = StateFailed
	m.detail = msg
	m.mu.Unlock()
	m.log.Error("中继失败", "err", msg)
	m.publish()
}

func (m *Manager) publish() {
	m.evbus.Publish("status", map[string]any{"relay": m.Status()})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ensureManagedType 接口类型归位 managed（hostapd 退出后常残留 AP 类型）
func ensureManagedType(ifaceName string) error {
	out := priv.Run(5*time.Second, "iw", "dev").Stdout
	// 找该接口的 type 字段
	inTarget := false
	curType := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Interface ") {
			inTarget = strings.TrimPrefix(t, "Interface ") == ifaceName
			continue
		}
		if inTarget && strings.HasPrefix(t, "type ") {
			curType = strings.TrimPrefix(t, "type ")
			break
		}
	}
	if curType == "managed" {
		return nil
	}
	// 切类型须先 down
	priv.SetDown(ifaceName)
	r := priv.Run(5*time.Second, "iw", "dev", ifaceName, "set", "type", "managed")
	if r.Err != nil {
		return fmt.Errorf("%s（原类型 %s）", strings.TrimSpace(r.Stderr), curType)
	}
	return priv.SetUp(ifaceName)
}

// parseSignal 从 STATUS 响应取当前信号（dBm）
func parseSignal(resp string) int {
	for _, line := range strings.Split(resp, "\n") {
		if strings.HasPrefix(line, "signal=") {
			v, _ := strconv.Atoi(strings.TrimPrefix(line, "signal="))
			return v
		}
	}
	return 0
}

// IsConnected STA 是否已连接（netrt 判上游类型用）
func (m *Manager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == StateConnected
}

// IfaceName STA 接口名
func (m *Manager) IfaceName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Relay.Interface
}
