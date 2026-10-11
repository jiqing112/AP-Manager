package netrt

import (
	"strings"
	"testing"

	"apmanager/internal/config"
)

// TestRenderDnsmasq 段渲染 + 掩码 + 网关下发 + 上行排除
func TestRenderDnsmasq(t *testing.T) {
	lans := []config.LANSeg{
		{Name: "home", Bridge: "br-lan", Subnet: "192.168.50.1/24",
			DHCPPool: [2]string{"192.168.50.100", "192.168.50.200"}, LeaseHours: 12, AP: true},
	}
	conf := RenderDnsmasq(lans, []string{"223.5.5.5"}, Paths{RunDir: "/run", StateDir: "/var/lib"}, []string{"aa:bb:cc:dd:ee:ff"})
	for _, want := range []string{
		"dhcp-range=192.168.50.100,192.168.50.200,255.255.255.0,12h", // 掩码必须是点分而非 ffffff00
		"dhcp-option=3,192.168.50.1",  // 网关 = 段主机地址而非网络地址
		"dhcp-option=6,192.168.50.1",  // DNS 指向本机
		"interface=br-lan",
		"except-interface=lo",
		"dhcp-host=aa:bb:cc:dd:ee:ff,ignore",
		"server=223.5.5.5",
		"address=/apmanager.lan/192.168.50.1",
		"dhcp-leasefile=/var/lib/dnsmasq.leases",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("缺少 %q\n%s", want, conf)
		}
	}
}

// TestRenderNft 隔离矩阵：段内隔离、跨段默认 deny、上网放行、masquerade
func TestRenderNft(t *testing.T) {
	lans := []config.LANSeg{
		{Name: "home", Bridge: "", Subnet: "192.168.50.1/24", AP: true},
		{Name: "guest", Bridge: "br-guest", Subnet: "192.168.60.1/24", Isolation: true},
	}
	zones := map[string]string{"guest_to_lan": "deny", "lan_to_lan": "allow"}
	rs, err := RenderNftFull(lans, "enx0", zones, "wlp1s0", []string{"192.168.60.66"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set dev_up { type ipv4_addr; flags dynamic; }",           // 计数集合
		"update @dev_up { ip saddr counter }",                     // 计数规则
		`ip saddr 192.168.60.66 drop comment "黑名单"`,             // 黑名单双向
		`ip saddr 192.168.50.0/24 ip daddr 192.168.50.0/24 accept`, // 段内互访（home 非隔离）
		`ip saddr 192.168.60.0/24 ip daddr 192.168.60.0/24 drop`,   // guest 段内隔离
		`iifname "br-guest" oifname "wlp1s0" drop`,                 // 跨段默认隔离
		`iifname "wlp1s0" oifname "enx0" accept`,                   // home 上网
		`ip saddr 192.168.50.0/24 oifname "enx0" masquerade`,       // NAT
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("缺少 %q\n%s", want, rs)
		}
	}
}

// TestSubnetHelpers CIDR 规整助手
func TestSubnetHelpers(t *testing.T) {
	if got := subnetOf("192.168.50.1/24"); got != "192.168.50.0/24" {
		t.Errorf("subnetOf = %q", got)
	}
	if got := lanGatewayIP("10.1.2.3/16"); got != "10.1.2.3" {
		t.Errorf("lanGatewayIP = %q", got)
	}
	if got := netmaskOf("192.168.50.1/24"); got != "255.255.255.0" {
		t.Errorf("netmaskOf = %q（不能是十六进制）", got)
	}
}

