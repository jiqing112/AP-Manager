// Package config 负责 /etc/apmanager/config.yaml 的加载、校验、默认值与原子保存。
// 配置是"期望态"：守护进程负责把系统实际状态收敛到它。
package config

import (
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// 当前配置结构版本（未来字段迁移用）
const SchemaVersion = 1

// Config 顶层配置结构，字段与 PLAN §9.3 示例对应
type Config struct {
	Version   int             `yaml:"version"`
	Country   string          `yaml:"country"` // 国家码，影响合法信道与功率，默认 CN
	AP        AP              `yaml:"ap"`
	Relay     Relay           `yaml:"relay"`
	Upstreams Upstreams       `yaml:"upstreams"`
	Ports     []Port          `yaml:"ports"` // 每个物理口的角色
	LAN       []LANSeg        `yaml:"lan"`   // LAN 网段列表
	ZonePolicy map[string]string `yaml:"zone_policy"`
	Firewall  Firewall        `yaml:"firewall"` // WAN 侧防火墙：端口转发 / 开放端口 / DMZ
	DNS       DNSConfig       `yaml:"dns"`      // DNS 上游覆盖（空 = 自动跟随 WAN/系统）
	System    System          `yaml:"system"`   // 定时任务等系统级配置
	IPv6      IPv6Config      `yaml:"ipv6"`     // IPv6（默认关闭，需手动开启）
	Admin     Admin           `yaml:"admin"`

	// 网络模式（GL.iNet 式）：router=路由（默认）；ap=有线接入点（本机桥进上级网络，
	// 不做 NAT/DHCP）。wisp(无线扩展)=无线中继页实现；wds 不支持。
	NetworkMode string `yaml:"network_mode,omitempty" json:"networkMode,omitempty"` // router | ap
	// APWanPort 记忆 AP 模式下被收编的原 WAN 口，切回路由模式时恢复其角色
	APWanPort string `yaml:"ap_wan_port,omitempty" json:"apWanPort,omitempty"`
}

// IPv6Config IPv6 支持（默认关闭）。
// v1 形态：WAN 侧 SLAAC + DHCPv6 获取地址；LAN 侧 ULA 前缀 + dnsmasq RA/SLAAC
// 通告 + NAT66 兜底出网。PD 前缀委托列入后续版本。
type IPv6Config struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	ULAPrefix string `yaml:"ula_prefix" json:"ulaPrefix"` // fdxx::/48，空则自动生成并持久化
	Nat66     bool   `yaml:"nat66" json:"nat66"`          // ULA 经上行口 NAT66 出网（默认开）
}

// System 系统级定时任务
type System struct {
	RebootAt string `yaml:"reboot_at" json:"rebootAt"` // 每日定时重启 HH:MM，空 = 禁用
}

// DNSConfig DNS 上游：手动指定后 dnsmasq 按此转发；留空则自动从 WAN 口/系统解析配置继承
type DNSConfig struct {
	Upstream []string `yaml:"upstream" json:"upstream"`
}

// Firewall WAN 侧防火墙规则（GL.iNet 式三件套）。全部规则只在存在默认路由（WAN）时生效。
type Firewall struct {
	PortForwards []PortForward `yaml:"port_forwards" json:"portForwards"`
	OpenPorts    []OpenPort    `yaml:"open_ports" json:"openPorts"`
	DMZ          DMZ           `yaml:"dmz" json:"dmz"`
	UpnpEnabled  bool          `yaml:"upnp_enabled" json:"upnpEnabled"` // UPnP/NAT-PMP（miniupnpd）
}

// PortForward WAN 端口转发：WAN 口 proto wan_port → 内网 dest_ip:dest_port
type PortForward struct {
	Name     string `yaml:"name" json:"name"`
	Proto    string `yaml:"proto" json:"proto"` // tcp | udp | tcpudp
	WANPort  int    `yaml:"wan_port" json:"wanPort"`
	DestIP   string `yaml:"dest_ip" json:"destIP"`
	DestPort int    `yaml:"dest_port" json:"destPort"`
	Enabled  bool   `yaml:"enabled" json:"enabled"`
}

// OpenPort 在 WAN 侧放行本机端口（input）
type OpenPort struct {
	Name    string `yaml:"name" json:"name"`
	Proto   string `yaml:"proto" json:"proto"` // tcp | udp | tcpudp
	Port    int    `yaml:"port" json:"port"`
	Enabled bool   `yaml:"enabled" json:"enabled"`
}

