// dev.go —— dev 模式的假数据：让 UI 在没有网卡/无 root 的环境（如 Windows）也能开发调试。
// 真机上也可用 --dev 跑，行为 = 完全不动系统、只吐模拟数据。
package priv

import (
	"fmt"
	"strings"
)

func devLinks() []LinkInfo {
	return []LinkInfo{
		{Name: "lo", MAC: "00:00:00:00:00:00", Type: "loopback", OperState: "up", MTU: 65536, Addrs: []string{"127.0.0.1/8"}},
		{Name: "eth0", MAC: "aa:bb:cc:00:00:01", Type: "device", OperState: "up", MTU: 1500,
			Addrs: []string{"192.168.68.28/24"}, Stats: LinkStats{RxBytes: 8_000_000, TxBytes: 1_000_000, RxPackets: 9000, TxPackets: 5000}},
		{Name: "wlan0", MAC: "24:b7:2a:20:bb:01", Type: "device", IsWireless: true, OperState: "up", MTU: 1500,
			Addrs: []string{"192.168.50.1/24"}, Stats: LinkStats{RxBytes: 2_000_000, TxBytes: 9_000_000, RxPackets: 6000, TxPackets: 12000}},
	}
}

func devExec(name string, args []string) string {
	switch name {
	case "iw":
		if len(args) >= 1 && args[0] == "list" {
			return devIwList()
		}
	}
	return ""
}

// devIwList 模拟 iw list 的关键片段（能力探测解析器的输入）
func devIwList() string {
	var b strings.Builder
	b.WriteString("Wiphy phy0\n")
	b.WriteString("\tSupported interface modes:\n")
	b.WriteString("\t\t * managed\n\t\t * AP\n\t\t * AP/VLAN\n")
	b.WriteString("\tBand 1:\n")
	b.WriteString("\t\tFrequencies:\n")
	for _, ch := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13} {
		fmt.Fprintf(&b, "\t\t\t* %.1f MHz [%d] (20.0 dBm)\n", 2407.0+float64(ch)*5, ch)
	}
	b.WriteString("\tBand 2:\n")
	b.WriteString("\t\tFrequencies:\n")
	for _, ch := range []int{36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165} {
		fmt.Fprintf(&b, "\t\t\t* %.1f MHz [%d] (20.0 dBm)\n", 5000.0+float64(ch)*5, ch)
	}
	b.WriteString("\tvalid interface combinations:\n")
	b.WriteString("\t\t * #{ AP } <= 1, #{ managed } <= 1,\n")
	b.WriteString("\t\t   total <= 2, #channels <= 1, STA/AP BI must match\n")
	return b.String()
}
