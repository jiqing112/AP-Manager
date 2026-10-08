// Package ethport 网口管理：角色分配（WAN/LAN/unused）、LAN 桥与多网段、
// 多口桥接、WAN 静态/DHCP 接入、安全应用（快照 → 应用 → 60s 未确认自动回滚）。
package ethport

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"apmanager/internal/ap"
	"apmanager/internal/bus"
	"apmanager/internal/config"
	"apmanager/internal/iface"
	"apmanager/internal/priv"
	"apmanager/internal/supervisor"
)

// Manager 网口模块
type Manager struct {
	mu      sync.Mutex
	cfg     *config.Config
	cfgPath string
	ifaces  *iface.Manager
	apm     *ap.Manager
	sup     *supervisor.Supervisor
	evbus   *bus.Bus
	log     *slog.Logger

	pending *PendingApply
	// OnNetworkChanged 应用/回滚后通知 netrt 重刷（main 注入）
	OnNetworkChanged func()
	// RestartAP AP 归属桥变化时需要重启 AP（main 注入）
	RestartAP func() error
}

// PendingApply 一次待确认的安全应用
type PendingApply struct {
	ID          string        `json:"id"`
	Description string        `json:"description"`
	Deadline    time.Time     `json:"deadline"`
	NewPorts    []config.Port `json:"newPorts"`
	NewLAN      []config.LANSeg `json:"newLan"`
	// 回滚用的旧值（不序列化给前端）
	RollbackPorts []config.Port   `json:"-"`
	RollbackLAN   []config.LANSeg `json:"-"`
}

// New 构造
func New(cfg *config.Config, cfgPath string, ifaces *iface.Manager, apm *ap.Manager,
	sup *supervisor.Supervisor, evbus *bus.Bus, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, cfgPath: cfgPath, ifaces: ifaces, apm: apm, sup: sup, evbus: evbus, log: log}
}

// SetConfig 热更新引用
func (m *Manager) SetConfig(c *config.Config) {
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
}

// Pending 当前待确认的应用（无则 nil）
func (m *Manager) Pending() *PendingApply {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		return nil
	}
	cp := *m.pending
	return &cp
}

// Candidate 一次角色/网段变更请求
type Candidate struct {
	Ports []config.Port   `json:"ports"`
	LAN   []config.LANSeg `json:"lan"`
	// AcknowledgeRisk 在"当前管理连接所在网段的接口"上做变更需显式勾选
	AcknowledgeRisk bool `json:"acknowledgeRisk"`
}

