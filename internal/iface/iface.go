// Package iface 统一网络接口抽象层：所有模块只经它看/操作接口。
// 职责：netlink 快照、无线能力探测（iw list 解析）、2s 统计采样发 traffic 事件。
package iface

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"apmanager/internal/bus"
	"apmanager/internal/priv"
)

// Kind 接口类别
type Kind string

const (
	KindEthernet Kind = "ethernet"
	KindWireless Kind = "wireless"
	KindBridge   Kind = "bridge"
	KindLoopback Kind = "loopback"
	KindOther    Kind = "other"
)

// Role 配置里声明的角色（抽象层只存不判）
type Role string

const (
	RoleWan    Role = "wan"
	RoleLan    Role = "lan"
	RoleUnused Role = "unused"
)

// Iface 对外暴露的接口视图
type Iface struct {
	Name       string   `json:"name"`
	MAC        string   `json:"mac"`
	Kind       Kind     `json:"kind"`
	Role       Role     `json:"role"`
	OperState  string   `json:"operState"`
	Addrs      []string `json:"addrs"`
	MTU        int      `json:"mtu"`
	Master     string   `json:"master,omitempty"` // 桥成员时所属桥
	SpeedMbps  int      `json:"speedMbps"`        // eth 用 ethtool 读
	RxBytes    uint64   `json:"rxBytes"`
	TxBytes    uint64   `json:"txBytes"`
	RxRate     float64  `json:"rxRate"` // B/s，采样差分
	TxRate     float64  `json:"txRate"`
	Wireless   *WirelessInfo `json:"wireless,omitempty"`
}

// WirelessInfo 无线能力（iw list 解析产物）
type WirelessInfo struct {
	Phy           string   `json:"phy"`
	Bands         []string `json:"bands"`          // ["2g","5g"]
	Channels      map[string][]int `json:"channels"` // band -> 合法信道
	CapAP         bool     `json:"capAP"`
	CapSTA        bool     `json:"capSTA"`
	ComboSupported bool    `json:"comboSupported"` // 是否声明多接口组合
	ComboSameChannelOnly bool `json:"comboSameChannelOnly"` // true=共存须同信道
	ComboTested   *bool    `json:"comboTested,omitempty"`   // 实测能否添加第二个接口
	CurrentChan   int      `json:"currentChan"`
	Mode          string   `json:"mode"` // managed / AP / ...
}

// Manager 接口管理器
type Manager struct {
	mu       sync.RWMutex
	cache    map[string]*Iface
	lastStat map[string]priv.LinkStats
	lastTime time.Time
	bus      *bus.Bus
	log      *slog.Logger
	wireless map[string]*WirelessInfo // 接口名 -> 能力（探测一次缓存）
	roles    map[string]Role         // 来自 config 的角色声明
}

func New(b *bus.Bus, log *slog.Logger) *Manager {
	return &Manager{
		cache: map[string]*Iface{},
		lastStat: map[string]priv.LinkStats{},
		bus: b, log: log,
		wireless: map[string]*WirelessInfo{},
		roles: map[string]Role{},
	}
}

// SetRoles 同步配置声明的角色（config 变更后调用）
func (m *Manager) SetRoles(r map[string]Role) {
	m.mu.Lock()
	m.roles = r
	m.mu.Unlock()
}

// ProbeAll 启动时做一次全量探测（含无线能力），耗时约 1s
func (m *Manager) ProbeAll() {
	m.refresh(false)
	if priv.Mode != "dev" {
		m.probeWirelessCaps()
		// 实测接口组合：能否在同 phy 上再建一个 managed 接口（删不掉也没关系，iw dev del 兜底）
		m.testCombo()
	}
}

// List 返回全部接口（角色来自 config 声明）
func (m *Manager) List() []Iface {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Iface, 0, len(m.cache))
	for _, ifc := range m.cache {
		v := *ifc
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get 单个接口快照
func (m *Manager) Get(name string) (Iface, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ifc, ok := m.cache[name]
	if !ok {
		return Iface{}, false
	}
	return *ifc, true
}

// StartSampler 启动 2s 采样循环：刷新统计、计算速率、发 traffic 事件
func (m *Manager) StartSampler(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.refresh(true)
			}
		}
	}()
}

