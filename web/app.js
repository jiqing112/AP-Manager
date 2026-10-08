/* app.js —— Alpine 应用：store（登录态/路由/暗色/toast）、SSE 客户端、页面组件、工具函数。
   页面不轮询：首屏 fetch 一次，之后由 SSE 持续刷新 live store。 */

/* ---------------- 工具函数（模板里可直接调用） ---------------- */

function fmtBytes(n) {
  if (n == null) return "—";
  if (n < 1024) return n + " B";
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 ? 0 : 1) + " " + units[i];
}

function fmtRate(bps) {
  if (bps == null) return "—";
  const bits = bps * 8;
  if (bits < 1000) return bits.toFixed(0) + " bps";
  const units = ["Kbps", "Mbps", "Gbps"];
  let v = bits / 1000, i = 0;
  while (v >= 1000 && i < units.length - 1) { v /= 1000; i++; }
  return v.toFixed(v >= 100 ? 0 : 1) + " " + units[i];
}

function fmtDur(sec) {
  if (sec == null) return "—";
  sec = Math.floor(sec);
  if (sec < 60) return sec + " 秒";
  if (sec < 3600) return Math.floor(sec / 60) + " 分 " + (sec % 60) + " 秒";
  if (sec < 86400) return Math.floor(sec / 3600) + " 时 " + Math.floor((sec % 3600) / 60) + " 分";
  return Math.floor(sec / 86400) + " 天 " + Math.floor((sec % 86400) / 3600) + " 时";
}

function spinner() {
  return '<svg viewBox="0 0 24 24" fill="none"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"/><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 0 1 8-8v4a4 4 0 0 0-4 4H4z"/></svg>';
}

/* lucide 图标子集（1.5px 描边，16px） */
const ICONS = {
  gauge: '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="m12 14 4-4"/><path d="M3.34 19a10 10 0 1 1 17.32 0"/></svg>',
  wifi: '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h.01"/><path d="M2 8.82a15 15 0 0 1 20 0"/><path d="M5 12.859a10 10 0 0 1 14 0"/><path d="M8.5 16.429a5 5 0 0 1 7 0"/></svg>',
  devices: '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><rect width="20" height="14" x="2" y="3" rx="2"/><line x1="8" x2="16" y1="21" y2="21"/><line x1="12" x2="12" y1="17" y2="21"/></svg>',
  ports: '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="7" width="20" height="10" rx="2"/><path d="M6 10v4M10 10v4M14 10v4M18 10v4"/></svg>',
  relay: '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z"/></svg>',
  system: '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="4" width="16" height="16" rx="2"/><rect x="9" y="9" width="6" height="6"/><path d="M15 2v2M15 20v2M2 15h2M2 9h2M20 15h2M20 9h2M9 2v2M9 20v2"/></svg>',
  logs: '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><line x1="8" x2="16" y1="13" y2="13"/><line x1="8" x2="16" y1="17" y2="17"/></svg>',
};

/* ---------------- fetch 封装：401 跳登录、错误 toast、CSRF 头 ---------------- */

async function apiFetch(path, opts = {}) {
  const headers = { "X-Requested-With": "XMLHttpRequest", ...(opts.headers || {}) };
  if (opts.body && typeof opts.body !== "string") {
    opts = { ...opts, body: JSON.stringify(opts.body) };
    headers["Content-Type"] = "application/json";
  }
  const resp = await fetch(path, { ...opts, headers });
  let data = {};
  try { data = await resp.json(); } catch (e) { /* 非 JSON */ }
  if (resp.status === 401) {
    Alpine.store("app").logout(false);
    const err = new Error(data.error || "未登录");
    err.silent = true; // 401 属会话态切换，不该刷错误 toast
    throw err;
  }
  if (!resp.ok) throw new Error(data.error || ("HTTP " + resp.status));
  return data;
}

/* 页面加载失败的统一提示（静默 401 除外） */
function loadErr(prefix, e) {
  if (e && e.silent) return;
  Alpine.store("app").toast(prefix + "：" + e.message, "error");
}

/* ---------------- Alpine 组件与 store 注册 ---------------- */

