// Package upnp UPnP IGD / NAT-PMP（miniupnpd 托管）：让内网设备（游戏机、
// BT 客户端等）自助打洞，改善 NAT 类型。参考 drouter：渲染原生 miniupnpd.conf
// 交由 supervisor 托管；miniupnpd 使用它自己的 nftables 表，与 apmanager 表互不干扰。
package upnp

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"apmanager/internal/priv"
	"apmanager/internal/supervisor"
)

// Installed miniupnpd 是否可用（未安装时 UI 给出安装提示而非报错）
func Installed() bool {
	_, err := exec.LookPath("miniupnpd")
	return err == nil
}

// RenderConf 渲染 miniupnpd.conf。
// extIface: WAN 口；lanCIDR: "192.168.50.1/24" 形态的 LAN 网关地址（可多个，每段一行）。
func RenderConf(extIface string, lanCIDRs []string, mgmtPort int) string {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	p("# 由 apmanager 自动生成，勿手改")
	p("ext_ifname=%s", extIface)
	for _, cidr := range lanCIDRs {
		p("listening_ip=%s", cidr)
	}
	p("port=5555")
	p("enable_upnp=yes")
	p("enable_natpmp=yes")
	p("secure_mode=yes")             // 仅允许向 LAN 内地址做重定向
	p("system=yes")                  // 使用系统防火墙后端（netfilter/nftables）
	p("presentation_url=http://%s:%d", lanGateway(lanCIDRs), mgmtPort)
	p("upnp_uuid=upnp-apmanager-0001")
	p("friendly_name=AP-Manager Router")
	return b.String()
}

func lanGateway(cidrs []string) string {
	if len(cidrs) == 0 {
		return "192.168.50.1"
	}
	return strings.SplitN(cidrs[0], "/", 2)[0]
}

// Reconcile 期望态对账：enabled 且已安装且存在 LAN 段 → 确保运行；
// 关闭/未安装/无 LAN → 停止。extIface 为空（无 WAN）时也停止。
func Reconcile(sup *supervisor.Supervisor, runDir string, enabled bool, extIface string, lanCIDRs []string, mgmtPort int, dev bool) {
	if !enabled || !Installed() || extIface == "" || len(lanCIDRs) == 0 {
		sup.Stop("miniupnpd", 4*time.Second)
		return
	}
	conf := RenderConf(extIface, lanCIDRs, mgmtPort)
	confPath := runDir + "/miniupnpd.conf"
	if err := priv.WriteFileAtomic(confPath, []byte(conf), 0o644); err != nil {
		return
	}
	if dev {
		return // 开发模式不真的拉起
	}
	_ = os.MkdirAll(runDir, 0o755)
	spec := &supervisor.Spec{
		Name:     "miniupnpd",
		Argv:     []string{"miniupnpd", "-d", "-c", confPath},
		LogRing:  supervisor.NewRing(200),
		FailLimit: 3,
	}
	_ = sup.Start(spec) // Start 幂等：配置变化重启即换 conf
}
