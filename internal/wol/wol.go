// Package wol 局域网唤醒（Wake-on-LAN）：向目标 MAC 发送魔术包。
// 纯 Go 实现（UDP 广播），同时向全网广播与定向子网广播投递，提高唤醒成功率。
// 参考 drouter 的 WOL：设备列表里的已知设备（DHCP 租约）一键唤醒。
package wol

import (
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"
)

// ParseMAC 校验并归一化 MAC（支持 : - 空格分隔）
func ParseMAC(s string) ([]byte, error) {
	s = strings.NewReplacer(":", "", "-", "", " ", "").Replace(strings.TrimSpace(s))
	if len(s) != 12 {
		return nil, fmt.Errorf("MAC 格式应为 12 位十六进制（如 aa:bb:cc:dd:ee:ff）")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("MAC 含非十六进制字符")
	}
	return b, nil
}

// Send 发送魔术包：6 字节 0xFF + 16 遍目标 MAC。
// 对 255.255.255.255 的 9/7 端口各广播一次；subnetBcast 形如 192.168.50.255
// 时额外定向广播（跨 VLAN 场景全网广播可能被网关过滤）。
func Send(mac string, subnetBcasts ...string) error {
	target, err := ParseMAC(mac)
	if err != nil {
		return err
	}
	packet := make([]byte, 0, 6+16*6)
	packet = append(packet, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	for i := 0; i < 16; i++ {
		packet = append(packet, target...)
	}

	addrs := []string{"255.255.255.255:9", "255.255.255.255:7"}
	for _, b := range subnetBcasts {
		if ip := net.ParseIP(b); ip != nil {
			addrs = append(addrs, b+":9")
		}
	}

	var errs []string
	sent := 0
	for _, addr := range addrs {
		conn, err := net.DialTimeout("udp", addr, 2*time.Second)
		if err != nil {
			errs = append(errs, addr+": "+err.Error())
			continue
		}
		_, err = conn.Write(packet)
		conn.Close()
		if err != nil {
			errs = append(errs, addr+": "+err.Error())
			continue
		}
		sent++
	}
	if sent == 0 {
		return fmt.Errorf("魔术包发送失败: %s", strings.Join(errs, "; "))
	}
	return nil
}