document.addEventListener("alpine:init", () => {
  /* ===== app store：登录态、路由、暗色、toast、SSE ===== */
  Alpine.store("app", {
    page: "login",
    authed: false, // boot()/login() 成功后才为 true；路由据此决定是否拉数据
    dark: false,
    drawer: false,
    sse: "off", // on | conn | off
    toasts: [],
    loading: { overview: true },
    loginErr: "",
    loginBusy: false,
    mustChange: false,
    nav: [
      { id: "overview", label: "概览", icon: "gauge" },
      { id: "ap", label: "热点", icon: "wifi" },
      { id: "relay", label: "中继", icon: "relay" },
      { id: "devices", label: "设备", icon: "devices" },
      { id: "ports", label: "网口", icon: "ports" },
      { id: "system", label: "系统", icon: "system" },
      { id: "logs", label: "日志", icon: "logs" },
    ],
    navIcon(name) { return ICONS[name] || ""; },

    init() {
      this.dark = localStorage.getItem("apm-theme") === "dark";
      window.addEventListener("hashchange", () => this.route());
      this.route();
      this.boot();
    },

    route() {
      let h = location.hash.replace(/^#\/?/, "").split("?")[0];
      const known = this.nav.map((n) => n.id);
      this.page = known.includes(h) ? h : "overview";
      if (this.page !== "login" && this.authed) {
        this.loadPage(this.page);
        // 概览页激活：流量图首次可见才初始化 / 已存在则随视口 resize
        if (this.page === "overview") window.ensureChart();
      }
      // 通知各页面组件"我被激活了"（表单回填类组件借此带登录态重新拉取）
      if (this.page !== "login") {
        window.dispatchEvent(new CustomEvent("apm:page-" + this.page));
      }
    },

    async boot() {
      // 校验登录态
      try {
        await apiFetch("/api/session");
        this.authed = true;
        this.page = "overview";
        this.route();
        this.connectSSE();
        this.loadOverview();
      } catch (e) {
        this.logout(false);
      }
    },

    async login(ev) {
      this.loginBusy = true; this.loginErr = "";
      const fd = new FormData(ev.target);
      try {
        const r = await fetch("/api/login", {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest" },
          body: JSON.stringify({ password: fd.get("password") }),
        });
        const data = await r.json();
        if (!r.ok) throw new Error(data.error || "登录失败");
        this.mustChange = !!data.mustChange;
        ev.target.reset();
        this.authed = true;
        this.page = "overview";
        location.hash = "#/overview";
        this.route();
        this.connectSSE();
        this.toast("登录成功", "success");
        if (this.mustChange) this.toast("首次登录，请尽快到「系统」页修改密码", "info", 6000);
      } catch (e) {
        this.loginErr = e.message;
      } finally {
        this.loginBusy = false;
      }
    },

    async logout(call = true) {
      if (call) { try { await apiFetch("/api/logout", { method: "POST" }); } catch (e) {} }
      this.authed = false;
      this.closeSSE();
      this.page = "login";
      if (location.hash !== "#/login") location.hash = "#/login";
    },

    /* ---- SSE ---- */
    _es: null,
    connectSSE() {
      this.closeSSE();
      this.sse = "conn";
      const es = new EventSource("/api/events?topics=status,traffic,devices,system,upstream");
      this._es = es;
      es.onopen = () => { this.sse = "on"; };
      es.onerror = () => { this.sse = "off"; /* EventSource 自动重连 */ };
      const live = Alpine.store("live");
      const apply = (topic, handler) => {
        es.addEventListener(topic, (ev) => {
          if (this.sse !== "on") this.sse = "on";
          try { handler(JSON.parse(ev.data)); } catch (e) { }
        });
      };
      apply("status", (d) => {
        if (d.ap) live.ap = { ...live.ap, ...d.ap };
        if (d.relay) live.relay = { ...live.relay, ...d.relay };
        if (d.proc) {
          const i = live.procs.findIndex((p) => p.name === d.proc.name);
          if (i >= 0) live.procs.splice(i, 1, d.proc); else live.procs.push(d.proc);
        }
      });
      apply("traffic", (d) => {
        live.traffic = d;
        if (d.links) live.ifaces = d.links;
        pushTrafficSample(d);
      });
      apply("devices", (d) => { live.devices = d; });
      apply("system", (d) => { live.system = d; });
      apply("upstream", (d) => { live.upstream = d; });
    },
    closeSSE() { if (this._es) { this._es.close(); this._es = null; this.sse = "off"; } },

    /* ---- 页面数据加载（首屏；后续靠 SSE） ---- */
    async loadPage(page) {
      if (page === "overview") this.loadOverview();
      if (page === "devices") this.loadDevices();
      if (page === "ports") this.loadTopology();
    },
    async loadOverview() {
      this.loading.overview = true;
      try {
        const d = await apiFetch("/api/overview");
        const live = Alpine.store("live");
        if (d.ap) live.ap = d.ap;
        if (d.relay) live.relay = d.relay;
        if (d.upstream) live.upstream = d.upstream;
        if (d.ifaces) live.ifaces = d.ifaces;
        if (d.system) live.system = d.system;
        if (d.procs) live.procs = d.procs;
        if (d.devices) live.devSummary = d.devices; // {online,total}
      } catch (e) {
        loadErr("加载概览失败", e);
      } finally {
        this.loading.overview = false;
      }
    },
    async loadDevices() {
      try {
        const d = await apiFetch("/api/devices");
        Alpine.store("live").devices = d.devices || [];
      } catch (e) { loadErr("加载设备失败", e); }
    },
    async loadTopology() {
      try {
        const d = await apiFetch("/api/net/topology");
        Alpine.store("live").ifaces = d.ifaces || [];
        Alpine.store("live").upstream = d.upstream || {};
      } catch (e) { loadErr("加载拓扑失败", e); }
    },

    /* ---- 杂项 ---- */
    apBusy: false,
    async quickApToggle() {
      const running = Alpine.store("live").ap?.state === "running";
      if (running && !confirm("停止热点？已连接设备将立即断网。")) return;
      this.apBusy = true;
      try {
        await apiFetch("/api/ap/" + (running ? "stop" : "start"), { method: "POST" });
        this.toast(running ? "热点已停止" : "热点已启动", "success");
      } catch (e) { this.toast("操作失败：" + e.message, "error"); }
      finally { this.apBusy = false; }
    },
    toggleDark() {
      this.dark = !this.dark;
      localStorage.setItem("apm-theme", this.dark ? "dark" : "light");
    },
    toast(msg, type = "info", ms = 3500) {
      if (this.toasts.some((t) => t.msg === msg)) return; // 同文案去重
      const id = Math.random().toString(36).slice(2);
      this.toasts.push({ id, msg, type });
      setTimeout(() => this.dismissToast(id), ms);
    },
    dismissToast(id) {
      this.toasts = this.toasts.filter((t) => t.id !== id);
    },
  });

  /* ===== live store：SSE 喂入的实时数据 ===== */
  Alpine.store("live", {
    ap: {}, relay: {}, upstream: {}, system: {},
    ifaces: [], devices: [], procs: [],
    traffic: null,
    history: { ts: [], rx: [], tx: [] }, // 最近 10 分钟（300 点 ×2s）
  });

  /* ---- 页面组件 ---- */

  Alpine.data("pageAp", () => ({
    form: { ssid: "", password: "", security: "wpa2", band: "2g", channel: 0, bandwidth: "20mhz", hidden: false, maxClients: 32, enabled: false },
    caps: null,
    showPw: false,
    saving: false,
    busy: "",
    formErr: "",
    confirmStop: false,
    qrOpen: false,
    qrTs: 0,

    async init() {
      // 首次初始化可能发生在登录前（401 静默）；登录/切页激活时经此事件重拉
      window.addEventListener("apm:page-ap", () => this.reload());
      await this.reload();
    },
    async reload() {
      try {
        const [cfg, caps] = await Promise.all([apiFetch("/api/ap/config"), apiFetch("/api/ap/capabilities")]);
        // 表单有未保存修改时不覆盖（切页回来不丢输入）
        const dirty = this._orig && JSON.stringify(this.form) !== JSON.stringify(this._orig);
        if (!dirty) {
          this.form = { ...this.form, ...cfg };
          this._orig = { ...this.form };
        }
        this.caps = caps;
        this.fixBand();
      } catch (e) { loadErr("加载 AP 配置失败", e); }
    },
    get ap() { return Alpine.store("live").ap; },
    bandAvail(b) { return !this.caps?.wireless || (this.caps.wireless.bands || []).includes(b); },
    channelsFor() {
      const chs = this.caps?.wireless?.channels?.[this.form.band] || [];
      return chs;
    },
    onBandChange() {
      const chs = this.channelsFor();
      if (chs.length && !chs.includes(this.form.channel)) this.form.channel = chs[0];
      if (this.form.band === "2g" && this.form.bandwidth === "80mhz") this.form.bandwidth = "40mhz";
    },
    fixBand() { this.onBandChange(); },
    reset() { this.form = { ...this._orig }; this.fixBand(); },
    isDirty() { return !!this._orig && JSON.stringify(this.form) !== JSON.stringify(this._orig); },
    genPw() {
      // 去除易混字符（0O1lI|）的 16 位随机强密码
      const cs = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789!@#$%^&*-_=+";
      const a = new Uint32Array(16);
      crypto.getRandomValues(a);
      this.form.password = Array.from(a, (n) => cs[n % cs.length]).join("");
      this.showPw = true;
    },
    openQR() { this.qrTs = Date.now(); this.qrOpen = true; },
    async save() {
      this.saving = true; this.formErr = "";
      try {
        const r = await apiFetch("/api/ap/config", { method: "PUT", body: this.form });
        this._orig = { ...this.form };
        Alpine.store("app").toast(r.warning || "已保存并生效", r.warning ? "info" : "success");
      } catch (e) { this.formErr = e.message; }
      finally { this.saving = false; }
    },
    async action(a) {
      if (a === "stop") this.confirmStop = false;
      this.busy = a;
      try {
        const r = await apiFetch("/api/ap/" + a, { method: "POST" });
        Alpine.store("app").toast(({ start: "热点已启动", stop: "热点已停止", restart: "热点已重启" })[a] || "完成", "success");
      } catch (e) { Alpine.store("app").toast("操作失败：" + e.message, "error"); }
      finally { this.busy = ""; }
    },
  }));

  Alpine.data("pageDevices", () => ({
    loaded: false,
    aliasDlg: { open: false, mac: "", alias: "" },
    rateDlg: { open: false, mac: "", ip: "", down: 0, up: 0 },

    async init() {
      await Alpine.store("app").loadDevices();
      this.loaded = true;
    },
    editAlias(d) {
      this.aliasDlg = { open: true, mac: d.mac, alias: d.alias || "" };
    },
    async saveAlias() {
      try {
        await apiFetch("/api/devices/" + this.aliasDlg.mac + "/alias", {
          method: "PUT", body: { alias: this.aliasDlg.alias },
        });
        this.aliasDlg.open = false;
        Alpine.store("app").loadDevices();
      } catch (e) { Alpine.store("app").toast("保存失败：" + e.message, "error"); }
    },
    editRate(d) {
      this.rateDlg = { open: true, mac: d.mac, ip: d.ip, down: d.rateDown || 0, up: d.rateUp || 0 };
    },
    async saveRate() {
      try {
        await apiFetch("/api/devices/" + this.rateDlg.mac + "/rate", {
          method: "PUT", body: { down: +this.rateDlg.down, up: +this.rateDlg.up },
        });
        this.rateDlg.open = false;
        Alpine.store("app").toast("限速已生效", "success");
        Alpine.store("app").loadDevices();
      } catch (e) { Alpine.store("app").toast("设置失败：" + e.message, "error"); }
    },
    async kick(d) {
      if (!confirm("把 " + (d.alias || d.hostname || d.mac) + " 踢下线？")) return;
      try {
        const r = await apiFetch("/api/devices/" + d.mac + "/kick", { method: "POST" });
        if (r.note) Alpine.store("app").toast(r.note, "info", 6000);
        else Alpine.store("app").toast("已踢下线", "success");
      } catch (e) { Alpine.store("app").toast("操作失败：" + e.message, "error"); }
    },
    async toggleBlock(d) {
      const name = d.alias || d.hostname || d.mac;
      if (d.blocked) {
        if (!confirm("解除 " + name + " 的拉黑？")) return;
        try {
          await apiFetch("/api/devices/" + d.mac + "/block", { method: "DELETE" });
          Alpine.store("app").toast("已解除拉黑", "success");
        } catch (e) { Alpine.store("app").toast("操作失败：" + e.message, "error"); }
      } else {
        if (!confirm("拉黑 " + name + "？\n该设备将无法再关联 WiFi / 获取地址 / 访问网络。")) return;
        try {
          await apiFetch("/api/devices/" + d.mac + "/block", { method: "POST" });
          Alpine.store("app").toast("已拉黑", "success");
        } catch (e) { Alpine.store("app").toast("操作失败：" + e.message, "error"); }
      }
      Alpine.store("app").loadDevices();
    },
  }));

  Alpine.data("pageRelay", () => ({
    _status: { state: "off" },
    // SSE 的 relay 状态与本地初值合并（live store 由事件流持续更新）
    get status() { return { ...this._status, ...(Alpine.store("live").relay || {}) }; },
    results: [],
    saved: [],
    caps: null,
    scanning: false,
    disconnecting: false,
    dlg: { open: false, ssid: "", password: "", save: true, needPw: true, busy: false, err: "" },

    async init() {
      window.addEventListener("apm:page-relay", () => this.reload());
      await this.reload();
    },
    async reload() {
      try {
        const d = await apiFetch("/api/relay/config");
        this.saved = d.networks || [];
        this.caps = d.caps;
        this._status = d.status || this._status;
      } catch (e) { loadErr("加载中继配置失败", e); }
      try {
        const d = await apiFetch("/api/relay/scan-results");
        this.results = d.results || [];
      } catch (e) { /* 还没扫描过，正常 */ }
    },
    apRunning() { return Alpine.store("live").ap?.state === "running"; },
    async scan(force) {
      this.scanning = true;
      try {
        const d = await apiFetch("/api/relay/scan", { method: "POST", body: { force: !!force } });
        this.results = d.results || [];
        if (!this.results.length) Alpine.store("app").toast("没扫到任何网络", "info");
      } catch (e) {
        Alpine.store("app").toast(e.message, "error", 6000);
        if (String(e.message).includes("客户端")) {
          // AP 服务中：给带强制的二次入口
          if (confirm(e.message + "\n\n仍要扫描吗？（会短暂打断在线客户端）")) await this.scan(true);
        }
      } finally { this.scanning = false; }
    },
    openConnect(b) {
      this.dlg = { open: true, ssid: b.ssid, password: "", save: true, needPw: b.security !== "open", busy: false, err: "" };
    },
    async doConnect() {
      this.dlg.busy = true; this.dlg.err = "";
      try {
        await apiFetch("/api/relay/connect", {
          method: "POST",
          body: { ssid: this.dlg.ssid, password: this.dlg.password, save: this.dlg.save },
        });
        this.dlg.open = false;
        Alpine.store("app").toast("已连接 " + this.dlg.ssid, "success");
        this.reload();
      } catch (e) {
        this.dlg.err = e.message;
      } finally { this.dlg.busy = false; }
    },
    async disconnect() {
      if (!confirm("断开中继？期间本机经该 WiFi 的上行会中断。")) return;
      this.disconnecting = true;
      try {
        await apiFetch("/api/relay/connect", { method: "DELETE" });
        Alpine.store("app").toast("已断开中继", "success");
        this.reload();
      } catch (e) { Alpine.store("app").toast("断开失败：" + e.message, "error"); }
      finally { this.disconnecting = false; }
    },
    async forget(ssid) {
      if (!confirm("删除已保存的网络 " + ssid + "？")) return;
      try {
        await apiFetch("/api/relay/networks/" + encodeURIComponent(ssid), { method: "DELETE" });
        this.reload();
      } catch (e) { Alpine.store("app").toast("删除失败：" + e.message, "error"); }
    },
    sigBars(dbm) {
      if (dbm >= -50) return 4;
      if (dbm >= -60) return 3;
      if (dbm >= -70) return 2;
      if (dbm >= -80) return 1;
      return 1;
    },
    secBadge(sec) {
      const m = {
        wpa3: { label: "WPA3", cls: "badge-success" },
        wpa2: { label: "WPA2", cls: "badge-default" },
        wpa: { label: "WPA", cls: "badge-warning" },
        wep: { label: "WEP", cls: "badge-warning" },
        enterprise: { label: "企业级", cls: "badge-destructive" },
        open: { label: "开放", cls: "badge-outline" },
      };
      return m[sec] || { label: sec, cls: "badge-outline" };
    },
  }));

  Alpine.data("pagePorts", () => ({
    ports: [], lan: [], zones: {}, _orig: null, _origLan: null,
    pendingId: null, pendingDeadline: 0, pendingDesc: "",
    _tick: 0,
    portDlg: { open: false, name: "", role: "unused", bridge: "", wanMode: "dhcp", addr: "", gw: "", idx: -1 },
    planDlg: { open: false, loading: false, warns: [], risks: [], errors: [], ack: false },

    async init() {
      await this.reload();
      // 每秒驱动倒计时刷新
      setInterval(() => { this._tick++; if (this.pendingId && Date.now() > this.pendingDeadline) this.reload(); }, 1000);
    },
    async reload() {
      try {
        const d = await apiFetch("/api/net/topology");
        this.ports = (d.ports || []).map((p) => ({ name: p.name, role: p.role, bridge: p.bridge || "", wan: p.wan ? { ...p.wan } : null }));
        this.lan = (d.lan || []).map((s) => ({ ...s, dhcpPool: [...(s.dhcpPool || s.DHCPPool || ["", ""])] }));
        this.zones = d.zones || {};
        this._orig = JSON.stringify(this.ports);
        this._origLan = JSON.stringify(this.lan);
        const pd = d.pending;
        if (pd && pd.id) {
          this.pendingId = pd.id;
          this.pendingDeadline = new Date(pd.deadline).getTime();
          this.pendingDesc = pd.description;
        } else {
          this.pendingId = null;
        }
      } catch (e) { loadErr("加载拓扑失败", e); }
    },
    ifaces() { return Alpine.store("live").ifaces || []; },
    ifaceByName(n) { return this.ifaces().find((i) => i.name === n) || {}; },
    portKindLabel(n) {
      const k = this.ifaceByName(n).kind;
      return { ethernet: "有线", wireless: "无线", bridge: "网桥", loopback: "回环", other: "其他" }[k] || "—";
    },
    roleLabel(r) { return { wan: "WAN", lan: "LAN", unused: "未使用" }[r] || r; },
    dirty() { return this._orig && (JSON.stringify(this.ports) !== this._orig || JSON.stringify(this.lan) !== this._origLan); },

    editPort(p) {
      const i = this.ports.indexOf(p);
      this.portDlg = {
        open: true, name: p.name, role: p.role || "unused",
        bridge: p.bridge || (this.lan.find((s) => s.bridge) || {}).bridge || "",
        wanMode: p.wan?.mode || "dhcp",
        addr: p.wan?.address || "", gw: p.wan?.gateway || "",
        idx: i,
      };
    },
    isValidIP(s) {
      const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s || "");
      return !!m && m.slice(1).every((x) => +x <= 255);
    },
    isValidCIDR(s) {
      const [ip, pfx] = (s || "").split("/");
      if (!pfx) return false;
      const n = +pfx;
      return this.isValidIP(ip) && /^\d{1,2}$/.test(pfx) && n >= 8 && n <= 32;
    },
    savePort() {
      // 静态参数即时校验不通过则不收
      if (this.portDlg.role === "wan" && this.portDlg.wanMode === "static") {
        if (!this.isValidCIDR(this.portDlg.addr) || !this.isValidIP(this.portDlg.gw)) {
          Alpine.store("app").toast("静态地址或网关格式不正确", "error");
          return;
        }
      }
      const p = this.ports[this.portDlg.idx];
      p.role = this.portDlg.role;
      p.bridge = this.portDlg.role === "lan" ? this.portDlg.bridge : "";
      if (this.portDlg.role === "wan") {
        p.wan = { mode: this.portDlg.wanMode };
        if (this.portDlg.wanMode === "static") {
          p.wan.address = this.portDlg.addr;
          p.wan.gateway = this.portDlg.gw;
        }
      } else {
        p.wan = null;
      }
      this.portDlg.open = false;
    },
    addSeg() {
      const n = this.lan.length + 1;
      this.lan.push({
        name: "lan" + n, bridge: "br-lan" + (n > 1 ? n : ""),
        subnet: "192.168." + (50 + n) + ".1/24",
        dhcpPool: ["192.168." + (50 + n) + ".100", "192.168." + (50 + n) + ".200"],
        leaseHours: 12, isolation: false, ap: false,
      });
    },
    delSeg(i) {
      const seg = this.lan[i];
      if (!confirm("删除网段 " + seg.name + "？使用该桥的端口会变成未使用。")) return;
      this.lan.splice(i, 1);
      for (const p of this.ports) {
        if (p.bridge === seg.bridge) { p.bridge = ""; p.role = "unused"; }
      }
    },

    candidate() {
      return {
        ports: this.ports.map((p) => ({ name: p.name, role: p.role, bridge: p.bridge || undefined, wan: p.wan || undefined })),
        lan: this.lan.map((s) => ({
          name: s.name, bridge: s.bridge || "", subnet: s.subnet,
          dhcpPool: [s.dhcpPool[0], s.dhcpPool[1]],
          leaseHours: s.leaseHours, isolation: !!s.isolation, ap: !!s.ap,
        })),
      };
    },
    async openPlan() {
      this.planDlg = { open: true, loading: true, warns: [], risks: [], errors: [], ack: false };
      try {
        const d = await apiFetch("/api/net/plan", { method: "POST", body: this.candidate() });
        this.planDlg.warns = d.warns || [];
        this.planDlg.risks = d.risks || [];
        this.planDlg.errors = d.errors || [];
      } catch (e) {
        this.planDlg.errors = [e.message];
      } finally { this.planDlg.loading = false; }
    },
    async doApply() {
      try {
        const cand = this.candidate();
        cand.acknowledgeRisk = this.planDlg.ack;
        const d = await apiFetch("/api/net/apply", { method: "POST", body: { candidate: cand, description: "网口变更" } });
        this.planDlg.open = false;
        this.pendingId = d.id;
        this.pendingDeadline = new Date(d.pending.deadline).getTime();
        this.pendingDesc = d.pending.description;
        this.reload();
      } catch (e) { Alpine.store("app").toast("应用失败：" + e.message, "error", 6000); }
    },
    pending() { return this.pendingId ? { description: this.pendingDesc } : null; },
    countdown() { return Math.max(0, Math.round((this.pendingDeadline - Date.now()) / 1000)); },
    async confirmApply() {
      try {
        await apiFetch("/api/net/apply/" + this.pendingId + "/confirm", { method: "POST" });
        Alpine.store("app").toast("变更已确认并持久化", "success");
        this.pendingId = null;
        this.reload();
      } catch (e) { Alpine.store("app").toast(e.message, "error"); }
    },
    async rollbackApply() {
      try {
        await apiFetch("/api/net/apply/" + this.pendingId + "/rollback", { method: "POST" });
        Alpine.store("app").toast("已回滚", "info");
        this.pendingId = null;
        this.reload();
      } catch (e) { Alpine.store("app").toast("回滚失败：" + e.message, "error"); }
    },
  }));

  Alpine.data("pageSystem", () => ({
    pw: { old: "", new: "" }, pwErr: "", pwBusy: false,
    memPct() {
      const s = Alpine.store("live").system;
      if (!s.memTotalMB) return 0;
      return Math.round((s.memUsedMB / s.memTotalMB) * 100);
    },
    procs() { return Alpine.store("live").procs || []; },
    async changePw() {
      this.pwBusy = true; this.pwErr = "";
      try {
        await apiFetch("/api/system/password", { method: "PUT", body: this.pw });
        Alpine.store("app").toast("密码已修改，请重新登录", "success");
        setTimeout(() => Alpine.store("app").logout(false), 1200);
      } catch (e) { this.pwErr = e.message; }
      finally { this.pwBusy = false; }
    },
  }));

  Alpine.data("pageLogs", () => ({
    items: [], level: "", loading: false, paused: false, loadErr: "", diag: [],
    async init() {
      window.addEventListener("apm:page-logs", () => { if (!this.loading) this.load(); });
      await this.load();
      this.loadDiag();
    },
    async load() {
      this.loading = true;
      this.loadErr = "";
      try {
        const q = this.level ? "?level=" + this.level : "";
        const d = await apiFetch("/api/logs" + q);
        this.items = d.items || [];
        this.$nextTick(() => { if (this.$refs.box && !this.paused) this.$refs.box.scrollTop = this.$refs.box.scrollHeight; });
      } catch (e) {
        if (!e.silent) this.loadErr = e.message; // 401 静默（跳登录），其余内联显示
      } finally { this.loading = false; }
      this.loadDiag(); // 刷新按钮一并更新诊断面板
    },
    async loadDiag() {
      try {
        const d = await apiFetch("/api/logs/diagnostics");
        this.diag = d.procs || [];
      } catch (e) { if (!e.silent) Alpine.store("app").toast("诊断加载失败：" + e.message, "error"); }
    },
  }));
});

