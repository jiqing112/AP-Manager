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
	rs, err := RenderNftFull(lans, "enx0", zones, "wlp1s0", []string{"192.168.60.66"}, true)
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
