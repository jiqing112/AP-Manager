// Package api HTTP 层：REST 路由（Go 1.22 方法+通配）、鉴权中间件、SSE Hub、静态资源挂载。
package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"apmanager/internal/ap"
	"apmanager/internal/bus"
	"apmanager/internal/config"
	"apmanager/internal/devices"
	"apmanager/internal/diag"
	"apmanager/internal/ethport"
	"apmanager/internal/iface"
	"apmanager/internal/netrt"
	"apmanager/internal/priv"
	"apmanager/internal/relay"
	"apmanager/internal/supervisor"
	"apmanager/internal/system"
	"apmanager/internal/upnp"
	"apmanager/internal/wol"
)

// Server API 服务器：聚合各模块
type Server struct {
	cfg     *config.Config
	cfgPath string
	sys     *system.Manager
	apm     *ap.Manager
	netrtm  *netrt.Manager
	relm    *relay.Manager
	ethm    *ethport.Manager
	devs    *devices.Manager
	ifaces  *iface.Manager
	sup     *supervisor.Supervisor
	evbus   *bus.Bus
	ring    *system.AppRing
	sampler *system.Sampler
	log     *slog.Logger
	assets  fs.FS

	// NetRefresh 网络面刷新回调（main 注入 refreshNet 闭包）：防火墙/DNS/UPnP
	// 保存后触发整表重渲染
	NetRefresh func()

	mu  sync.Mutex // 串行化变更类操作（全局变更串行化要求）
	mux *http.ServeMux
}