/* ---------------- 概览页流量图（Chart.js 惰性初始化） ---------------- */

let _chart = null;
function pushTrafficSample(d) {
  const live = Alpine.store("live");
  if (!live) return;
  const h = live.history;
  let rx = 0, tx = 0;
  for (const name in (d.ifaces || {})) {
    if (name === "lo") continue;
    rx += d.ifaces[name][0] || 0;
    tx += d.ifaces[name][1] || 0;
  }
  h.ts.push(d.ts || Date.now());
  h.rx.push(rx);
  h.tx.push(tx);
  const max = 300; // 2s × 300 = 10 分钟
  while (h.ts.length > max) { h.ts.shift(); h.rx.shift(); h.tx.shift(); }
  updateChart();
}

function chartColors() {
  // canvas 不能用 CSS 变量，先解析成具体 hsl()
  const css = getComputedStyle(document.documentElement);
  const hsl = (n) => "hsl(" + css.getPropertyValue(n).replace(/^(\d+)\s+(\d+)%\s+(\d+)%.*$/, "$1 $2% $3%") + ")";
  return { line: hsl("--border"), text: hsl("--muted-foreground"), primary: hsl("--primary"), success: hsl("--success") };
}

/* Chart.js 惰性加载：首次需要画图才注入 vendor/chart.umd.js（省 ~60KB gzip 首屏） */
let _chartLib = null;
function loadChartLib() {
  if (window.Chart) return Promise.resolve();
  if (_chartLib) return _chartLib;
  _chartLib = new Promise((res, rej) => {
    const s = document.createElement("script");
    s.src = "vendor/chart.umd.js";
    s.onload = res;
    s.onerror = () => { _chartLib = null; rej(new Error("chart.umd.js 加载失败")); };
    document.head.appendChild(s);
  });
  return _chartLib;
}

