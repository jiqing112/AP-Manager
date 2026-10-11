// wpasupplicant_conf.go —— 渲染 wpa_supplicant.conf。
package relay

import (
	"fmt"
	"strings"

	"apmanager/internal/config"
)

// RenderWpaConf 渲染 wpa_supplicant.conf：
// - 目标网络（当前要连的）priority 最高、放最前；
// - 已保存的其他上游按 priority 递减跟在后面（连接失败时自动切换）；
// - 密钥管理不写死，交 wpa_supplicant 与上游协商（PSK/SAE 自适应）。
func RenderWpaConf(relay config.Relay, targetSSID, targetPassword, runDir string) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("# 由 apmanager 自动生成，勿手改")
	p("ctrl_interface=%s/wpa_supplicant", runDir)
	p("eapol_version=1")
	p("ap_scan=1")        // 常规基础设施网络（AP 模式的上游）
	p("fast_reauth=1")    // EAP 快速重连
	p("")
	// 目标网络
	p("network={")
	p("\tssid=\"%s\"", targetSSID)
	p("\tpsk=\"%s\"", targetPassword)
	p("\tkey_mgmt=WPA-PSK WPA-PSK-SHA256 SAE") // 兼容 WPA2/WPA3 上游
	p("\tpriority=100")
	p("\tscan_ssid=1") // 也搜隐藏 SSID
	p("}")
	// 其他已存上游（不含当前目标）
	saved := append([]config.UpstreamNet{}, relay.Networks...)
	for i := range saved {
		if saved[i].SSID == targetSSID {
			saved = append(saved[:i], saved[i+1:]...)
			break
		}
	}
	for _, n := range saved {
		p("")
		p("network={")
		p("\tssid=\"%s\"", n.SSID)
		p("\tpsk=\"%s\"", n.Password)
		p("\tkey_mgmt=WPA-PSK WPA-PSK-SHA256 SAE")
		p("\tpriority=%d", n.Priority)
		p("\tscan_ssid=1")
		p("}")
	}
	return b.String()
}
