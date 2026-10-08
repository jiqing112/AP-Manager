// apmanager 入口：flag 解析、模块装配、HTTP 服务与优雅退出。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"apmanager/internal/api"
	"apmanager/internal/ap"
	"apmanager/internal/bus"
	"apmanager/internal/config"
	"apmanager/internal/devices"
	"apmanager/internal/ethport"
	"apmanager/internal/iface"
	"apmanager/internal/netrt"
	"apmanager/internal/priv"
	"apmanager/internal/relay"
	"apmanager/internal/supervisor"
	"apmanager/internal/system"
	"apmanager/web"
)

func main() {
	var (
		cfgPath = flag.String("config", "/etc/apmanager/config.yaml", "配置文件路径")
		stateDir = flag.String("state", "/var/lib/apmanager", "状态与运行时目录")
		logPath  = flag.String("log", "/var/log/apmanager/app.log", "日志文件")
		addr     = flag.String("addr", "", "HTTP 监听地址（默认取配置 admin.bind:port）")
		dev      = flag.Bool("dev", false, "开发模式：假数据驱动 UI，不动系统")
		dryRun   = flag.Bool("dry-run", false, "演练模式：特权命令只打印不执行")
		showVer  = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("apmanager 1.0.0")
		return
	}

	if *dev {
		priv.Mode = "dev"
		*cfgPath = filepath.Join(*stateDir, "config.yaml")
	}
	priv.DryRun = *dryRun

	// root 检查（dev 模式豁免）
	if os.Geteuid() != 0 && !*dev {
		fmt.Fprintln(os.Stderr, "需要 root 运行（hostapd/nft/netlink 权限）；开发 UI 请用 --dev")
		os.Exit(1)
	}

	// 日志链：stdout + 轮转文件 + 环形缓冲
	ring, logger, err := system.SetupLogging(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化日志失败:", err)
		os.Exit(1)
	}
	priv.SetLogger(logger)

	// 目录：run/、state/
	runDir := filepath.Join(*stateDir, "run")
	for _, d := range []string{runDir, filepath.Dir(*logPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			logger.Error("创建目录失败", "dir", d, "err", err)
			os.Exit(1)
		}
	}

	// 配置与状态
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	sys, err := system.New(filepath.Join(*stateDir, "state.json"), logger)
	if err != nil {
		logger.Error("初始化状态失败", "err", err)
		os.Exit(1)
	}

	// 总线与基础模块
	evbus := bus.New()
	sup := supervisor.New(logger)
	ifaces := iface.New(evbus, logger)

	// 路径注入
	paths := ap.Paths{RunDir: runDir}
	npaths := netrt.Paths{RunDir: runDir, StateDir: *stateDir}

	apm := ap.New(cfg, sup, ifaces, evbus, logger, paths)
	netrtm := netrt.New(cfg, sup, evbus, logger, npaths)
	relm := relay.New(cfg, *cfgPath, sup, ifaces, apm, evbus, logger, runDir)
	ethm := ethport.New(cfg, *cfgPath, ifaces, apm, sup, evbus, logger)
	// afterNetRefresh：网络面刷新后的附加动作（限速树重建），devs 创建后赋值
	var afterNetRefresh func()
	refreshNet := func() {
		apRunning := apm.Status().State == ap.StateRunning
		netrtm.Refresh(apRunning)
		// AP 起来后确保 LAN 地址在接口上（阶段一无桥：直接配无线口）
		if apRunning {
			if seg := cfg.LANForAP(); seg != nil && seg.Bridge == "" {
				if err := priv.AddrReplace(cfg.AP.Interface, seg.Subnet); err != nil {
					logger.Warn("配置 LAN 地址失败", "iface", cfg.AP.Interface, "err", err)
				}
			}
		}
		if afterNetRefresh != nil {
			afterNetRefresh()
		}
	}
	apm.OnNetworkChanged = refreshNet
	netrtm.RefreshFn = refreshNet
	relm.OnNetworkChanged = refreshNet
	relm.StopAP = func() error { return apm.Stop() }
	apm.StopRelay = relm.Disconnect
	ethm.OnNetworkChanged = refreshNet
	ethm.RestartAP = func() error { return apm.Restart() }
	netrtm.RelayIfaceFn = func(name string) bool {
		return relm.IfaceName() == name && relm.IsConnected()
	}
	devs := devices.New(sys, apm, cfg, evbus, logger, func() []netrt.Lease {
		return netrt.ReadLeases(netrtm.LeasesPath())
	}, func() map[string]bool {
		// LAN 侧接口：AP 直挂口 + LAN 桥（阶段三加入 eth LAN 口）
		set := map[string]bool{}
		if seg := cfg.LANForAP(); seg != nil {
			if seg.Bridge != "" {
				set[seg.Bridge] = true
			} else {
				set[cfg.AP.Interface] = true
			}
		}
		return set
	})
	devs.SetDenyFile(runDir + "/deny_mac")
	applyRates := func() {
		// LAN 侧出口（下载方向整形）：AP 直挂口或各 LAN 桥；WAN 侧（上传方向）：当前上行
		lanIfaces := make([]string, 0, len(cfg.LAN))
		for _, seg := range cfg.LAN {
			if seg.Bridge != "" {
				lanIfaces = append(lanIfaces, seg.Bridge)
			}
		}
		if seg := cfg.LANForAP(); seg != nil && seg.Bridge == "" && apm.Status().State == ap.StateRunning {
			lanIfaces = append(lanIfaces, cfg.AP.Interface)
		}
		wan := ""
		if dr, err := priv.DefaultRoute4(); err == nil {
			wan = dr.Iface
		}
		if err := priv.ApplyRateLimits(lanIfaces, wan, devs.RateLimits()); err != nil {
			logger.Warn("重建限速树失败", "err", err)
		}
	}
	devs.SetHooks(netrtm.DeviceCounters, applyRates, refreshNet)
	afterNetRefresh = applyRates
	netrtm.BlockedIPsFn = devs.BlockedIPs
	netrtm.BlockedMACsFn = func() []string { return sys.Snapshot().BlockedMACs }
	netrtm.CountersEnabled = true

	// 无线能力探测 + 角色同步
	ifaces.SetRoles(rolesFromConfig(cfg))
	ifaces.ProbeAll()

	// 启动清场：杀掉上一实例被 SIGKILL 留下的孤儿（占 LAN 地址会导致 dnsmasq 起不来），
	// 并撤掉残留的自家 nft 表（只动 apmanager 表，安全）
	if !*dev {
		if n := supervisor.SweepOrphans(runDir, logger); n > 0 {
			logger.Info("启动清扫完成", "killed", n)
		}
		priv.NftDelete()
	}

	// supervisor 状态 → SSE
	sup.OnState(func(name string, st supervisor.ProcState, detail string) {
		evbus.Publish("status", map[string]any{"proc": map[string]string{
			"name": name, "state": string(st), "detail": detail,
		}})
	})

	// API 服务器
	sampler := system.NewSampler(evbus, logger)
	srv := api.New(cfg, *cfgPath, sys, apm, netrtm, relm, ethm, devs, ifaces, sup, evbus, ring, sampler, logger, web.Assets)
	listen := *addr
	if listen == "" {
		listen = fmt.Sprintf("%s:%d", cfg.Admin.Bind, cfg.Admin.Port)
	}
	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler()}

	// 启动循环
	stop := make(chan struct{})
	ifaces.StartSampler(stop)
	netrtm.StartLoops(stop)
	devs.Start(stop)
	sampler.Start(stop)

	// 对账循环（5s）：期望态收敛 —— AP enabled 但没跑 → 拉起；ip_forward；等
	go reconcileLoop(stop, cfg, apm, netrtm, ethm, logger)

	// 开机即启用 AP（config.ap.enabled）
	if cfg.AP.Enabled && !*dev {
		if err := apm.Start(); err != nil {
			logger.Error("自启 AP 失败", "err", err)
		}
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		logger.Error("HTTP 监听失败", "addr", listen, "err", err)
		os.Exit(1)
	}
	logger.Info("apmanager 已启动", "addr", listen, "dev", *dev, "config", *cfgPath)

	// 优雅退出：信号 → 停 HTTP → 停模块与子进程组（TERM→KILL）→ join 完成后才退出主流程。
	// 同步化是关键：此前清理与 Serve 返回并发，main 可能在子进程未死尽时就退出，留孤儿。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	cleanupDone := make(chan struct{})
	go func() {
		<-sig
		logger.Info("收到退出信号，停止各模块…")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = httpSrv.Shutdown(ctx) // 先停 HTTP，不再接受会改状态的新请求
		cancel()
		close(stop)
		_ = apm.Stop()               // hostapd（含 ctrl 客户端）
		sup.StopAll(4 * time.Second) // dnsmasq / wpa_supplicant / pppd：TERM→4s→KILL
		close(cleanupDone)
	}()

	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Error("HTTP 服务异常退出", "err", err)
		os.Exit(1)
	}
	// 等子进程组全部回收再退出；Serve 意外退出（非信号路径）时防死等
	select {
	case <-cleanupDone:
	case <-time.After(6 * time.Second):
		logger.Warn("清理超时，强制退出（孤儿由下次启动清扫兜底）")
	}
	logger.Info("apmanager 已退出")
}