// refresh 重新 dump netlink；emit=true 时发 traffic 事件
func (m *Manager) refresh(emit bool) {
	links, err := priv.AllLinks()
	if err != nil {
		m.log.Error("iface dump 失败", "err", err)
		return
	}
	now := time.Now()
	var dt float64 = 2
	m.mu.Lock()
	if !m.lastTime.IsZero() {
		dt = now.Sub(m.lastTime).Seconds()
	}
	traffic := map[string][2]float64{}
	for _, li := range links {
		kind := classify(li)
		ifc, ok := m.cache[li.Name]
		if !ok {
			ifc = &Iface{Name: li.Name}
			m.cache[li.Name] = ifc
		}
		old := m.lastStat[li.Name]
		ifc.MAC = li.MAC
		ifc.Kind = kind
		ifc.OperState = li.OperState
		ifc.Addrs = li.Addrs
		ifc.MTU = li.MTU
		ifc.Master = li.Master
		ifc.RxBytes = li.Stats.RxBytes
		ifc.TxBytes = li.Stats.TxBytes
		if dt > 0.1 && emit {
			ifc.RxRate = float64(int64(li.Stats.RxBytes)-int64(old.RxBytes)) / dt
			ifc.TxRate = float64(int64(li.Stats.TxBytes)-int64(old.TxBytes)) / dt
		}
		if r, ok := m.roles[li.Name]; ok {
			ifc.Role = r
		} else if ifc.Master != "" {
			// 桥成员继承桥的角色
			if r, ok := m.roles[ifc.Master]; ok {
				ifc.Role = r
			}
		} else if ifc.Role == "" {
			ifc.Role = RoleUnused
		}
		ifc.Wireless = m.wireless[li.Name]
		if kind == KindEthernet {
			ifc.SpeedMbps = readEthtoolSpeed(li.Name)
		}
		m.lastStat[li.Name] = li.Stats
		traffic[li.Name] = [2]float64{ifc.RxRate, ifc.TxRate}
	}
	// 删除已消失的接口
	for name := range m.cache {
		alive := false
		for _, li := range links {
			if li.Name == name {
				alive = true
				break
			}
		}
		if !alive {
			delete(m.cache, name)
			delete(m.lastStat, name)
		}
	}
	m.lastTime = now
	m.mu.Unlock()
	if emit {
		m.bus.Publish("traffic", map[string]any{
			"ts":      now.UnixMilli(),
			"ifaces":  traffic,
			"links":   m.List(),
		})
	}
}

func classify(li priv.LinkInfo) Kind {
	switch {
	case li.Type == "loopback":
		return KindLoopback
	case li.Type == "bridge":
		return KindBridge
	case li.IsWireless:
		return KindWireless
	case li.Type == "device":
		return KindEthernet
	default:
		return KindOther
	}
}

// readEthtoolSpeed 解析 ethtool 的 "Speed: 1000Mb/s"。
// 网卡速率极少变化：加 60s TTL 缓存，否则 2s 巡检每次都 exec ethtool，
// 审计日志被刷屏且白白消耗 CPU。
type ethSpeedEntry struct {
	at  time.Time
	val int
}

var (
	ethSpeedMu    sync.Mutex
	ethSpeedCache = map[string]ethSpeedEntry{}
)

func readEthtoolSpeed(name string) int {
	if priv.Mode == "dev" {
		return 1000
	}
	ethSpeedMu.Lock()
	c, ok := ethSpeedCache[name]
	ethSpeedMu.Unlock()
	if ok && time.Since(c.at) < time.Minute {
		return c.val
	}
	out := priv.RunQuiet(3*time.Second, "ethtool", name)
	mb := 0
	for _, line := range strings.Split(out.Stdout, "\n") {
		if strings.HasPrefix(line, "\tSpeed:") {
			s := strings.TrimSpace(strings.TrimPrefix(line, "\tSpeed:"))
			if mb, _ = fmt.Sscanf(s, "%dMb/s"); mb > 0 {
				break
			}
		}
	}
	if out.Err == nil {
		ethSpeedMu.Lock()
		ethSpeedCache[name] = ethSpeedEntry{time.Now(), mb}
		ethSpeedMu.Unlock()
	}
	return mb
}

// probeWirelessCaps 解析 `iw list` 的能力段，填充 m.wireless。
// 能力（频段/信道/组合）只在驱动重载或改 regdom 时变化：加 60s TTL 缓存，
// 避免 2s 巡检反复执行重命令 `iw list`。
var (
	capsMu  sync.Mutex
	capsAt  time.Time
	capsOut string
	capsDev string
)

func (m *Manager) probeWirelessCaps() {
	capsMu.Lock()
	if !capsAt.IsZero() && time.Since(capsAt) < time.Minute {
		out, iwdev := capsOut, capsDev
		capsMu.Unlock()
		m.applyCaps(out, iwdev)
		return
	}
	capsMu.Unlock()
	out := priv.RunQuiet(15*time.Second, "iw", "list").Stdout
	iwdev := priv.RunQuiet(5*time.Second, "iw", "dev").Stdout
	capsMu.Lock()
	capsAt, capsOut, capsDev = time.Now(), out, iwdev
	capsMu.Unlock()
	m.applyCaps(out, iwdev)
}

func (m *Manager) applyCaps(out, iwdev string) {
	wi := parseIwList(out)
	// iw dev: 每个 Interface 段有 iface/phy/type/channel 信息
	curChan, mode := parseIwDev(iwdev)
	wi.CurrentChan = curChan
	wi.Mode = mode
	m.mu.Lock()
	m.wireless["wlp1s0"] = wi // 当前只有一张卡；多卡场景按 phy 归属扩展
	m.mu.Unlock()
}