// New 构造并注册路由
func New(cfg *config.Config, cfgPath string, sys *system.Manager, apm *ap.Manager, netrtm *netrt.Manager,
	relm *relay.Manager, ethm *ethport.Manager, devs *devices.Manager, ifaces *iface.Manager,
	sup *supervisor.Supervisor, evbus *bus.Bus, ring *system.AppRing, sampler *system.Sampler,
	log *slog.Logger, assets fs.FS) *Server {
	s := &Server{
		cfg: cfg, cfgPath: cfgPath, sys: sys, apm: apm, netrtm: netrtm, relm: relm, ethm: ethm, devs: devs,
		ifaces: ifaces, sup: sup, evbus: evbus, ring: ring, sampler: sampler,
		log: log, assets: assets, mux: http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler 返回根 Handler
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", s.handleLogout)

	// 以下 /api 均要求登录
	m.HandleFunc("GET /api/session", s.auth(s.handleSession))

	m.HandleFunc("GET /api/overview", s.auth(s.handleOverview))
	m.HandleFunc("GET /api/ap/config", s.auth(s.handleAPConfigGet))
	m.HandleFunc("PUT /api/ap/config", s.auth(s.mutation(s.handleAPConfigPut)))
	m.HandleFunc("GET /api/ap/capabilities", s.auth(s.handleAPCaps))
	m.HandleFunc("GET /api/ap/qrcode.png", s.auth(s.handleAPQRCode))
	m.HandleFunc("POST /api/ap/{action}", s.auth(s.mutation(s.handleAPAction)))
	m.HandleFunc("GET /api/net/topology", s.auth(s.handleTopology))
	m.HandleFunc("POST /api/net/plan", s.auth(s.mutation(s.handleNetPlan)))
	m.HandleFunc("POST /api/net/apply", s.auth(s.mutation(s.handleNetApply)))
	m.HandleFunc("GET /api/net/mode", s.auth(s.handleNetModeGet))
	m.HandleFunc("POST /api/net/mode", s.auth(s.mutation(s.handleNetModeSet)))
	m.HandleFunc("POST /api/net/apply/{id}/confirm", s.auth(s.mutation(s.handleNetConfirm)))
	m.HandleFunc("POST /api/net/apply/{id}/rollback", s.auth(s.mutation(s.handleNetRollback)))
	m.HandleFunc("POST /api/relay/scan", s.auth(s.mutation(s.handleRelayScan)))
	m.HandleFunc("GET /api/relay/scan-results", s.auth(s.handleRelayResults))
	m.HandleFunc("POST /api/relay/connect", s.auth(s.mutation(s.handleRelayConnect)))
	m.HandleFunc("DELETE /api/relay/connect", s.auth(s.mutation(s.handleRelayDisconnect)))
	m.HandleFunc("DELETE /api/relay/networks/{ssid}", s.auth(s.mutation(s.handleRelayForget)))
	m.HandleFunc("GET /api/relay/config", s.auth(s.handleRelayConfigGet))
	m.HandleFunc("PUT /api/relay/config", s.auth(s.mutation(s.handleRelayConfigPut)))
	m.HandleFunc("GET /api/devices", s.auth(s.handleDevices))
	m.HandleFunc("PUT /api/devices/{mac}/alias", s.auth(s.mutation(s.handleDeviceAlias)))
	m.HandleFunc("PUT /api/devices/{mac}/rate", s.auth(s.mutation(s.handleDeviceRate)))
	m.HandleFunc("POST /api/devices/{mac}/kick", s.auth(s.mutation(s.handleDeviceKick)))
	m.HandleFunc("POST /api/devices/{mac}/block", s.auth(s.mutation(s.handleDeviceBlock)))
	m.HandleFunc("DELETE /api/devices/{mac}/block", s.auth(s.mutation(s.handleDeviceUnblock)))
	m.HandleFunc("GET /api/system/status", s.auth(s.handleSystemStatus))
	m.HandleFunc("PUT /api/system/password", s.auth(s.mutation(s.handlePassword)))
	m.HandleFunc("POST /api/system/reboot", s.auth(s.mutation(s.handleReboot)))
	m.HandleFunc("GET /api/system/schedule", s.auth(s.handleScheduleGet))
	m.HandleFunc("PUT /api/system/schedule", s.auth(s.mutation(s.handleSchedulePut)))
	m.HandleFunc("POST /api/wol", s.auth(s.mutation(s.handleWOL)))
	m.HandleFunc("GET /api/upnp/status", s.auth(s.handleUpnpStatus))
	m.HandleFunc("GET /api/firewall", s.auth(s.handleFirewallGet))
	m.HandleFunc("PUT /api/firewall", s.auth(s.mutation(s.handleFirewallPut)))
	m.HandleFunc("POST /api/diag/run", s.auth(s.handleDiagRun))
	m.HandleFunc("GET /api/dns", s.auth(s.handleDNSGet))
	m.HandleFunc("PUT /api/dns", s.auth(s.mutation(s.handleDNSPut)))
	m.HandleFunc("GET /api/logs", s.auth(s.handleLogs))
	m.HandleFunc("GET /api/logs/diagnostics", s.auth(s.handleDiagnostics))
	m.HandleFunc("GET /api/events", s.auth(s.handleSSE))

	// 静态资源（SPA）：embed FS 根即站点根（index.html / app.js / dist/ / vendor/）
	staticFS := s.assets
	fileServer := http.FileServerFS(staticFS)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// SPA fallback：非文件命中时回 index.html（hash 路由无需此步，但直接路径访问更友好）
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := fs.Stat(staticFS, path); err != nil {
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ---- 中间件 ----

const sessionCookie = "apm_session"

// auth 会话校验；滑动续期（剩余 <6 天重发 cookie）
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// SSE 的 EventSource 不能带自定义头，但 GET 只读，无需 CSRF
		ck, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, 401, "未登录")
			return
		}
		ok, renewed := s.sys.ValidateSession(ck.Value)
		if !ok {
			writeErr(w, 401, "会话已过期，请重新登录")
			return
		}
		if renewed != "" {
			http.SetCookie(w, &http.Cookie{
				Name: sessionCookie, Value: renewed, Path: "/", HttpOnly: true,
				SameSite: http.SameSiteLaxMode, MaxAge: int(7 * 24 * time.Hour / time.Second),
			})
		}
		next(w, r)
	}
}

// mutation 变更类操作：额外校验 X-Requested-With（轻量 CSRF 防护）+ 串行化
func (s *Server) mutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			writeErr(w, 403, "缺少 X-Requested-With 头")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		next(w, r)
	}
}

// ---- handlers ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Password string `json:"password"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	ip := clientIP(r)
	cookie, mustChange, err := s.sys.Login(ip, body.Password)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: cookie, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(7 * 24 * time.Hour / time.Second),
	})
	writeJSON(w, 200, map[string]any{"ok": true, "mustChange": mustChange})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"logged":      true,
		"mustChange":  s.sys.Snapshot().MustChangePassword,
		"version":     "1.0.0-phase1",
	})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	aps := s.apm.Status()
	writeJSON(w, 200, map[string]any{
		"ap":          aps,
		"relay":       s.relm.Status(),
		"upstream":    s.netrtm.Upstream(),
		"devices":     map[string]any{"online": s.devs.CountOnline(), "total": len(s.devs.List())},
		"ifaces":      s.ifaces.List(),
		"procs":       s.sup.Status(),
		"system":      s.sampler.Latest(),
		"lan":         s.cfg.LAN,
		"networkMode": s.cfg.NetworkMode,
		"ts":          time.Now().UnixMilli(),
	})
}

