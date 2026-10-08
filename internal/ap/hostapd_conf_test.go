package ap

import (
	"strings"
	"testing"

	"apmanager/internal/config"
)

// TestRenderHostapdConf_2gWpa2 2.4G WPA2 基本形态
func TestRenderHostapdConf_2gWpa2(t *testing.T) {
	apc := config.AP{
		Interface: "wlp1s0", SSID: "Home", Password: "12345678",
		Security: "wpa2", Band: "2g", Channel: 6, Bandwidth: "20mhz", MaxClients: 16,
	}
	conf, err := RenderHostapdConf(apc, "CN", "/run/apm", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"interface=wlp1s0",
		"ssid=Home",
		"hw_mode=g",
		"channel=6",
		"wpa=2",
		"wpa_key_mgmt=WPA-PSK",
		"rsn_pairwise=CCMP",
		"ieee80211w=1",
		"wpa_passphrase=12345678",
		"max_num_sta=16",
		"country_code=CN",
		"deny_mac_file=/run/apm/deny_mac",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("缺少 %q\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "bridge=") {
		t.Error("无桥形态不应输出 bridge=")
	}
	if strings.Contains(conf, "ignore_broadcast_ssid=1") {
		t.Error("未隐藏 SSID 却输出 ignore_broadcast_ssid=1")
	}
}

// TestRenderHostapdConf_5g80MHz 5G 80MHz：VHT 中心信道 = 主信道 + 6
func TestRenderHostapdConf_5g80MHz(t *testing.T) {
	apc := config.AP{
		Interface: "wlp1s0", SSID: "Home5G", Password: "12345678",
		Security: "wpa2wpa3", Band: "5g", Channel: 36, Bandwidth: "80mhz", Hidden: true,
	}
	conf, _ := RenderHostapdConf(apc, "CN", "/run/apm", "")
	for _, want := range []string{
		"hw_mode=a",
		"ieee80211ac=1",
		"channel=36",
		"vht_oper_chwidth=1",
		"vht_oper_centr_freq_seg0_idx=42", // 36 + 6
		"wpa_key_mgmt=WPA-PSK SAE",
		"ieee80211w=1", // 混合模式 PMF 可选
		"ignore_broadcast_ssid=1",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("缺少 %q\n%s", want, conf)
		}
	}
}

// TestRenderHostapdConf_Wpa3 WPA3-SAE：强制 PMF
func TestRenderHostapdConf_Wpa3(t *testing.T) {
	apc := config.AP{Interface: "w", SSID: "S", Password: "12345678", Security: "wpa3", Band: "2g", Channel: 1}
	conf, _ := RenderHostapdConf(apc, "CN", "/run/apm", "")
	if !strings.Contains(conf, "wpa_key_mgmt=SAE") || !strings.Contains(conf, "ieee80211w=2") {
		t.Errorf("WPA3 应为 SAE + 强制 PMF:\n%s", conf)
	}
}

// TestRenderHostapdConf_Bridge 桥形态
func TestRenderHostapdConf_Bridge(t *testing.T) {
	apc := config.AP{Interface: "w", SSID: "S", Password: "12345678", Security: "wpa2", Band: "2g", Channel: 1}
	conf, _ := RenderHostapdConf(apc, "CN", "/run/apm", "br-lan")
	if !strings.Contains(conf, "bridge=br-lan") {
		t.Errorf("桥形态应输出 bridge=br-lan:\n%s", conf)
	}
}

// TestRenderHostapdConf_2g40MHz 2.4G 40MHz 上下扩展信道
func TestRenderHostapdConf_2g40MHz(t *testing.T) {
	apc := config.AP{Interface: "w", SSID: "S", Password: "12345678", Security: "wpa2", Band: "2g", Channel: 3, Bandwidth: "40mhz"}
	conf, _ := RenderHostapdConf(apc, "CN", "/run/apm", "")
	if !strings.Contains(conf, "HT40+") {
		t.Errorf("信道 3 的 40MHz 应为 HT40+:\n%s", conf)
	}
	apc.Channel = 11
	conf, _ = RenderHostapdConf(apc, "CN", "/run/apm", "")
	if !strings.Contains(conf, "HT40-") {
		t.Errorf("信道 11 的 40MHz 应为 HT40-:\n%s", conf)
	}
}