// ValidatePlan 校验变更方案的合法性，返回错误列表
func (m *Manager) ValidatePlan(cand Candidate) []string {
	m.mu.Lock()
	cur := m.cfg
	m.mu.Unlock()
	var errs []string

	seen := map[string]bool{}
	subnets := []*net.IPNet{}
	for i, p := range cand.Ports {
		if seen[p.Name] {
			errs = append(errs, fmt.Sprintf("端口 %s 重复出现", p.Name))
		}
		seen[p.Name] = true
		if p.Role == "lan" && p.Bridge == "" {
			errs = append(errs, fmt.Sprintf("端口 %s 角色为 LAN 但未指定网桥", p.Name))
		}
		if p.Role == "wan" && p.WAN != nil && p.WAN.Mode == "static" {
			if _, _, err := net.ParseCIDR(p.WAN.Address); err != nil {
				errs = append(errs, fmt.Sprintf("端口 %s 静态地址 %q 不是合法 CIDR", p.Name, p.WAN.Address))
			}
			if p.WAN.Gateway != "" && net.ParseIP(p.WAN.Gateway) == nil {
				errs = append(errs, fmt.Sprintf("端口 %s 网关 %q 非法", p.Name, p.WAN.Gateway))
			}
		}
		_ = i
	}
	// LAN 段校验：CIDR 合法、网段不重叠、名字唯一
	lanSeen := map[string]bool{}
	brSeen := map[string]bool{}
	for i, seg := range cand.LAN {
		if seg.Name == "" {
			errs = append(errs, fmt.Sprintf("lan[%d] 缺少名字", i))
		}
		if lanSeen[seg.Name] {
			errs = append(errs, fmt.Sprintf("LAN 段名 %s 重复", seg.Name))
		}
		lanSeen[seg.Name] = true
		if seg.Bridge != "" {
			if brSeen[seg.Bridge] {
				errs = append(errs, fmt.Sprintf("网桥 %s 被多个 LAN 段使用", seg.Bridge))
			}
			brSeen[seg.Bridge] = true
		}
		_, ipnet, err := net.ParseCIDR(seg.Subnet)
		if err != nil {
			errs = append(errs, fmt.Sprintf("LAN 段 %s 子网 %q 非法", seg.Name, seg.Subnet))
			continue
		}
		ones, _ := ipnet.Mask.Size()
		if ones > 24 {
			errs = append(errs, fmt.Sprintf("LAN 段 %s 子网过小（/%d，最大 /24）", seg.Name, ones))
		}
		for j, other := range subnets {
			if other.Contains(ipnet.IP) || ipnet.Contains(other.IP) {
				errs = append(errs, fmt.Sprintf("LAN 段 %s 与第 %d 个段地址重叠", seg.Name, j+1))
			}
		}
		subnets = append(subnets, ipnet)
		// 池地址须在段内
		for _, p := range seg.DHCPPool {
			if ip := net.ParseIP(p); ip == nil || !ipnet.Contains(ip) {
				errs = append(errs, fmt.Sprintf("LAN 段 %s 池地址 %q 不在子网内", seg.Name, p))
			}
		}
	}
	// LAN 口引用的桥必须存在于 LAN 段定义
	for _, p := range cand.Ports {
		if p.Role != "lan" {
			continue
		}
		ok := false
		for _, seg := range cand.LAN {
			if seg.Bridge == p.Bridge {
				ok = true
				break
			}
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("端口 %s 引用的网桥 %s 没有对应 LAN 段", p.Name, p.Bridge))
		}
	}
	_ = cur
	return errs
}

// detectIfupdownConflict 检测目标口是否被 /etc/network/interfaces 托管（ifupdown 冲突告警）
func detectIfupdownConflict(ports []config.Port) []string {
	data, err := readFileAll("/etc/network/interfaces")
	if err != nil {
		return nil
	}
	entries, _ := readDirAll("/etc/network/interfaces.d")
	data += "\n" + entries
	var warns []string
	for _, p := range ports {
		if p.Role == "unused" {
			continue
		}
		if strings.Contains(data, " "+p.Name+"\n") || strings.Contains(data, " "+p.Name+" ") || strings.Contains(data, "\t"+p.Name+"\n") {
			warns = append(warns, fmt.Sprintf("端口 %s 正被 ifupdown(/etc/network/interfaces) 托管，双方同时管理会冲突；请先从该文件移除对应 iface 段", p.Name))
		}
	}
	return warns
}

// Plan 预检：校验 + 冲突检测 + 风险评估，返回给 UI 确认页展示
func (m *Manager) Plan(cand Candidate) (warns []string, risks []string, errs []string) {
	errs = m.ValidatePlan(cand)
	if len(errs) > 0 {
		return nil, nil, errs
	}
	warns = detectIfupdownConflict(cand.Ports)
	// 风险评估：当前默认路由口角色变化 / 管理连接所在口变化
	dr, err := priv.DefaultRoute4()
	if err == nil {
		for _, p := range cand.Ports {
			if p.Name == dr.Iface {
				for _, old := range m.cfg.Ports {
					if old.Name == p.Name && old.Role != p.Role {
						risks = append(risks, fmt.Sprintf("端口 %s 是当前默认路由/上行口，角色从 %s 改为 %s 可能导致本机失联（有 60s 自动回滚保护）", p.Name, old.Role, p.Role))
					}
				}
			}
		}
	}
	return warns, risks, errs
}