func (s *Server) handleAPConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.cfg.AP)
}

func (s *Server) handleAPConfigPut(w http.ResponseWriter, r *http.Request) {
	var body config.AP
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	body.Interface = s.cfg.AP.Interface // 接口暂不允许改（单卡）
	old := s.cfg.AP
	s.cfg.AP = body
	if errs := s.cfg.Validate(); len(errs) > 0 {
		s.cfg.AP = old
		writeErr(w, 400, strings.Join(errs, "; "))
		return
	}
	if err := s.cfg.Save(s.cfgPath); err != nil {
		s.cfg.AP = old
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	s.apm.SetConfig(s.cfg)
	s.netrtm.SetConfig(s.cfg)

	wasRunning := s.apm.Status().State == ap.StateRunning
	needRestart := wasRunning && apConfigDiffers(old, body)
	var startErr string
	switch {
	case body.Enabled && (needRestart || !wasRunning):
		if err := s.apm.Restart(); err != nil {
			startErr = err.Error()
		}
	case !body.Enabled && wasRunning:
		_ = s.apm.Stop()
	}
	resp := map[string]any{"ok": true, "state": s.apm.Status().State}
	if startErr != "" {
		resp["warning"] = "配置已保存，但 AP 生效失败：" + startErr
	}
	writeJSON(w, 200, resp)
}

func apConfigDiffers(a, b config.AP) bool {
	return a.SSID != b.SSID || a.Password != b.Password || a.Security != b.Security ||
		a.Band != b.Band || a.Channel != b.Channel || a.Bandwidth != b.Bandwidth ||
		a.Hidden != b.Hidden || a.MaxClients != b.MaxClients
}

func (s *Server) handleAPCaps(w http.ResponseWriter, r *http.Request) {
	wi := s.ifaces.WirelessCaps(s.cfg.AP.Interface)
	writeJSON(w, 200, map[string]any{
		"wireless":  wi,
		"hostapdVersion": "2.10",
		"interface": s.cfg.AP.Interface,
	})
}

func (s *Server) handleAPAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	var err error
	switch action {
	case "start":
		err = s.apm.Start()
		s.cfg.AP.Enabled = true
		_ = s.cfg.Save(s.cfgPath)
	case "stop":
		err = s.apm.Stop()
		s.cfg.AP.Enabled = false
		_ = s.cfg.Save(s.cfgPath)
	case "restart":
		err = s.apm.Restart()
	default:
		writeErr(w, 404, "未知操作 "+action)
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": s.apm.Status().State})
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ifaces":   s.ifaces.List(),
		"ports":    s.cfg.Ports,
		"lan":      s.cfg.LAN,
		"zones":    s.cfg.ZonePolicy,
		"pending":  s.ethm.Pending(),
		"nftables": s.netrtm.NftDump(),
		"dnsmasq":  s.netrtm.IsDnsmasqRunning(),
		"upstream": s.netrtm.Upstream(),
		"relay":    s.relm.Status(),
		"dnsservers": s.netrtm.DNSUpstreams(),
		"networkMode": s.cfg.NetworkMode,
	})
}

// ---- 网口安全应用 ----

func (s *Server) handleNetPlan(w http.ResponseWriter, r *http.Request) {
	var cand ethport.Candidate
	if err := json.NewDecoder(r.Body).Decode(&cand); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	warns, risks, errs := s.ethm.Plan(cand)
	writeJSON(w, 200, map[string]any{"warns": warns, "risks": risks, "errors": errs})
}