// rolesFromConfig 从配置提取接口角色表：ports[].role + LAN 桥 → lan
func rolesFromConfig(cfg *config.Config) map[string]iface.Role {
	out := map[string]iface.Role{}
	for _, p := range cfg.Ports {
		switch p.Role {
		case "wan", "lan":
			out[p.Name] = iface.Role(p.Role)
		default:
			out[p.Name] = iface.RoleUnused
		}
	}
	for _, seg := range cfg.LAN {
		if seg.Bridge != "" {
			out[seg.Bridge] = iface.RoleLan
		}
	}
	return out
}

// reconcileLoop 期望态对账：AP 应跑未跑 → 重拉；LAN 地址丢失补配（驱动重载重建
// 接口后 192.168.50.x 会消失，BUGS2 #3）；WAN 口掉址补拉；网口桥/成员核对
func reconcileLoop(stop <-chan struct{}, cfg *config.Config, apm *ap.Manager, netrtm *netrt.Manager, ethm *ethport.Manager, log *slog.Logger) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if cfg.AP.Enabled && apm.Status().State == ap.StateFailed {
				log.Info("对账：AP 处于 failed，尝试重新拉起")
				_ = apm.Restart()
			}
			reconcileLANAddrs(cfg, apm, log)
			ethm.Reconcile()
			_ = netrtm
		}
	}
}

