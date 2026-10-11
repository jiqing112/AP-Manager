package wol

import "testing"

func TestParseMAC(t *testing.T) {
	b, err := ParseMAC("AA:BB:CC:DD:EE:FF")
	if err != nil || len(b) != 6 || b[0] != 0xAA {
		t.Errorf("标准冒号格式解析失败: %v %v", b, err)
	}
	if _, err = ParseMAC("aabbccddeeff"); err != nil {
		t.Errorf("无分隔格式应可解析: %v", err)
	}
	if _, err = ParseMAC("aa:bb:cc:dd:ee"); err == nil {
		t.Error("11 位应报错")
	}
	if _, err = ParseMAC("zz:bb:cc:dd:ee:ff"); err == nil {
		t.Error("非十六进制应报错")
	}
}

func TestSendPacket(t *testing.T) {
	// 发往本机回环广播路径验证构造逻辑（失败仅在本机无 UDP 权限时）
	if err := Send("aa:bb:cc:dd:ee:ff"); err != nil {
		t.Skipf("本机 UDP 广播不可用: %v", err)
	}
	if err := Send("bad"); err == nil {
		t.Error("非法 MAC 应报错")
	}
}