func (s *Server) handleNetApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Candidate    ethport.Candidate `json:"candidate"`
		Description  string            `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	if body.Description == "" {
		body.Description = "网口变更"
	}
	id, err := s.ethm.Apply(body.Candidate, body.Description)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "pending": s.ethm.Pending()})
}

func (s *Server) handleNetConfirm(w http.ResponseWriter, r *http.Request) {
	if err := s.ethm.Confirm(r.PathValue("id")); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleNetRollback(w http.ResponseWriter, r *http.Request) {
	if err := s.ethm.Rollback(r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- 网络模式（GL.iNet 式：路由 / AP / 无线扩展 / WDS）----

func (s *Server) handleNetModeGet(w http.ResponseWriter, r *http.Request) {
	mode := s.cfg.NetworkMode
	if mode == "" {
		mode = "router"
	}
	writeJSON(w, 200, map[string]any{"mode": mode, "apWanPort": s.cfg.APWanPort})
}

// handleNetModeSet 切换网络模式：本质是构造对应网口拓扑走安全应用事务
// （确认窗口内未确认自动回滚，模式随拓扑一起恢复）。
func (s *Server) handleNetModeSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	switch body.Mode {
	case "wisp":
		// 无线扩展 = 无线中继做上行，连接动作在「无线中继」页完成
		writeJSON(w, 200, map[string]any{"ok": true, "goto": "relay",
			"note": "无线扩展模式请到「无线中继」页选择上游 WiFi 连接"})
		return
	case "wds":
		writeErr(w, 400, "WDS 依赖主路由开启 WDS 且驱动支持，本面板暂未实现；请使用「无线扩展模式」")
		return
	case "ap", "router":
	default:
		writeErr(w, 400, "未知网络模式 "+body.Mode)
		return
	}

	// 候选 = 当前拓扑 + 模式翻译
	ports := append([]config.Port(nil), s.cfg.Ports...)
	lan := append([]config.LANSeg(nil), s.cfg.LAN...)
	mode := body.Mode

	if mode == "ap" {
		// 找当前 WAN 口收编进主桥
		wanIdx := -1
		for i := range ports {
			if ports[i].Role == "wan" {
				wanIdx = i
				break
			}
		}
		if wanIdx < 0 {
			writeErr(w, 409, "当前没有 WAN 口（可能已处于 AP 模式）")
			return
		}
		wanName := ports[wanIdx].Name
		// 主桥：第一个带桥的 LAN 段；全都没有则给 AP 所在段补 br-lan
		bridge := ""
		for i := range lan {
			if lan[i].Bridge != "" {
				bridge = lan[i].Bridge
				break
			}
		}
		if bridge == "" && len(lan) > 0 {
			bridge = "br-lan"
			lan[0].Bridge = bridge
		}
		if bridge == "" {
			writeErr(w, 409, "没有 LAN 网段，无法切换 AP 模式")
			return
		}
		ports[wanIdx].Role = "lan"
		ports[wanIdx].Bridge = bridge
		ports[wanIdx].WAN = nil
		s.cfg.APWanPort = wanName // 记忆原 WAN 口，切回路由模式时恢复
	} else {
		// 切回路由：把 AP 模式收编的口恢复为 WAN
		if s.cfg.APWanPort == "" {
			writeErr(w, 409, "当前已是路由模式")
			return
		}
		found := false
		for i := range ports {
			if ports[i].Name == s.cfg.APWanPort {
				ports[i].Role = "wan"
				ports[i].Bridge = ""
				ports[i].WAN = &config.WANSetting{Mode: "dhcp"}
				found = true
			}
		}
		if !found {
			writeErr(w, 409, "原 WAN 口 "+s.cfg.APWanPort+" 不存在，无法自动恢复；请到「局域网」页手动分配")
			return
		}
		s.cfg.APWanPort = ""
	}

	id, err := s.ethm.Apply(ethport.Candidate{
		Ports: ports, LAN: lan, NetworkMode: mode, AcknowledgeRisk: true,
	}, "切换网络模式："+map[string]string{"ap": "AP 接入点", "router": "路由"}[mode])
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "pending": s.ethm.Pending(), "mode": mode})
}

// ---- 中继 ----

func (s *Server) handleRelayScan(w http.ResponseWriter, r *http.Request) {
	var body struct{ Force bool `json:"force"` }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.relm.Scan(body.Force); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"results": s.relm.Results()})
}

func (s *Server) handleRelayResults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"results": s.relm.Results(),
		"status":  s.relm.Status(),
	})
}

func (s *Server) handleRelayConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SSID     string `json:"ssid"`
		Password string `json:"password"`
		Save     bool   `json:"save"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	if err := s.relm.Connect(body.SSID, body.Password, body.Save); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": s.relm.Status()})
}