// parseIwList 解析 iw list 关键段：接口模式、频段信道、组合能力
func parseIwList(out string) *WirelessInfo {
	wi := &WirelessInfo{Phy: "phy0", Channels: map[string][]int{}}
	sec := section(out, "Supported interface modes:")
	for _, line := range sec {
		t := strings.TrimSpace(line)
		switch t {
		case "* AP", "* AP/VLAN":
			wi.CapAP = true
		case "* managed":
			wi.CapSTA = true
		}
	}
	// 频段与信道
	bandIdx := 0
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Band ") && strings.HasSuffix(t, ":") {
			bandIdx++
			continue
		}
		if strings.Contains(t, "MHz [") {
			ch, ok := parseChanFreq(t)
			if ok {
				band := "2g"
				if bandIdx >= 2 {
					band = "5g"
				}
				wi.Channels[band] = append(wi.Channels[band], ch)
			}
		}
	}
	for b := range wi.Channels {
		wi.Bands = append(wi.Bands, b)
		if b == "2g" && len(wi.Channels[b]) > 0 {
			// 保持顺序
		}
	}
	sort.Strings(wi.Bands) // 2g 在前 5g 在后
	// 组合能力
	if strings.Contains(out, "interface combinations are not supported") {
		wi.ComboSupported = false
	} else {
		sec = section(out, "valid interface combinations:")
		wi.ComboSupported = true
		for _, line := range sec {
			t := strings.TrimSpace(line)
			// "#channels <= 1" 表示只能同信道共存；"#{ AP }" 与 "#channels" 都可能出现
			if strings.Contains(t, "#channels <= 1") {
				wi.ComboSameChannelOnly = true
			}
			if strings.Contains(t, "total <= 1") {
				wi.ComboSupported = false // 只许一个接口
			}
		}
	}
	return wi
}

// section 提取标题行之后到下一个同级缩进段的行
func section(out, header string) []string {
	var lines []string
	in := false
	prefix := ""
	for _, line := range strings.Split(out, "\n") {
		if !in {
			if strings.Contains(line, header) {
				in = true
				// 记录标题行缩进，正文行缩进更深
				prefix = line[:len(line)-len(strings.TrimLeft(line, "\t"))]
			}
			continue
		}
		if line == "" {
			continue
		}
		cur := line[:len(line)-len(strings.TrimLeft(line, "\t"))]
		if len(cur) <= len(prefix) && strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "*") {
			// 回到标题同级或更浅：段结束
			break
		}
		lines = append(lines, line)
	}
	return lines
}

// parseChanFreq 从 "\t* 2412.0 MHz [1] (20.0 dBm)" 提取信道号；被动扫描/禁用信道仍列出但标记跳过 radar
func parseChanFreq(t string) (int, bool) {
	i := strings.Index(t, "MHz [")
	if i < 0 {
		return 0, false
	}
	rest := t[i+len("MHz ["):]
	j := strings.Index(rest, "]")
	if j < 0 {
		return 0, false
	}
	ch, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	// (no IR) / radar 信道不可用于 AP 发射，剔除
	if strings.Contains(t, "(no IR)") || strings.Contains(t, "radar") {
		return ch, false
	}
	return ch, true
}

// parseIwDev 从 iw dev 输出解析当前信道与模式（任意接口）
func parseIwDev(out string) (chNum int, mode string) {
	mode = "managed"
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "type ") {
			mode = strings.TrimPrefix(t, "type ")
		}
		if strings.HasPrefix(t, "channel ") {
			// "channel 6 (2437 MHz), width: 20 MHz ..."
			f := strings.Fields(t)
			if len(f) >= 2 {
				if c, err := strconv.Atoi(f[1]); err == nil {
					return c, mode
				}
			}
		}
	}
	return 0, mode
}

// testCombo 实测组合能力：在 phy0 上尝试加一个 managed 接口，记录结果并清理
func (m *Manager) testCombo() {
	added := priv.Run(5*time.Second, "iw", "phy", "phy0", "interface", "add", "apm-combo-test", "type", "managed")
	ok := added.Err == nil
	tested := ok
	m.mu.Lock()
	if wi, ok2 := m.wireless["wlp1s0"]; ok2 {
		wi.ComboTested = &tested
	}
	m.mu.Unlock()
	if ok {
		// rtw88 驱动上 ip link del 会报 Operation not supported，必须走 iw
		priv.Run(5*time.Second, "iw", "dev", "apm-combo-test", "del")
		m.log.Info("接口组合实测", "add_second_iface", "成功（AP+STA 共存可行性待关联实测）")
	} else {
		m.log.Warn("接口组合实测", "add_second_iface", "失败", "stderr", added.Stderr)
	}
}

// WirelessCaps 便捷读取无线能力
func (m *Manager) WirelessCaps(ifaceName string) *WirelessInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	wi := m.wireless[ifaceName]
	if wi == nil {
		return nil
	}
	cp := *wi
	return &cp
}

// ValidChannels 给定频段返回合法信道列表
func (wi *WirelessInfo) ValidChannels(band string) []int {
	if wi == nil {
		return nil
	}
	return wi.Channels[band]
}