// Apply 执行安全应用：快照 → 变更 → 60s 确认窗口（未确认自动回滚）
func (m *Manager) Apply(cand Candidate, description string) (string, error) {
	warns, risks, errs := m.Plan(cand)
	_ = warns
	if len(errs) > 0 {
		return "", fmt.Errorf("方案不合法: %s", strings.Join(errs, "; "))
	}
	if len(risks) > 0 && !cand.AcknowledgeRisk {
		return "", fmt.Errorf("存在风险项需勾选「我知道风险」: %s", strings.Join(risks, "; "))
	}

	m.mu.Lock()
	if m.pending != nil {
		p := m.pending
		m.mu.Unlock()
		return "", fmt.Errorf("已有待确认的应用 %s（%s 截止），请先确认或回滚", p.ID, p.Deadline.Format("15:04:05"))
	}
	m.mu.Unlock()

	// 1. 快照（回滚依据）
	snap, err := takeSnapshot(m.cfg, cand)
	if err != nil {
		return "", fmt.Errorf("快照失败: %w", err)
	}

	// 2. 执行变更（候选配置先落到内存，netrt/dnsmasq/nft 立即按新拓扑工作；
	//    确认后才写 config.yaml，回滚则恢复旧内存配置并重新落地）
	oldPorts := deepCopyPorts(m.cfg.Ports)
	oldLAN := deepCopyLAN(m.cfg.LAN)
	newCfg := *m.cfg
	newCfg.Ports = deepCopyPorts(cand.Ports)
	newCfg.LAN = deepCopyLAN(cand.LAN)

	m.log.Info("开始应用网口变更", "desc", description)
	m.mu.Lock()
	m.cfg.Ports = newCfg.Ports
	m.cfg.LAN = newCfg.LAN
	m.mu.Unlock()
	if err := m.realize(&newCfg, &config.Config{Ports: oldPorts, LAN: oldLAN}); err != nil {
		// 应用失败：恢复内存配置 + 回滚快照
		m.log.Error("应用失败，立即回滚", "err", err)
		m.mu.Lock()
		m.cfg.Ports = oldPorts
		m.cfg.LAN = oldLAN
		m.mu.Unlock()
		if rbErr := snap.restore(); rbErr != nil {
			m.log.Error("回滚也失败了！需要人工介入", "err", rbErr)
		}
		if m.OnNetworkChanged != nil {
			m.OnNetworkChanged()
		}
		return "", fmt.Errorf("应用失败已回滚: %w", err)
	}

	// 3. 挂起确认窗口
	id := fmt.Sprintf("apply-%d", time.Now().UnixMilli())
	m.mu.Lock()
	m.pending = &PendingApply{
		ID: id, Description: description,
		Deadline: time.Now().Add(60 * time.Second),
		NewPorts: newCfg.Ports, NewLAN: newCfg.LAN,
		RollbackPorts: oldPorts, RollbackLAN: oldLAN,
	}
	m.mu.Unlock()

	if m.OnNetworkChanged != nil {
		m.OnNetworkChanged()
	}

	// 4. 60s 自动回滚定时器
	go func() {
		deadline := m.Pending().Deadline
		time.Sleep(time.Until(deadline))
		m.mu.Lock()
		p := m.pending
		if p == nil || p.ID != id {
			m.mu.Unlock()
			return // 已确认或已回滚
		}
		m.pending = nil
		m.mu.Unlock()
		m.log.Warn("应用未在 60s 内确认，自动回滚")
		if err := m.rollbackTo(snap, oldPorts, oldLAN); err != nil {
			m.log.Error("自动回滚失败", "err", err)
		}
		m.evbus.Publish("status", map[string]any{"net_rolled_back": map[string]string{"id": id, "reason": "60s 未确认"}})
	}()
	m.evbus.Publish("status", map[string]any{"net_pending": m.Pending()})
	return id, nil
}

// rollbackTo 恢复内存配置 + 快照恢复（若有）+ 按旧配置重新落地
func (m *Manager) rollbackTo(snap *Snapshot, oldPorts []config.Port, oldLAN []config.LANSeg) error {
	m.mu.Lock()
	m.cfg.Ports = oldPorts
	m.cfg.LAN = oldLAN
	old := *m.cfg
	m.mu.Unlock()
	if snap != nil {
		if err := snap.restore(); err != nil {
			m.log.Error("快照恢复失败，改为按旧配置重新落地", "err", err)
		}
	}
	if err := m.realize(&old, nil); err != nil {
		return fmt.Errorf("回滚落地失败: %w", err)
	}
	if m.OnNetworkChanged != nil {
		m.OnNetworkChanged()
	}
	return nil
}