func (s *Server) handleRelayDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.relm.Disconnect(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRelayForget(w http.ResponseWriter, r *http.Request) {
	ssid := r.PathValue("ssid")
	s.mu.Lock()
	defer s.mu.Unlock()
	nets := s.cfg.Relay.Networks[:0]
	for _, n := range s.cfg.Relay.Networks {
		if n.SSID != ssid {
			nets = append(nets, n)
		}
	}
	s.cfg.Relay.Networks = nets
	if err := s.cfg.Save(s.cfgPath); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.relm.SetConfig(s.cfg)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRelayConfigGet(w http.ResponseWriter, r *http.Request) {
	// 密码不回传，只回 SSID/优先级
	nets := make([]map[string]any, 0, len(s.cfg.Relay.Networks))
	for _, n := range s.cfg.Relay.Networks {
		nets = append(nets, map[string]any{"ssid": n.SSID, "priority": n.Priority})
	}
	writeJSON(w, 200, map[string]any{
		"interface": s.cfg.Relay.Interface,
		"current":   s.cfg.Relay.Current,
		"networks":  nets,
		"reconnect": s.cfg.Relay.Reconnect,
		"status":    s.relm.Status(),
		"caps":      s.ifaces.WirelessCaps(s.cfg.Relay.Interface),
	})
}

func (s *Server) handleRelayConfigPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reconnect *config.RelayReconnect `json:"reconnect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	if body.Reconnect != nil {
		s.cfg.Relay.Reconnect = *body.Reconnect
	}
	if err := s.cfg.Save(s.cfgPath); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.relm.SetConfig(s.cfg)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"devices": s.devs.List()})
}

func (s *Server) handleDeviceAlias(w http.ResponseWriter, r *http.Request) {
	mac := r.PathValue("mac")
	var body struct{ Alias string `json:"alias"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	if err := s.devs.SetAlias(mac, body.Alias); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDeviceRate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Down int `json:"down"`
		Up   int `json:"up"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	if err := s.devs.SetRate(r.PathValue("mac"), body.Down, body.Up); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDeviceKick(w http.ResponseWriter, r *http.Request) {
	note, err := s.devs.Kick(r.PathValue("mac"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "note": note})
}

func (s *Server) handleDeviceBlock(w http.ResponseWriter, r *http.Request) {
	if err := s.devs.Block(r.PathValue("mac")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDeviceUnblock(w http.ResponseWriter, r *http.Request) {
	if err := s.devs.Unblock(r.PathValue("mac")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.sampler.Latest())
}

// handleReboot 重启整机。systemctl 优先（干净地走 systemd 关机序列），
// 失败回退 reboot；响应后 1s 才发指令，让 HTTP 应答先落到客户端。
func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	dev := priv.Mode == "dev"
	if !dev {
		time.AfterFunc(time.Second, func() {
			if _, ok := priv.RunOK(5*time.Second, "systemctl", "reboot"); !ok {
				_, _ = priv.RunOK(5*time.Second, "reboot")
			}
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "dev": dev, "note": "系统将在数秒内重启，面板稍后自动恢复"})
}

// ---- 定时重启 ----

func (s *Server) handleScheduleGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"rebootAt": s.cfg.System.RebootAt})
}

func (s *Server) handleSchedulePut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RebootAt string `json:"rebootAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	old := s.cfg.System.RebootAt
	s.cfg.System.RebootAt = strings.TrimSpace(body.RebootAt)
	if errs := s.cfg.Validate(); len(errs) > 0 {
		s.cfg.System.RebootAt = old
		writeErr(w, 400, strings.Join(errs, "; "))
		return
	}
	if err := s.cfg.Save(s.cfgPath); err != nil {
		s.cfg.System.RebootAt = old
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "rebootAt": s.cfg.System.RebootAt})
}

// ---- WOL 网络唤醒 ----