function updateChart() {
  const el = document.getElementById("trafficChart");
  if (!el || typeof Chart === "undefined") return;
  // 页面隐藏时 canvas 尺寸为 0：此时初始化会得到 0×0 的死图。
  // 数据已进 history，等概览页激活时 ensureChart() 再画。
  if (el.clientWidth === 0 || el.clientHeight === 0) return;
  const live = Alpine.store("live");
  const h = live.history;
  if (!_chart) {
    // 防御：残留实例占用 canvas（"Canvas is already in use"）
    try {
      const stale = Chart.getChart(el);
      if (stale) stale.destroy();
    } catch (e) { /* Chart 3 无 getChart */ }
    const c = chartColors();
    try {
      _chart = new Chart(el, {
        type: "line",
        data: {
          labels: [],
          datasets: [
            { label: "下载", data: [], borderColor: c.primary, backgroundColor: c.primary.replace(")", " / 0.08)"), fill: true, tension: 0.35, pointRadius: 0, borderWidth: 1.5 },
            { label: "上传", data: [], borderColor: c.success, backgroundColor: c.success.replace(")", " / 0.08)"), fill: true, tension: 0.35, pointRadius: 0, borderWidth: 1.5 },
          ],
        },
        options: {
          responsive: true, maintainAspectRatio: false, animation: false,
          interaction: { mode: "index", intersect: false },
          plugins: {
            legend: { display: false },
            tooltip: { callbacks: { label: (c) => c.dataset.label + " " + fmtRate(c.parsed.y) } },
          },
          scales: {
            x: { ticks: { display: false }, grid: { color: c.line } },
            y: { ticks: { callback: (v) => fmtRate(v), maxTicksLimit: 5 }, grid: { color: c.line } },
          },
        },
      });
    } catch (e) {
      console.warn("chart init 失败", e);
      return;
    }
    // 主题切换时重绘颜色
    const mo = new MutationObserver(() => { if (_chart) { _chart.destroy(); _chart = null; updateChart(); } });
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
  }
  const fmt = (ts) => new Date(ts).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  // 注意：必须传纯数组快照。Chart.js 持有 Alpine 响应式 Proxy 数组时，
  // 读取会触发代理递归（RangeError: Maximum call stack size exceeded），图表永久为空。
  _chart.data.labels = h.ts.map(fmt);
  _chart.data.datasets[0].data = h.rx.slice();
  _chart.data.datasets[1].data = h.tx.slice();
  _chart.update("none");
}

