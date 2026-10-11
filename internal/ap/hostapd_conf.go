// hostapd_conf.go —— 把 config.AP 渲染成 hostapd.conf。关键项写清含义。
package ap

import (
	"fmt"
	"strings"

	"apmanager/internal/config"
)

// RenderHostapdConf 渲染 hostapd.conf 全文
// 带宽/信道换算：
//   2.4G 40MHz：channel 1–9 用 HT40+（扩展信道在上 ch+4），5–13 用 HT40-（扩展信道在下 ch-4）
//   5G 40MHz：ch%8==4 → HT40+，ch%8==0 → HT40-
//   5G 80MHz：vht_oper_chwidth=1，中心频率段 vht_oper_centr_freq_seg0_idx = ch+6（36..48 / 149..161 组）
// bridge 非空时 AP 帧直接进桥（hostapd 自动把无线口挂进桥，客户端与有线口同网段）
func RenderHostapdConf(apc config.AP, country, ctrlDir, bridge string) (string, error) {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("# 由 apmanager 自动生成，勿手改（改 /etc/apmanager/config.yaml）")
	p("interface=%s", apc.Interface)
	if bridge != "" {
		p("bridge=%s", bridge) // AP 帧直通网桥（与有线 LAN 同段）
	}
	p("driver=nl80211")
	p("ctrl_interface=%s", ctrlDir+"/hostapd") // Go 侧经此 unix socket 交互（PING/STATUS/STA/DEAUTH/事件）
	p("")
	p("ssid=%s", apc.SSID)
	if country != "" {
		// 国家码：决定合法信道与最大发射功率；ieee80211d 向客户端通告
		p("country_code=%s", country)
		p("ieee80211d=1")
	}

	band := apc.Band
	if band == "" {
		band = "2g"
	}
	hwMode := "g"
	if band == "5g" {
		hwMode = "a"
	}
	channel := apc.Channel
	p("hw_mode=%s", hwMode)

	bw := apc.Bandwidth
	if band == "2g" {
		p("ieee80211n=1") // 2.4G HT
		switch bw {
		case "40mhz":
			var ht string
			switch {
			case channel >= 1 && channel <= 9:
				ht = "HT40+"
			case channel >= 5 && channel <= 13:
				ht = "HT40-"
			default:
				ht = "HT40+"
			}
			p("ht_capab=[SHORT-GI-20][SHORT-GI-40][%s]", ht)
		default: // 20mhz
			p("ht_capab=[SHORT-GI-20]")
		}
		if channel == 0 {
			// ACS 自动选信道（channel=0 即触发）
			p("channel=0")
			p("acs_num_scans=3")
		} else {
			p("channel=%d", channel)
		}
	} else {
		// 5G：HT + VHT
		p("ieee80211n=1")
		p("ieee80211ac=1")
		var ht string
		if channel%8 == 4 {
			ht = "HT40+"
		} else if channel%8 == 0 {
			ht = "HT40-"
		}
		if bw == "40mhz" && ht != "" {
			p("ht_capab=[SHORT-GI-20][SHORT-GI-40][%s]", ht)
		} else {
			p("ht_capab=[SHORT-GI-20]")
		}
		if channel == 0 {
			p("channel=0")
			p("acs_num_scans=3")
			p("vht_oper_chwidth=0")
		} else {
			p("channel=%d", channel)
			switch bw {
			case "80mhz":
				// VHT80：主信道 + 中心频段索引（主信道所在 80MHz 块的中心）
				p("vht_oper_chwidth=1")
				p("vht_oper_centr_freq_seg0_idx=%d", channel+6)
				p("vht_capab=[SHORT-GI-80]")
			case "40mhz":
				p("vht_oper_chwidth=0")
			default:
				p("vht_oper_chwidth=0")
			}
		}
	}

	p("")
	p("wmm_enabled=1") // QoS，现代客户端默认期待
	p("auth_algs=1")   // 开放系统认证（WPA2/3 的 frame 交换仍走 open + 4 次握手）

	// 加密与密钥管理
	switch apc.Security {
	case "wpa3":
		p("wpa=2")
		p("wpa_key_mgmt=SAE")
		p("rsn_pairwise=CCMP")
		p("ieee80211w=2") // 强制 PMF（SAE 必需）
	case "wpa2wpa3":
		p("wpa=2")
		p("wpa_key_mgmt=WPA-PSK SAE")
		p("rsn_pairwise=CCMP")
		p("ieee80211w=1") // PMF 可选（兼顾老设备）
	default: // wpa2
		p("wpa=2")
		p("wpa_key_mgmt=WPA-PSK")
		p("rsn_pairwise=CCMP")
		p("ieee80211w=1")
	}
	p("wpa_passphrase=%s", apc.Password)

	if apc.Hidden {
		p("ignore_broadcast_ssid=1") // 隐藏 SSID（不广播 beacon 中的 SSID）
	}
	if apc.MaxClients > 0 {
		p("max_num_sta=%d", apc.MaxClients)
	}

	// MAC 准入：deny_mac_file（黑名单模式）。hostapd 支持运行期 DENY_MAC 增删，
	// 文件作为重启后的持久来源（devices 模块在拉黑时同步写这个文件）。
	denyFile := ctrlDir + "/deny_mac"
	p("deny_mac_file=%s", denyFile)

	// 日志走 stdout（supervisor 捕获进环形缓冲与日志页）
	p("logger_syslog=0")
	p("logger_stdout=-1")
	p("logger_stdout_level=2") // 0 最详 .. 4 仅错误；2=信息级
	return b.String(), nil
}