func (s *Server) handleWOL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MAC string `json:"mac"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	// 定向子网广播：对每个带桥的 LAN 段广播地址各发一次（跨 VLAN 更可靠）
	var bcasts []string
	for _, seg := range s.cfg.LAN {
		if seg.Bridge == "" {
			continue
		}
		if gw, _, err := net.ParseCIDR(seg.Subnet); err == nil {
			ip := gw.To4()
			if ip != nil {
				bcasts = append(bcasts, fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3]+1))
			}
		}
	}
	if err := wol.Send(body.MAC, bcasts...); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "note": "魔术包已发送（全网广播 + 各 LAN 段定向广播）"})
}

// ---- UPnP 状态 ----

func (s *Server) handleUpnpStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.upnpStatus())
}

// ---- 防火墙（端口转发 / 开放端口 / DMZ）----

func (s *Server) handleFirewallGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.cfg.Firewall)
}

func (s *Server) upnpStatus() map[string]any {
	running := false
	for _, p := range s.sup.Status() {
		if p["name"] == "miniupnpd" && p["state"] == "running" {
			running = true
		}
	}
	return map[string]any{"enabled": s.cfg.Firewall.UpnpEnabled, "installed": upnp.Installed(), "running": running}
}

// handleFirewallPut 整体替换防火墙规则：校验 → 原子保存 → 整表重渲染下发。
// nft 表整体刷新与既有 Refresh 通路复用，任何后续刷新（AP 起停等）都会带上最新规则。
func (s *Server) handleFirewallPut(w http.ResponseWriter, r *http.Request) {
	var fw config.Firewall
	if err := json.NewDecoder(r.Body).Decode(&fw); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	old := s.cfg.Firewall
	s.cfg.Firewall = fw
	if errs := fw.Validate(); len(errs) > 0 {
		s.cfg.Firewall = old
		writeErr(w, 400, strings.Join(errs, "; "))
		return
	}
	if err := s.cfg.Save(s.cfgPath); err != nil {
		s.cfg.Firewall = old
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	applied := true
	note := ""
	if s.NetRefresh != nil {
		s.NetRefresh()
	} else {
		applied = false
		note = "网络面刷新回调未接入，规则将在下次网络变更时生效"
	}
	writeJSON(w, 200, map[string]any{"ok": true, "applied": applied, "note": note})
}

// ---- DNS 上游 ----

// handleDNSGet 返回用户配置的上游与当前实际生效的上游（WAN 页展示/编辑用）
func (s *Server) handleDNSGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"configured": s.cfg.DNS.Upstream,   // 手动配置（可能为空 = 自动）
		"effective":  s.netrtm.DNSUpstreams(), // dnsmasq 实际在用的
	})
}

// handleDNSPut 整体替换 DNS 上游：校验 IP → 保存 → dnsmasq 重渲染重启。
// 空列表 = 恢复自动（跟随 WAN 口/系统解析配置）。
func (s *Server) handleDNSPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Servers []string `json:"servers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	old := s.cfg.DNS.Upstream
	s.cfg.DNS.Upstream = body.Servers
	if errs := s.cfg.Validate(); len(errs) > 0 {
		s.cfg.DNS.Upstream = old
		writeErr(w, 400, strings.Join(errs, "; "))
		return
	}
	if err := s.cfg.Save(s.cfgPath); err != nil {
		s.cfg.DNS.Upstream = old
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	if s.NetRefresh != nil {
		s.NetRefresh() // dnsmasq 换上游
	}
	writeJSON(w, 200, map[string]any{"ok": true, "effective": s.netrtm.DNSUpstreams()})
}

// ---- 网络诊断 ----

// handleDiagRun 从路由器本机执行一项诊断并返回文本输出（ping/路由跟踪/DNS/TCP 探测）
func (s *Server) handleDiagRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tool   string `json:"tool"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	out, err := diag.Run(priv.Mode == "dev", body.Tool, body.Target)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "tool": body.Tool, "target": body.Target, "output": out})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	if err := s.sys.SetPassword(body.Old, body.New); err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	// 改密后当前会话也失效（密钥轮换），前端跳登录
	writeJSON(w, 200, map[string]any{"ok": true, "relogin": true})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	items := s.ring.Tail(500)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if level != "" && !strings.Contains(it.Line, "level="+level) {
			continue
		}
		out = append(out, map[string]any{"ts": it.Ts.UnixMilli(), "line": it.Line})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	procs := s.sup.Status()
	// 每个托管进程附 stderr 尾部（诊断面板）
	for _, p := range procs {
		if lines := s.sup.TailLog(p["name"].(string), 25); len(lines) > 0 {
			out := make([]string, 0, len(lines))
			for _, l := range lines {
				out = append(out, l.Line)
			}
			p["log"] = out
		}
	}
	writeJSON(w, 200, map[string]any{
		"procs": procs,
		"nft":   s.netrtm.NftDump(),
	})
}

// ---- 小助手 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