// ensureChart 概览页激活时调用：先确保 Chart.js 已加载，再建图/resize/喂数据
window.ensureChart = function () {
  loadChartLib().then(() => {
    if (_chart) {
      _chart.resize();
      updateChart();
      return;
    }
    updateChart();
  }).catch((e) => console.warn(e.message));
};
window.addEventListener("resize", () => { if (_chart) _chart.resize(); });

/* 全局暴露给模板使用 */
window.fmtBytes = fmtBytes;
window.fmtRate = fmtRate;
window.fmtDur = fmtDur;
window.spinner = spinner;
window.memUsePct = function () {
  const s = window.Alpine ? Alpine.store("live").system : {};
  if (!s || !s.memTotalMB) return 0;
  return Math.round((s.memUsedMB / s.memTotalMB) * 100);
};
window.totalRate = function () {
  const live = window.Alpine ? Alpine.store("live") : null;
  if (!live || !live.ifaces) return { rx: 0, tx: 0 };
  let rx = 0, tx = 0;
  for (const i of live.ifaces) { if (i.name === "lo") continue; rx += i.rxRate || 0; tx += i.txRate || 0; }
  return { rx, tx };
};
window.onlineCount = function () {
  const live = window.Alpine ? Alpine.store("live") : null;
  if (!live) return 0;
  if (live.devSummary && typeof live.devSummary.online === "number") return live.devSummary.online;
  return (live.devices || []).filter((d) => d.online).length;
};
window.knownCount = function () {
  const live = window.Alpine ? Alpine.store("live") : null;
  if (!live) return 0;
  if (live.devSummary && typeof live.devSummary.total === "number") return live.devSummary.total;
  return (live.devices || []).length;
};