// TestRenderNftFirewall 防火墙规则：端口转发 dnat + forward 放行、tcpudp 展开、
// 停用规则不渲染、DMZ、开放端口 input 链；无上行口时不渲染任何防火墙链
func TestRenderNftFirewall(t *testing.T) {
	lans := []config.LANSeg{
		{Name: "home", Bridge: "br-lan", Subnet: "192.168.50.1/24", AP: false},
	}
	fw := &config.Firewall{
		PortForwards: []config.PortForward{
			{Name: "nas", Proto: "tcp", WANPort: 8080, DestIP: "192.168.50.10", DestPort: 80, Enabled: true},
			{Name: "game", Proto: "tcpudp", WANPort: 7777, DestIP: "192.168.50.20", DestPort: 7777, Enabled: true},
			{Name: "off", Proto: "tcp", WANPort: 1234, DestIP: "192.168.50.30", DestPort: 1234, Enabled: false},
		},
		OpenPorts: []config.OpenPort{
			{Name: "wg", Proto: "udp", Port: 51820, Enabled: true},
		},
		DMZ: config.DMZ{},
	}
	rs, err := RenderNftFull(lans, "enx0", map[string]string{}, "wlp1s0", nil, false, fw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`chain prerouting {`,
		`iifname "enx0" tcp dport 8080 dnat ip to 192.168.50.10:80 comment "端口转发 nas"`,
		`iifname "enx0" tcp dport 7777 dnat ip to 192.168.50.20:7777`,  // tcpudp → tcp
		`iifname "enx0" udp dport 7777 dnat ip to 192.168.50.20:7777`,  // tcpudp → udp
		`iifname "enx0" ip daddr 192.168.50.10 tcp dport 80 accept`,    // forward 放行在段间 drop 之前
		`chain input {`,
		`iifname "enx0" udp dport 51820 accept`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("缺少 %q\n%s", want, rs)
		}
	}
	if strings.Contains(rs, "1234") {
		t.Errorf("停用的规则不应渲染\n%s", rs)
	}
	if strings.Contains(rs, "DMZ 主机") {
		t.Errorf("DMZ 关闭时不应渲染\n%s", rs)
	}
}

// TestRenderNftNoWANNoFW 无上行口（纯 AP 无 WAN）时不渲染 prerouting/input 链
func TestRenderNftNoWANNoFW(t *testing.T) {
	lans := []config.LANSeg{
		{Name: "home", Bridge: "", Subnet: "192.168.50.1/24", AP: true},
	}
	fw := &config.Firewall{
		PortForwards: []config.PortForward{{Name: "x", Proto: "tcp", WANPort: 80, DestIP: "192.168.50.9", DestPort: 80, Enabled: true}},
	}
	rs, err := RenderNftFull(lans, "", map[string]string{}, "wlp1s0", nil, false, fw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rs, "prerouting") || strings.Contains(rs, "dnat") {
		t.Errorf("无上行口不应渲染 dnat\n%s", rs)
	}
}

// TestFirewallValidate 配置校验
func TestFirewallValidate(t *testing.T) {
	fw := &config.Firewall{
		PortForwards: []config.PortForward{
			{Proto: "sctp", WANPort: 0, DestIP: "not-ip", DestPort: 80, Enabled: true},
		},
		DMZ: config.DMZ{Enabled: true, IP: "192.168.50"},
	}
	errs := fw.Validate()
	if len(errs) < 4 {
		t.Errorf("应报出 proto/wan_port/dest_ip/dmz 多项错误，实际 %v", errs)
	}
	ok := &config.Firewall{
		PortForwards: []config.PortForward{{Name: "ok", Proto: "tcpudp", WANPort: 1, DestIP: "10.0.0.1", DestPort: 65535, Enabled: true}},
	}
	if errs := ok.Validate(); len(errs) != 0 {
		t.Errorf("合法配置不应报错: %v", errs)
	}
}

// TestFirewallActive 纯 WAN 形态渲染条件
func TestFirewallActive(t *testing.T) {
	if firewallActive(&config.Firewall{}) {
		t.Error("空防火墙不应视为 active")
	}
	if !firewallActive(&config.Firewall{DMZ: config.DMZ{Enabled: true}}) {
		t.Error("DMZ 开启应视为 active")
	}
	if !firewallActive(&config.Firewall{OpenPorts: []config.OpenPort{{Port: 22, Enabled: true}}}) {
		t.Error("启用中的开放端口应视为 active")
	}
}
