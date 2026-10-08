//go:build !linux

// netlink_stub.go —— 非 Linux 平台的占位实现（Windows 上仅配合 --dev 做 UI 开发）
package priv

import (
	"errors"
	"net"
)

var errNotLinux = errors.New("netlink 操作仅支持 Linux（请用 --dev 模式开发 UI）")

func AllLinks() ([]LinkInfo, error) { return devLinks(), nil }
func LinkByName(name string) (LinkInfo, error) {
	for _, l := range devLinks() {
		if l.Name == name {
			return l, nil
		}
	}
	return LinkInfo{}, errors.New("接口不存在")
}
func SetUp(string) error        { return nil }
func SetDown(string) error      { return nil }
func AddrReplace(_, _ string) error { return nil }
func AddrFlush(string) error    { return nil }
func BridgeCreate(string) error { return nil }
func BridgeDelete(string) error { return nil }
func BridgeAddPort(_, _ string) error { return nil }
func BridgeDelPort(string) error { return nil }
func DefaultRoute4() (DefaultRoute, error) {
	return DefaultRoute{Iface: "eth-mock", GW: net.ParseIP("192.168.68.1")}, nil
}
func NeighDump() ([]NeighEntry, error) { return nil, nil }
func EnableIPForward() error           { return nil }
func DetectHostapdVersion() int        { return 210 }