// DMZ 主机：WAN 全端口转发到内网某台机器（与端口转发互斥使用，DMZ 优先级低——命中 DMZ 的前提是没有更早的 dnat 规则）
type DMZ struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	IP      string `yaml:"ip" json:"ip"`
}

// AP 无线发射配置
type AP struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	Interface     string `yaml:"interface" json:"interface"`
	SSID          string `yaml:"ssid" json:"ssid"`
	Password      string `yaml:"password" json:"password"`
	Security      string `yaml:"security" json:"security"` // wpa2 | wpa3 | wpa2wpa3
	Band          string `yaml:"band" json:"band"`         // 2g | 5g
	Channel       int    `yaml:"channel" json:"channel"`   // 0 = 自动(ACS)
	Bandwidth     string `yaml:"bandwidth" json:"bandwidth"` // 20mhz | 40mhz | 80mhz
	Hidden        bool   `yaml:"hidden" json:"hidden"`
	MaxClients    int    `yaml:"max_clients" json:"maxClients"`
	FollowUpstreamChannel bool `yaml:"follow_upstream_channel" json:"followUpstreamChannel"` // 中继态把 AP 信道对齐上游
}

// Relay 无线中继（STA）配置
type Relay struct {
	Enabled  bool       `yaml:"enabled" json:"enabled"`
	Interface string    `yaml:"interface" json:"interface"` // STA 所用接口（可与 AP 同卡）
	Current  string     `yaml:"current" json:"current"`     // 当前连接的上游 SSID
	Networks []UpstreamNet `yaml:"networks" json:"networks"`
	Reconnect RelayReconnect `yaml:"reconnect" json:"reconnect"`
}

// UpstreamNet 已保存的上游 WiFi，按 Priority 升序尝试
type UpstreamNet struct {
	SSID     string `yaml:"ssid" json:"ssid"`
	Password string `yaml:"password" json:"password"`
	KeyMgmt  string `yaml:"key_mgmt" json:"keyMgmt"` // WPA-PSK | SAE | WPA-PSK SAE（空则自动）
	Priority int    `yaml:"priority" json:"priority"`
}

// RelayReconnect 重连策略
type RelayReconnect struct {
	BackoffMaxSeconds int `yaml:"backoff_max_seconds" json:"backoffMaxSeconds"` // 指数退避上限
	RescanAfter       int `yaml:"rescan_after" json:"rescanAfter"`              // 连续失败 N 次后重扫并切换次优先级
}

// Upstreams 上游选择与探测
type Upstreams struct {
	Priority     []string     `yaml:"priority"` // ["eth-wan","wifi-relay"]
	Check        CheckPolicy  `yaml:"check"`
}

// CheckPolicy 健康探测参数
type CheckPolicy struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	FailThreshold   int `yaml:"fail_threshold"`
}

// Port 物理网口角色
type Port struct {
	Name    string   `yaml:"name" json:"name"`
	Role    string   `yaml:"role" json:"role"` // wan | lan | unused
	Bridge  string   `yaml:"bridge,omitempty" json:"bridge,omitempty"` // lan 角色时挂的桥
	WAN     *WANSetting `yaml:"wan,omitempty" json:"wan,omitempty"`
}

// WANSetting WAN 接入方式
type WANSetting struct {
	Mode     string `yaml:"mode" json:"mode"` // dhcp | static | pppoe
	Address  string `yaml:"address,omitempty" json:"address,omitempty"` // 静态 IP/前缀
	Gateway  string `yaml:"gateway,omitempty" json:"gateway,omitempty"`
	DNS      []string `yaml:"dns,omitempty" json:"dns,omitempty"`
	PPPoEUser string `yaml:"pppoe_user,omitempty" json:"pppoe_user,omitempty"`
	PPPoEPass string `yaml:"pppoe_pass,omitempty" json:"pppoe_pass,omitempty"`
}

// LANSeg 一个 LAN 网段（一桥一子网一池）
type LANSeg struct {
	Name      string `yaml:"name" json:"name"`
	Bridge    string `yaml:"bridge" json:"bridge"` // 为空表示直接用 AP 无线接口，不建桥（阶段一形态）
	Subnet    string `yaml:"subnet" json:"subnet"` // 如 192.168.50.1/24，地址配在桥/接口上
	DHCPPool  [2]string `yaml:"dhcp" json:"dhcpPool"` // 起止地址
	LeaseHours int    `yaml:"lease_hours" json:"leaseHours"`
	Isolation bool   `yaml:"isolation" json:"isolation"` // 访客等：子网内客户端互访隔离
	AP        bool   `yaml:"ap" json:"ap"` // AP 的 BSS 挂在这个网段
}