// reconcileLANAddrs 校验每个生效 LAN 段的接口/桥拥有配置地址，缺失则补。
// 场景：rtw88 驱动重载后 wlp1s0 重建，地址丢失（BUGS2 #3）；桥地址被误删。
func reconcileLANAddrs(cfg *config.Config, apm *ap.Manager, log *slog.Logger) {
	apRunning := apm.Status().State == ap.StateRunning
	for _, seg := range cfg.LAN {
		ifaceName := seg.Bridge
		if ifaceName == "" {
			if !seg.AP || !apRunning {
				continue // AP 直挂段只在 AP 运行时归属该接口
			}
			ifaceName = cfg.AP.Interface
		}
		li, err := priv.LinkByName(ifaceName)
		if err != nil {
			continue // 接口还不存在（驱动未加载/桥未建），等下一轮
		}
		if hasAddr(li.Addrs, seg.Subnet) {
			continue
		}
		log.Warn("对账：LAN 地址丢失，补配", "iface", ifaceName, "subnet", seg.Subnet)
		if err := priv.AddrReplace(ifaceName, seg.Subnet); err != nil {
			log.Error("补配地址失败", "iface", ifaceName, "err", err)
		}
	}
}

// hasAddr 地址列表里是否有目标 CIDR 的主机地址（网关地址，如 192.168.50.1）
func hasAddr(addrs []string, cidr string) bool {
	want := strings.SplitN(cidr, "/", 2)[0]
	for _, a := range addrs {
		if a == cidr || strings.SplitN(a, "/", 2)[0] == want {
			return true
		}
	}
	return false
}