// Confirm 确认应用：内存中的新配置落盘持久化
func (m *Manager) Confirm(id string) error {
	m.mu.Lock()
	p := m.pending
	if p == nil || p.ID != id {
		m.mu.Unlock()
		return fmt.Errorf("没有这个待确认的应用")
	}
	m.pending = nil
	cfgRef := m.cfg
	path := m.cfgPath
	m.mu.Unlock()
	if err := cfgRef.Save(path); err != nil {
		return err
	}
	m.log.Info("网口变更已确认并持久化", "id", id)
	m.evbus.Publish("status", map[string]any{"net_confirmed": id})
	return nil
}

// Rollback 手动回滚（用户点了撤销）：内存配置已换新，需要恢复应用前的旧值。
// 旧值从 pending 之前的配置推导不出（未存），因此手动回滚 = 按 pending 前状态重新落地。
// 为可靠起见，Apply 时把旧值存进 pending（RollbackOld）。
func (m *Manager) Rollback(id string) error {
	m.mu.Lock()
	p := m.pending
	if p == nil || p.ID != id {
		m.mu.Unlock()
		return fmt.Errorf("没有这个待确认的应用")
	}
	m.pending = nil
	oldPorts := p.RollbackPorts
	oldLAN := p.RollbackLAN
	m.mu.Unlock()
	m.log.Info("用户手动回滚", "id", id)
	if err := m.rollbackTo(nil, oldPorts, oldLAN); err != nil {
		return err
	}
	m.evbus.Publish("status", map[string]any{"net_rolled_back": map[string]string{"id": id, "reason": "用户撤销"}})
	return nil
}

// realize 把目标配置变成系统现实。old 为 nil 表示这是回滚（跳过一些前置清理）。
func (m *Manager) realize(target *config.Config, old *config.Config) error {
	// PPPoE：目标里没有 pppoe WAN 就停掉 pppd
	hasPPPoE := false
	for _, p := range target.Ports {
		if p.Role == "wan" && p.WAN != nil && p.WAN.Mode == "pppoe" {
			hasPPPoE = true
			break
		}
	}
	if !hasPPPoE {
		m.stopPPPoE()
	}
	// 3.1 LAN 桥：创建 + 配地址 + up
	for _, seg := range target.LAN {
		if seg.Bridge == "" {
			continue // AP 直挂形态，地址由 AP 链路负责
		}
		if err := priv.BridgeCreate(seg.Bridge); err != nil {
			return fmt.Errorf("创建桥 %s: %w", seg.Bridge, err)
		}
		if err := priv.AddrReplace(seg.Bridge, seg.Subnet); err != nil {
			return fmt.Errorf("桥 %s 配地址 %s: %w", seg.Bridge, seg.Subnet, err)
		}
	}
	// 3.2 端口角色落地
	for _, p := range target.Ports {
		switch p.Role {
		case "lan":
			// 桥成员不能有 IP：先清再挂桥
			_ = priv.AddrFlush(p.Name)
			if err := priv.BridgeAddPort(p.Bridge, p.Name); err != nil {
				return fmt.Errorf("把 %s 挂进桥 %s: %w", p.Name, p.Bridge, err)
			}
			_ = priv.SetUp(p.Name)
		case "wan":
			if err := priv.BridgeDelPort(p.Name); err != nil {
				return fmt.Errorf("把 %s 移出桥: %w", p.Name, err)
			}
			_ = priv.SetUp(p.Name)
			if p.WAN != nil && p.WAN.Mode == "static" {
				if err := priv.AddrReplace(p.Name, p.WAN.Address); err != nil {
					return fmt.Errorf("WAN 静态地址 %s: %w", p.Name, err)
				}
			}
			if p.WAN != nil && p.WAN.Mode == "dhcp" {
				go priv.Run(60*time.Second, "dhclient", "-1", "-q", p.Name)
			}
			if p.WAN != nil && p.WAN.Mode == "pppoe" {
				if err := m.startPPPoE(p); err != nil {
					return fmt.Errorf("PPPoE 启动: %w", err)
				}
			}
		default: // unused：移出桥、清地址
			_ = priv.BridgeDelPort(p.Name)
			if old != nil { // 全新应用才清地址；避免误伤尚未迁移的场景
				_ = priv.AddrFlush(p.Name)
			}
		}
	}
	// 3.3 不再使用的桥：拆掉（成员先摘干净）
	if old != nil {
		newBridges := map[string]bool{}
		for _, seg := range target.LAN {
			if seg.Bridge != "" {
				newBridges[seg.Bridge] = true
			}
		}
		for _, seg := range old.LAN {
			if seg.Bridge != "" && !newBridges[seg.Bridge] {
				if err := priv.BridgeDelete(seg.Bridge); err != nil {
					m.log.Warn("删除旧桥失败", "bridge", seg.Bridge, "err", err)
				}
			}
		}
	}
	// 3.4 AP 桥归属变化 → 重启 AP（hostapd.conf 的 bridge= 变了）。
	// 注意：old 是变更前的配置快照（realize 被调用前 m.cfg 已换成 target）。
	if apSeg := target.LANForAP(); apSeg != nil {
		var oldBridge string
		if old != nil {
			if s := old.LANForAP(); s != nil {
				oldBridge = s.Bridge
			}
		}
		if oldBridge != apSeg.Bridge && m.apm.Status().State == ap.StateRunning && m.RestartAP != nil {
			m.log.Info("AP 桥归属变化，重启 AP", "old", oldBridge, "new", apSeg.Bridge)
			if err := m.RestartAP(); err != nil {
				m.log.Warn("AP 重启失败（桥变更后）", "err", err)
			}
		}
	}
	return nil
}