// Admin Web 管理界面
type Admin struct {
	Port     int    `yaml:"port"`
	Bind     string `yaml:"bind"` // 监听地址；开发期 127.0.0.1
}

// Default 返回适合本机（wlp1s0 + enx 上行）的默认配置。
// 上行口 enx00e04c4b4d10 标 unused：开发期只读，不动它（安全红线）。
func Default() *Config {
	return &Config{
		Version: SchemaVersion,
		Country: "CN",
		AP: AP{
			Enabled:   false, // 先由用户在 UI 里显式开启，避免意外占卡
			Interface: "wlp1s0",
			SSID:      "AP-Manager",
			Password:  "apmanager123",
			Security:  "wpa2",
			Band:      "2g",
			Channel:   6,
			Bandwidth: "20mhz",
			Hidden:    false,
			MaxClients: 32,
			FollowUpstreamChannel: true,
		},
		Relay: Relay{
			Interface: "wlp1s0",
			Reconnect: RelayReconnect{BackoffMaxSeconds: 30, RescanAfter: 3},
		},
		Upstreams: Upstreams{
			Priority: []string{"eth-wan", "wifi-relay"},
			Check:    CheckPolicy{IntervalSeconds: 5, FailThreshold: 3},
		},
		Ports: []Port{
			{Name: "enx00e04c4b4d10", Role: "unused"}, // 红线口：默认不管理
		},
		LAN: []LANSeg{
			{
				Name:  "home",
				Bridge: "", // 阶段一：地址直接配在无线口上；阶段三引入 br-lan
				Subnet: "192.168.50.1/24",
				DHCPPool: [2]string{"192.168.50.100", "192.168.50.200"},
				LeaseHours: 12,
				AP: true,
			},
		},
		ZonePolicy: map[string]string{
			"guest_to_lan": "deny",
			"iot_to_lan":   "deny",
			"lan_to_lan":   "allow",
		},
		IPv6:      IPv6Config{Enabled: false, Nat66: true}, // 默认关闭，ULA 由 fillDefaults 生成
		Admin: Admin{Port: 8080, Bind: "127.0.0.1"},
	}
}

// Load 从 path 读取配置；不存在则写入默认配置并返回之
func Load(path string) (*Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := c.Save(path); err != nil {
			return nil, fmt.Errorf("写入默认配置: %w", err)
		}
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	c.fillDefaults()
	return c, nil
}

// fillDefaults 补齐旧配置缺失的零值字段（版本演进时在这里迁移）
func (c *Config) fillDefaults() {
	d := Default()
	if c.Version == 0 {
		c.Version = SchemaVersion
	}
	if c.Country == "" {
		c.Country = d.Country
	}
	if c.AP.Interface == "" {
		c.AP.Interface = d.AP.Interface
	}
	if c.AP.SSID == "" {
		c.AP.SSID = d.AP.SSID
	}
	if c.AP.Security == "" {
		c.AP.Security = d.AP.Security
	}
	if c.AP.Band == "" {
		c.AP.Band = d.AP.Band
	}
	if c.AP.Bandwidth == "" {
		c.AP.Bandwidth = d.AP.Bandwidth
	}
	if c.AP.MaxClients == 0 {
		c.AP.MaxClients = d.AP.MaxClients
	}
	if c.Relay.Interface == "" {
		c.Relay.Interface = d.Relay.Interface
	}
	if c.Relay.Reconnect.BackoffMaxSeconds == 0 {
		c.Relay.Reconnect = d.Relay.Reconnect
	}
	if len(c.Upstreams.Priority) == 0 {
		c.Upstreams.Priority = d.Upstreams.Priority
	}
	if c.Upstreams.Check.IntervalSeconds == 0 {
		c.Upstreams.Check = d.Upstreams.Check
	}
	if len(c.LAN) == 0 {
		c.LAN = d.LAN
	}
	if c.Admin.Port == 0 {
		c.Admin.Port = d.Admin.Port
	}
	if c.IPv6.ULAPrefix == "" {
		c.IPv6.ULAPrefix = genULA()
	}
	if c.Admin.Bind == "" {
		c.Admin.Bind = d.Admin.Bind
	}
}