// startPPPoE 渲染 /etc/ppp/peers/apmanager 并托管 pppd（nodetach 由 supervisor 管理）
func (m *Manager) startPPPoE(p config.Port) error {
	if p.WAN.PPPoEUser == "" {
		return fmt.Errorf("PPPoE 缺少用户名")
	}
	peers := fmt.Sprintf(`# 由 apmanager 自动生成
plugin rp-pppoe.so
nic-%s
user "%s"
password "%s"
noipdefault
defaultroute      # 拨通后接管默认路由
persist           # 断线自动重拨
maxfail 0
holdoff 5
nodetach
`, p.Name, p.WAN.PPPoEUser, p.WAN.PPPoEPass)
	peersPath := "/etc/ppp/peers/apmanager"
	if err := priv.WriteFileAtomic(peersPath, []byte(peers), 0o600); err != nil {
		return err
	}
	// chap/pap 秘密走 peers 文件内联（0600），不写全局 secrets
	spec := &supervisor.Spec{
		Name:    "pppd",
		Argv:    []string{"pppd", "call", "apmanager"},
		LogRing: supervisor.NewRing(300),
	}
	return m.sup.Start(spec)
}

// stopPPPoE 停掉 pppd（WAN 模式切换/回滚时）
func (m *Manager) stopPPPoE() {
	m.sup.Stop("pppd", 5 * time.Second)
}

// Reconcile 对账：确保 WAN 口有地址（dhcp 租约掉了补拉）、桥和成员在位
func (m *Manager) Reconcile() {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	for _, p := range cfg.Ports {
		if p.Role != "wan" || p.WAN == nil || p.WAN.Mode != "dhcp" {
			continue
		}
		li, err := priv.LinkByName(p.Name)
		if err != nil || li.OperState != "up" {
			continue
		}
		if len(li.Addrs) == 0 {
			m.log.Info("对账：WAN 口无地址，重新 DHCP", "port", p.Name)
			go priv.Run(60*time.Second, "dhclient", "-1", "-q", p.Name)
		}
	}
}

func deepCopyPorts(in []config.Port) []config.Port {
	out := make([]config.Port, len(in))
	copy(out, in)
	return out
}

func deepCopyLAN(in []config.LANSeg) []config.LANSeg {
	out := make([]config.LANSeg, len(in))
	copy(out, in)
	return out
}

// PortsDesc 供日志/UI 的端口摘要
func PortsDesc(ports []config.Port) string {
	var parts []string
	for _, p := range ports {
		if p.Role == "unused" {
			continue
		}
		if p.Role == "lan" {
			parts = append(parts, fmt.Sprintf("%s→%s", p.Name, p.Bridge))
		} else {
			mode := ""
			if p.WAN != nil {
				mode = p.WAN.Mode
			}
			parts = append(parts, fmt.Sprintf("%s→WAN(%s)", p.Name, mode))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