// Validate 校验配置合法性，返回用户可读的错误列表
func (c *Config) Validate() []string {
	var errs []string
	if c.AP.Enabled {
		switch c.AP.Security {
		case "wpa2", "wpa3", "wpa2wpa3":
		default:
			errs = append(errs, "ap.security 必须是 wpa2/wpa3/wpa2wpa3")
		}
		if len(c.AP.Password) < 8 || len(c.AP.Password) > 63 {
			errs = append(errs, "ap.password 长度须在 8–63 之间")
		}
		switch c.AP.Band {
		case "2g", "5g":
		default:
			errs = append(errs, "ap.band 必须是 2g 或 5g")
		}
		if c.AP.MaxClients < 1 || c.AP.MaxClients > 64 {
			errs = append(errs, "ap.max_clients 须在 1–64 之间")
		}
		if c.AP.Band == "5g" && c.AP.Bandwidth == "40mhz" {
			// 5G 上 40MHz 合法，无操作；占位避免误报
			_ = 0
		}
	}
	for i, seg := range c.LAN {
		if seg.Subnet == "" {
			errs = append(errs, fmt.Sprintf("lan[%d].subnet 不能为空", i))
		}
	}
	errs = append(errs, c.Firewall.Validate()...)
	for i, srv := range c.DNS.Upstream {
		if net.ParseIP(srv) == nil {
			errs = append(errs, fmt.Sprintf("dns.upstream[%d] %q 不是合法的 IP 地址", i, srv))
		}
	}
	if c.IPv6.ULAPrefix != "" {
		if _, _, err := net.ParseCIDR(c.IPv6.ULAPrefix); err != nil || !strings.HasPrefix(c.IPv6.ULAPrefix, "fd") {
			errs = append(errs, "ipv6.ula_prefix 应为 fd 开头的 /48 前缀（如 fd52:3a91::/48）")
		}
	}
	if c.System.RebootAt != "" {
		if len(c.System.RebootAt) != 5 || c.System.RebootAt[2] != ':' {
			errs = append(errs, "system.reboot_at 格式应为 HH:MM，留空禁用")
		} else {
			hh, mm := c.System.RebootAt[:2], c.System.RebootAt[3:]
			if hh < "00" || hh > "23" || mm < "00" || mm > "59" {
				errs = append(errs, "system.reboot_at 时间非法")
			}
		}
	}
	if c.Admin.Port < 1 || c.Admin.Port > 65535 {
		errs = append(errs, "admin.port 非法")
	}
	return errs
}

// Validate 防火墙规则合法性（返回带规则定位的错误文案）
func (f *Firewall) Validate() []string {
	var errs []string
	validProto := func(p string) bool { return p == "tcp" || p == "udp" || p == "tcpudp" }
	validPort := func(n int) bool { return n >= 1 && n <= 65535 }
	for i, r := range f.PortForwards {
		where := fmt.Sprintf("port_forwards[%d]", i)
		if !validProto(r.Proto) {
			errs = append(errs, where+".proto 必须是 tcp/udp/tcpudp")
		}
		if !validPort(r.WANPort) {
			errs = append(errs, where+".wan_port 非法（1–65535）")
		}
		if !validPort(r.DestPort) {
			errs = append(errs, where+".dest_port 非法（1–65535）")
		}
		if ip := net.ParseIP(r.DestIP); ip == nil {
			errs = append(errs, where+".dest_ip 不是合法 IPv4/IPv6 地址")
		} else if ip.To4() == nil {
			errs = append(errs, where+".dest_ip 暂只支持 IPv4")
		}
	}
	for i, r := range f.OpenPorts {
		where := fmt.Sprintf("open_ports[%d]", i)
		if !validProto(r.Proto) {
			errs = append(errs, where+".proto 必须是 tcp/udp/tcpudp")
		}
		if !validPort(r.Port) {
			errs = append(errs, where+".port 非法（1–65535）")
		}
	}
	if ip := net.ParseIP(f.DMZ.IP); f.DMZ.Enabled && (ip == nil || ip.To4() == nil) {
		errs = append(errs, "dmz.ip 必须是合法的 IPv4 地址")
	}
	return errs
}

// Save 原子写入：先写临时文件再 rename，避免断电产生半截配置
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// genULA 生成随机 ULA /48 前缀（fd00::/8 内，RFC 4193：40 位随机全局 ID）
func genULA() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return fmt.Sprintf("fd%02x:%02x%02x::/48", b[0]&0x3F, b[1], b[2])
}

// LANForAP 返回 AP 挂的网段（找不到返回 nil）
func (c *Config) LANForAP() *LANSeg {
	for i := range c.LAN {
		if c.LAN[i].AP {
			return &c.LAN[i]
		}
	}
	return nil
}
