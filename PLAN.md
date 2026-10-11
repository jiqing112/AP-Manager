# AP Manager 规划文档

> x86 Linux 小主机 → 无线路由器：Go 单二进制守护进程 + 内嵌 Web UI。
> 核心能力：发射 WiFi、中继上游 WiFi、网口接管（WAN/LAN/桥接/多网段）、设备管理。
> 技术栈（已确定）：Go + net/http、Tailwind CSS（仅构建期）、Alpine.js、SSE、Chart.js、go:embed；
> 底层依赖 hostapd / dnsmasq / nftables / iw / wpa_supplicant / iproute2 / ethtool；目标系统 Debian/Ubuntu。

---

## 0. 目标形态

设备可工作在三种形态（可组合、可切换）：

| 形态 | 上游 | 下游 | 说明 |
|---|---|---|---|
| 有线主路由 | eth 口（DHCP/静态/PPPoE） | WiFi AP + 其他 eth 口（LAN/桥） | 最常见家庭路由形态 |
| 无线中继 | 上游 WiFi（STA 模式） | WiFi AP + LAN 口 | 同一网卡 AP+STA 共存（受硬件限制，见 §7） |
| 纯 AP / 纯 STA | 任意 | 任意 | 可单独启用任一功能 |

非目标（明确不做）：Captive Portal 认证页、Mesh 组网、容器化部署、完整 IPv6（架构预留 nft inet 表与 dnsmasq 扩展位，实施不在本路线图内）。

---

## 1. 关键技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| HTTP 框架 | 标准库 `net/http`（Go 1.22+ 的方法+通配路由） | 零第三方依赖，路由能力已够用 |
| 接口/地址/路由操作 | `github.com/vishvananda/netlink` 库为主，`ip` 命令兜底 | 解析文本输出易碎；netlink 可靠且能拿统计 |
| 无线信息 | exec `iw`（能力/扫描/STA 信息） | 没有维护良好的纯 Go 替代品；输出按节解析 |
| nftables 下发 | 整表生成 → `nft -f` 近原子替换，只管自家的 `table inet apmanager` | 幂等、可导出调试、绝不触碰他人规则 |
| 子进程管理 | 通用 Supervisor（期望态 + 周期对账 + 指数退避重启） | hostapd / dnsmasq / wpa_supplicant / pppd 共用一套 |
| 配置格式 | YAML（`gopkg.in/yaml.v3`），`/etc/apmanager/config.yaml` | 可注释、可手改 |
| 设备限速 | `tc` HTB：下载=LAN/桥出口整形，上传=WAN 出口整形 | 两个方向都是 egress，无需 IFB 镜像，简单可靠 |
| 网络变更加固 | Apply → 确认 → 超时自动回滚（防自锁） | 见 §7「防断网/断 SSH」 |
| Go 第三方依赖 | yaml.v3、netlink、golang.org/x/crypto/bcrypt，共 3 个 | 保持轻量 |
| 前端路由 | 单页 + hash 路由（Alpine 切页），一个 index.html | 首屏快、embed 简单 |
| 前端 JS 资源 | Alpine.js、Chart.js 下载后放 `web/vendor/` 提交进仓库 | 运行时离线可用，无 CDN 依赖 |
| Windows 开发 | `--dev` 模式：非 Linux 平台用 mock 后端（build tag 隔离） | 本机（Windows）可开发调试 UI，目标机真跑 |

---

## 2. 系统架构

### 2.1 分层

```
┌────────────────────────────────────────────────────┐
│  Web UI（Tailwind + Alpine.js + Chart.js，embed 进二进制）│
├────────────────────────────────────────────────────┤
│  HTTP API（net/http，REST + SSE /api/events）        │
├────────────────────────────────────────────────────┤
│  Go 核心守护进程（模块层）                            │
│   ap        AP 生命周期 / hostapd 配置渲染            │
│   relay     STA / 扫描 / wpa_supplicant / 自动重连    │
│   ethport   接口角色 WAN/LAN / 桥 / 多 LAN / 多 WAN    │
│   netrt     DHCP/DNS(dnsmasq) / NAT(nft) / 上游检测   │
│   devices   设备表 / ACL / 限速 / 流量统计             │
│   system    会话 / 状态采集 / 日志环                   │
├────────────────────────────────────────────────────┤
│  统一接口抽象层 iface（所有模块只经它操作网卡）          │
├────────────────────────────────────────────────────┤
│  特权操作层 priv（唯一被允许 exec/netlink/nft 的包）    │
├────────────────────────────────────────────────────┤
│  内核与用户态工具：nl80211 / hostapd / dnsmasq /       │
│  wpa_supplicant / nft / tc / ethtool / iw            │
└────────────────────────────────────────────────────┘
```

### 2.2 统一网络接口抽象层（internal/iface）

所有上层模块（AP、中继、网口、DHCP/NAT、设备管理）通过 `iface.Manager` 操作接口，**不直接调命令**。

数据模型（节选）：

```go
type Kind string     // ethernet | wireless | bridge | ppp | loopback | other
type Role string     // wan | lan | unused
type Iface struct {
    Name, MAC     string
    Kind          Kind
    Role          Role          // 来自 config，抽象层只存不判
    OperState     string        // up / down / lowerlayersdown ...
    Addrs         []net.IPNet
    MTU           int
    SpeedMbps     int           // eth：ethtool；wlan：协商速率另算
    Stats         IfaceStats    // rx/tx bytes, packets, errors, dropped（netlink 读取）
    Wireless      *WirelessInfo // 仅 wlan：频段、信道、regdom、能力矩阵
}
type WirelessInfo struct {
    Bands         []string          // 2.4G / 5G / 6G
    Channels      map[string][]int  // 每频段合法信道
    CapAP, CapSTA bool
    Combo         Combo             // AP-only / STA-only / AP+STA(同信道) / AP+STA(任意信道)
    CurrentChan   int
}
```

职责：
- `List / Get / SetUp / SetDown / SetAddr / SetMTU / AddBridge / AddBridgePort / SetRole`——内部走 netlink（`priv` 层包装）。
- **能力探测**：启动与配置校验时解析 `iw list`（`Supported interface modes`、`valid interface combinations`），产出上面 `WirelessInfo`；AP 模块与中继模块据此过滤可选项、给出友好报错（「硬件不支持的功能要有检测和友好提示」由此层统一承担）。
- **变更事件**：接口 up/down、地址变化、速率统计通过内部事件总线（`internal/bus`，pub/sub）发布，API 层订阅后推给 SSE。

### 2.3 进程模型

`internal/supervisor` 是通用子进程守护，hostapd / dnsmasq / wpa_supplicant / pppd 各一个实例：

- 状态机：`stopped → starting → running → backoff → stopped | failed`。
- 启动流程：渲染配置文件（Go text/template）→ 写入 `/var/lib/apmanager/run/` → 可校验的先校验（`dnsmasq --test`）→ fork 启动 → 3 秒内未退出视为 running，否则 failed（附 stderr 尾部）。
- 崩溃处理：指数退避自动重启（1s/2s/4s/…/上限 30s）；连续 5 次失败进入 `failed`，不再重启，UI 红色告警并展示日志尾部（诊断面板）。
- 优雅停止：SIGTERM → 10s 超时 SIGKILL；以进程组管理，防孤儿。
- hostapd / wpa_supplicant 开 unix 控制 socket（`ctrl_interface`），Go 侧直连读状态、STA 列表、事件（断连/连入），踢人/拉黑也走它；dnsmasq 无控制协议，读 leases 文件 + SIGHUP。
- **全局变更串行化**：所有改网络的操作（AP 重启、nft 重下发、角色变更、tc 重建）进入一个互斥事务队列执行，避免交叉产生半成品状态。
- **开机对账**：进程启动后把系统实际状态收敛到 `config.yaml` 的期望态（对账循环每 5s 巡检：进程活着？接口角色对？规则在？），这是「systemd 拉起后全功能自动恢复」的基础。

### 2.4 文件与数据布局

| 路径 | 内容 |
|---|---|
| `/etc/apmanager/config.yaml` | 期望配置（用户/API 可改）：AP、中继、接口角色、网段、ACL、上游优先级 |
| `/var/lib/apmanager/state.json` | 运行状态：会话、设备别名、流量累计值、apply/回滚日志、tc 句柄分配（10s 防抖落盘 + 原子写 rename） |
| `/var/lib/apmanager/run/` | 生成的 hostapd.conf / dnsmasq.conf / wpa_supplicant.conf、pid、控制 socket（tmpfiles.d 创建） |
| `/var/lib/apmanager/dnsmasq.leases` | DHCP 租约（dnsmasq 指定路径） |
| `/var/log/apmanager/app.log` | 自身日志（slog，按 10MB 轮转留 3 份）；systemd 部署时同时进 journald |

### 2.5 权限处理

- 推荐以 **root + systemd** 运行（hostapd/nft/tc 均需 CAP_NET_ADMIN）；附录提供两种 unit：root 版与最小 capabilities 版（`CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE`，二进制打 file caps 的备选说明）。
- **`internal/priv` 是全项目唯一允许特权操作的地方**：`priv.Cmd()`（exec 包装：超时、审计日志、`--dry-run` 只打印不执行）、netlink 操作、nft 下发。API 层及各业务模块禁止直接 exec——需要特权动作一律声明函数放进 priv。
- 所有变更操作（谁、何时、改了什么）写审计日志，日志页可查。

### 2.6 事件与状态流（SSE 的来源）

```
iface/netrt/ap/relay/devices/system ──事件──▶ internal/bus ──▶ api/sse Hub ──▶ 浏览器
                                                  ▲
        定时采样（2s 系统状态 / 2s 流量 / 5s 上游探测）──┘
```

模块只管发事件（如 `device.joined`、`ap.client.deauth`、`upstream.switched`），SSE Hub 按 topic 扇出；浏览器端一个 `EventSource` 订阅多 topic。

---

## 3. 功能清单

### 3.1 AP 发射模块（internal/ap）

| 功能 | 做什么 | 怎么实现 |
|---|---|---|
| 启动/停止/重启 | 按 config 渲染 hostapd.conf 并管理进程 | Supervisor 实例；重启=写新配置→SIGTERM→重拉；控制 socket（`ctrl_interface`）读 `STATUS` |
| SSID / 密码 / 加密 | WPA2-PSK、WPA3-SAE、WPA2/WPA3 混合 | `wpa=2`；`wpa_key_mgmt=WPA-PSK / SAE / "WPA-PSK SAE"`；`rsn_pairwise=CCMP`；`ieee80211w=1/2`。密码 8–63 位在 API 层校验；SAE 需 hostapd≥2.9，启动时探测版本降级并提示 |
| 信道 / 频段 / 带宽 | 2.4G（20/40MHz）、5G（20/40/80MHz）、自动选信道 | `hw_mode=g/a`、`ieee80211n/ac=1`、`ht_capab`、`vht_oper_chwidth`、`channel=N / acs_survey`。**可选项先经 iface 能力过滤**，不合法组合在 UI 直接禁用并说明 |
| 隐藏 SSID | 不广播 SSID | `ignore_broadcast_ssid=1` |
| 最大连接数 | 限制接入客户端数 | `max_num_sta=N`（1–64，默认 32），与 dnsmasq 池大小联动校验 |
| 多 SSID（可选） | 同一射频多 BSS（如主网络 + 访客） | 单配置多 `bss=` 段生成虚拟接口；访客 BSS 加 `ap_isolate=1`。做能力检测，不支持则隐藏入口 |
| 客户端事件 | 连入/断开实时上屏 | hostapd 控制接口事件流 → 事件总线 → SSE |
| 国别码 | 合法信道/功率 | `country_code=CN` + `ieee80211d=1`，启动时 `iw reg set` |

### 3.2 中继模块（internal/relay）

| 功能 | 做什么 | 怎么实现 |
|---|---|---|
| 扫描周围 WiFi | 列出 SSID/信号/信道/加密 | `iw dev wlan0 scan trigger` + `scan dump`，按 BSS 块解析。AP 运行时扫描会短暂离信道扰民：默认手动触发 + 客户端数为 0 时才允许自动扫描（60s） |
| 连接上游 | STA 模式关联上游 AP | 为该 iface 起 wpa_supplicant（独立 socket）；`network={ ssid psk key_mgmt priority }`，支持保存多个上游按优先级切换；WPA3 上游用 `key_mgmt=SAE` |
| AP + STA 共存 | 一张网卡边中继边发射 | iface 能力探测判定 Combo；**同信道限制的网卡**：连接后自动把 AP 信道对齐到上游信道（短暂重启 AP，UI 预先说明并征得勾选）；任意信道网卡直接共存 |
| 中继失败自动重连 | 上游断开后自愈 | 监听 wpa_supplicant `CTRL-EVENT-DISCONNECTED`：指数退避重连同一上游；连续 3 次失败触发重扫，切换到次优先级已存上游；全程事件上屏 |
| 中继态 NAT | 中继客户端共享上游 | STA 接口视为一类 WAN：nft masquerade `oifname wlan0`（与有线 WAN 规则同表管理） |

### 3.3 网口管理模块（internal/ethport）

| 功能 | 做什么 | 怎么实现 |
|---|---|---|
| 接口列表 | eth*/wlan*/br*/ppp* 全量展示：状态、速率、IP、MAC、收发/错误计数 | netlink dump + ethtool 取速率双工；2s 采样差分出实时速率 |
| 角色分配 | 每个口标 wan / lan / unused | 角色存 config；变更走「安全应用」事务（§7）。启动时检测 NetworkManager/systemd-networkd 是否也在管这些口，冲突则给出一键 unmanaged 指引 |
| WAN 接入 | DHCP / 静态 IP / PPPoE | DHCP：exec `dhclient -1`（Debian/Ubuntu 自带）；静态：netlink 设地址+路由；PPPoE：生成 `/etc/ppp/peers/apmanager`，pppd 由 Supervisor 托管（阶段六） |
| LAN 网段 | 网桥 IP、子网、DHCP 池、DNS | netlink 建桥并挂端口、桥上配静态 IP；dnsmasq 按 `interface=br-x` 出 `dhcp-range`；DNS 指向本机 |
| 多 LAN 隔离 | 家庭 / IoT / 访客 / 自定义网段 | 每网段一桥一子网一池；nft forward 链按「区域策略矩阵」放行/拦截（默认 LAN→WAN 通、IoT→LAN 断、访客→仅 WAN）；访客额外开客户端互访隔离（子网内 saddr=daddr 同段 drop） |
| 多口桥接 | 多个 eth 口桥成一个交换机 | netlink BridgeAdd + 端口 enslaved；管理 IP 在桥上；**所有 DHCP/NAT 规则按桥名下发**（详见 §7 桥接坑位） |
| 多 WAN（进阶） | 负载均衡与故障切换 | 阶段六：先做健康探测 + 主备切换（多默认路由 metric + 探测 5s/连续 3 败切换）；负载均衡用 nft mark（jhash/numgen）+ 策略路由，标注为实验特性 |
| 接口流量统计 | 每口实时速率与累计 | netlink 统计 2s 差分 → 事件总线（SSE + Chart.js 历史 10 分钟环形缓冲） |

### 3.4 网络与路由模块（internal/netrt）

| 功能 | 做什么 | 怎么实现 |
|---|---|---|
| DHCP 服务 | 每 LAN 段独立地址池、静态租约 | 单个 dnsmasq 进程，配置从所有 LAN 段渲染（`dhcp-range`/`dhcp-host=MAC,IP`/`dhcp-host=MAC,ignore` 用于黑名单拒发地址）；`--test` 校验后 Supervisor 拉起 |
| NAT / 转发 | 上网共享 + 区域策略 | 只写自家 `table inet apmanager`：input/forward/postrouting 三条基础链 + 每区域链；整表生成后 `nft -f` 近原子替换；sysctl `ip_forward=1` 由守护确保 |
| DNS 服务 | LAN 客户端 DNS 转发 + 本机名（apmanager.lan） | 同一 dnsmasq：`no-resolv` + 上游取自 WAN DHCP option / 中继上游 / 手填；`address=/apmanager.lan/<IP>` |
| 上游检测与切换 | 有线 / WiFi 中继自动选择 | 每上游探测（多目标 ICMP/HTTP，5s 间隔，3 连败判定 down，3 连胜恢复，防抖）；优先级 config 可调（默认 有线 > 中继）；切换产生 `upstream.switched` 事件，概览页横幅提示 |

### 3.5 设备管理模块（internal/devices）

| 功能 | 做什么 | 怎么实现 |
|---|---|---|
| 设备列表 | IP、MAC、主机名、信号、连接时长、上下行流量、来源接口（有线/无线） | 多源按 MAC 聚合：dnsmasq leases（IP/主机名）+ netlink neigh + 桥 FDB（判有线口）+ hostapd STA（无线：信号/速率/在线秒数）+ nft 计数（流量） |
| 别名/备注 | 给设备起名 | state.json 按 MAC 存，UI 即时生效 |
| 流量按设备统计 | 每设备上下行字节 | nft 动态集合（`flags dynamic`，元素带 counter，nft≥0.9.4）在 forward 链按源/目的 IP 双向计数，2s 轮询聚合；累计值落盘防重启丢失。硬件/版本不支持时降级为「仅总量」并提示 |
| 限速 | 按设备上下行（如 下行 20Mbps / 上行 5Mbps） | `tc` HTB：下载=LAN 桥 egress 按 `match ip dst` 分类，上传=WAN egress 按 `match ip src`；每设备一对 class+filter，句柄分配记 state；变更即重建对应 qdisc 树 |
| 踢下线 | 立即断开 | 无线：hostapd 控制 socket `DEAUTHENTICATE <mac>`；有线口无法去认证，等价实现为防火墙阻断并明确提示 |
| 拉黑/白名单 | MAC 级准入控制 | 拉黑：写 deny_mac 文件 + hostapd 重载 + dnsmasq `dhcp-host=MAC,ignore` + nft drop（对有线也生效）；白名单模式：`macaddr_acl=1` 仅许可名单可关联 |

### 3.6 系统与安全模块（internal/system）

| 功能 | 做什么 | 怎么实现 |
|---|---|---|
| 管理密码 | 单管理员，首跑生成随机密码 | bcrypt 哈希存 state；首次启动随机密码打印到控制台并写入 root-only 文件，首次登录强制改密；登录限速 5 次/分钟/IP |
| 会话 | 登录后免密 7 天（可滑动续期） | HMAC 签名 Cookie（HttpOnly、SameSite=Lax）；改密后其他会话全部失效；变更类 API 校验 `X-Requested-With` 头（轻量 CSRF 防护） |
| 基础防火墙 | WAN 侧只进不出 | nft input 链：WAN 口默认 drop，仅放行 established 与 ICMP；管理界面端口仅 LAN 可达 |
| 日志查看 | 级别/来源过滤、自动滚动、下载 | slog 双写（文件+journald）+ 内存环形缓冲最近 2000 条；子进程 stderr 由 Supervisor 捕获成独立环形缓冲用于诊断 |
| 系统状态 | CPU、内存、温度、负载、磁盘、uptime | `/proc/stat` 差分、`/proc/meminfo`、`/sys/class/thermal`、`/proc/loadavg`；2s 采样 → SSE |

### 3.7 Web UI 模块

**页面清单与 API/SSE 对照：**

| 页面（hash 路由） | 主要内容 | 依赖 API | SSE topics |
|---|---|---|---|
| `#/` 概览 | AP 状态卡、上游状态卡、在线设备数、实时流量图（Chart.js 面积图）、接口速览条、快捷启停开关 | `GET /api/overview` | status、traffic、devices |
| `#/ap` AP 设置 | SSID/密码/加密/信道/带宽/隐藏/最大客户端表单；启停按钮；硬件能力提示条 | `GET/PUT /api/ap/config`、`POST /api/ap/{start,stop,restart}`、`GET /api/ap/capabilities` | status |
| `#/relay` 中继 | 扫描结果表（信号/加密徽标/信道）、连接表单、当前上游状态卡、重连策略设置、已存上游列表 | `POST /api/relay/scan`、`GET /api/relay/scan-results`、`POST /api/relay/connect`、`DELETE /api/relay/connect`、`GET/PUT /api/relay/config` | status |
| `#/ports` 网口 | 接口卡片（角色徽标、IP、速率、实时流量）；角色编辑 Dialog（WAN：DHCP/静态/PPPoE；LAN：子网/池/DNS）；桥编辑；**安全应用确认倒计时** | `GET /api/net/topology`、`POST /api/net/apply`、`POST /api/net/apply/{id}/confirm`、`POST /api/net/apply/{id}/rollback` | status、traffic |
| `#/devices` 设备 | 设备 Table（别名、IP/MAC、来源、信号、在线时长、限速徽标、流量）、行操作菜单（备注/限速/踢/拉黑）、ACL 管理 Dialog | `GET /api/devices`、`PUT /api/devices/{mac}/{alias,rate}`、`POST /api/devices/{mac}/{kick,block}`、`DELETE /api/devices/{mac}/block`、`GET/PUT /api/acl` | devices |
| `#/system` 系统 | CPU/内存/温度图表、密码修改 Dialog、防火墙开关、关于/能力信息 | `GET /api/system/status`、`PUT /api/system/password`、`GET /api/system/capabilities` | system |
| `#/logs` 日志 | 级别与来源过滤、暂停滚动、下载、诊断面板（各子进程 stderr 尾部） | `GET /api/logs?source=&level=`、`GET /api/logs/diagnostics` | logs |
| `#/login` 登录 | 密码表单、首跑改密流程 | `POST /api/login`、`PUT /api/system/password` | — |

（另：`GET /api/events?topics=…` 为唯一 SSE 端点；未列出的 `GET /api/session` 用于前端启动时校验登录态与取能力集。）

**SSE 推送内容：**

| topic | 频率 | 载荷 |
|---|---|---|
| status | 事件驱动（去抖 500ms） | ap 状态/信道/客户端数、relay 状态/上游信号、upstream 类型与健康、接口概要 |
| traffic | 2s | 总 rx/tx 速率 + 每接口速率（喂图表） |
| devices | 变化或 5s | 设备列表全量快照（当前规模下全量推送最简单可靠） |
| system | 2s | cpu%、mem、温度、负载 |
| logs | 实时 | 新日志行（当前过滤条件下的） |

**前端组件清单（照 shadcn 样式手写）：**
Button（primary/secondary/outline/ghost/destructive × sm/default × loading）、Card（Header/Title/Description/Content/Footer）、Badge（default/success/warning/destructive/outline）、Table（含排序与行操作列）、Dialog（表单式 + 确认式，确认式支持危险红样式与倒计时）、Input/Label/Select/Switch/Textarea、Toast（success/error/info + 可选撤销动作）、Tabs、Skeleton、EmptyState、Alert（info/warning/destructive）、DropdownMenu、Tooltip、Progress、Slider（限速）、SegmentedControl（频段/加密选择）、StatusDot（呼吸动画）、StatCard、ChartCard、页头导航（侧栏 + 移动端抽屉）、内联 SVG 图标集（lucide 子集，统一 1.5px 描边、16/20/24 三档）。

---

## 4. 前端与构建方案

### 4.1 tailwind.config.js 思路

```js
module.exports = {
  content: ["./web/index.html", "./web/app.js"],   // 扫描 HTML 与 JS 里的类名
  darkMode: "class",                                // .dark 切换
  theme: {
    container: { center: true, padding: "1.5rem" },
    extend: {
      colors: {   // 全部指向 CSS 变量，明暗两套只需换变量值
        border: "var(--border)", input: "var(--input)",
        background: "var(--background)", foreground: "var(--foreground)",
        primary: { DEFAULT: "var(--primary)", foreground: "var(--primary-foreground)" },
        secondary: {...}, muted: {...}, accent: {...},
        destructive: {...}, success: {...}, warning: {...},
        card: {...}, popover: {...},
      },
      borderRadius: { lg: "var(--radius)", md: "calc(var(--radius) - 2px)", sm: "calc(var(--radius) - 4px)" },
      boxShadow: { xs: "0 1px 2px 0 rgb(0 0 0 / 0.05)" },   // 只留极轻两档
      keyframes/animation: { fade-in, slide-up, scale-in, shimmer }  // Dialog/Toast/Skeleton 用
    }
  }
}
```

### 4.2 input.css（CSS 变量 + 组件层）

- `:root` 定义亮色：背景 `hsl(0 0% 100%)`、前景 zinc-950、muted zinc-100、边框 zinc-200、主色蓝 `hsl(221 83% 53%)`、success emerald、warning amber、destructive red；`--radius: 0.5rem`。
- `.dark` 同名变量换暗值：背景 zinc-950、卡片 zinc-900、边框 zinc-800、主色亮一档。
- `@layer components`：`.btn`、`.card`、`.badge`、`.input`、`.table`、`.dialog-overlay/dialog-panel`、`.toast`、`.switch` 等手写类（@apply 组合 token），HTML 里不再堆散类。
- 全局 `::selection`、focus-visible 焦点环、`scroll-behavior`、`@media (prefers-reduced-motion: reduce)` 关闭动效。

### 4.3 构建命令与产物

```bash
# Tailwind 独立 CLI（单文件二进制，Windows/Linux 通用，无需 npm 项目）
./tailwindcss -c web/tailwind.config.js -i web/src/input.css -o web/dist/app.css --minify
# 监听模式（开发）
./tailwindcss -c web/tailwind.config.js -i web/src/input.css -o web/dist/app.css --watch
```

产物固定为 `web/dist/app.css`（预计 gzip 后 < 20KB）。版本锁定（v3.4.x），由 `scripts/install-deps.sh` 下载到项目根或 bin 目录。

### 4.4 go:embed 打包

`web/embed.go`（与资源同目录，规避 go:embed 不能引用上级目录的限制）：

```go
//go:embed all:dist all:vendor index.html app.js
var assets embed.FS
```

`main` 里 `import _ ".../web"`，API 层用 `http.FileServerFS` 把 `/` 指到子 FS（`fs.Sub`）；SPA fallback 到 index.html。Makefile 保证顺序：`css → build`，并对 `web/dist/app.css` 做存在性检查（防忘跑 CSS 就 build）。

### 4.5 Alpine.js 与 SSE 驱动

- `app.js` 结构：`Alpine.store('app')`（登录态、暗色、toast 队列、路由）、`Alpine.store('live')`（SSE 喂入的全部实时数据）、`Alpine.data('pageXxx')` 各页面组件（表单校验、Dialog 开关、行操作）。
- hash 路由：`window.addEventListener('hashchange')` → store 切页 → 惰性 fetch 首屏数据，之后由 SSE 持续刷新，**页面不再自己轮询**。
- `fetch` 封装：401 跳登录、loading 状态、错误 toast、`X-Requested-With` 头。
- Chart.js 仅在概览与系统页惰性初始化，颜色取 CSS 变量、细线、无渐变。

### 4.6 Makefile

```makefile
css:        ## 生成 web/dist/app.css（--minify）
css-watch:  ## 监听模式
build: css  ## go build -trimpath -ldflags "-s -w" -o bin/apmanager .
dev:        ## go run . --dev（Windows/非 root 下 mock 后端，专供 UI 开发）
release:    ## CGO_ENABLED=0 GOOS=linux GOARCH=amd64 交叉编译
check:      ## go vet + embed 资源完整性检查
clean:
```

---

## 5. 项目目录结构

```
ap-manage/                        # 当前工作区即项目根
├── main.go                       # 入口：flag 解析、root/--dev 检查、依赖装配、优雅退出
├── go.mod
├── Makefile
├── PLAN.md                       # 本文档
├── README.md                     # 部署与使用文档（阶段六）
├── scripts/
│   ├── install-deps.sh           # apt 装 hostapd/dnsmasq/nftables/iw/ethtool/ppp + 下载 Tailwind CLI
│   ├── install.sh / uninstall.sh # 二进制落位 + systemd 装卸
├── deploy/
│   └── apmanager.service         # systemd unit（root 版；capabilities 变体以注释给出）
├── internal/
│   ├── config/                   # config.yaml 读写、校验、默认值、版本迁移
│   ├── bus/                      # 进程内事件总线（pub/sub，SSE 数据源）
│   ├── priv/                     # ★ 唯一特权层
│   │   ├── cmd.go                #   exec 包装：超时/审计/dry-run
│   │   ├── netlink_linux.go      #   接口/地址/路由/桥/统计（build tag linux）
│   │   ├── nft.go                #   规则集生成与原子下发
│   │   ├── tc.go                 #   限速 qdisc/class/filter
│   │   └── mock.go               #   非平台/开发 mock（build tag !linux 或 --dev）
│   ├── iface/                    # ★ 统一接口抽象层（模型、枚举、能力探测 iw 解析、事件）
│   ├── supervisor/               # 通用子进程守护（状态机/退避/日志环）
│   ├── ap/                       # AP：hostapd 配置渲染、生命周期、控制 socket 客户端
│   ├── relay/                    # 中继：扫描、wpa_supplicant 控制、重连状态机
│   ├── ethport/                  # 网口：角色事务、WAN 接入、桥、多 LAN、多 WAN
│   ├── netrt/                    # dnsmasq 渲染、nft 区域策略、上游健康探测
│   ├── devices/                  # 设备聚合表、别名、ACL、限速、流量
│   ├── system/                   # 会话/密码、状态采集、日志环、审计
│   └── api/                      # 路由、鉴权中间件、handler、SSE Hub、静态挂载
└── web/
    ├── embed.go                  # //go:embed all:dist all:vendor index.html app.js
    ├── index.html                # 单页骨架（语义化标签 + Alpine 模板）
    ├── app.js                    # store/路由/SSE 客户端/页面组件/toast
    ├── tailwind.config.js
    ├── src/input.css             # CSS 变量 + 组件层
    ├── dist/app.css              # 构建产物（make css 生成，Makefile 检查存在性）
    └── vendor/                   # alpine.min.js、chart.umd.js（本地化提交）
```

（用户参考稿中的 `internal/net` 命名为 `netrt`、`internal/eth` 命名为 `ethport`，避免与标准库语义混淆；新增 `priv`、`supervisor`、`bus`、`config` 四个支撑包。）

---

## 6. 实施路线图

| 阶段 | 交付 | 怎么验证 |
|---|---|---|
| **一：最小可用** | 项目骨架；priv/iface/supervisor；AP 启停（hostapd）；dnsmasq DHCP；nft NAT；有线 WAN（DHCP）；概览+AP 两页基础版；登录；Makefile；`--dev` mock 模式；install-deps.sh | 盒子上 `sudo ./apmanager`，手机搜到 SSID、拿到 192.168.x 地址、能上网；UI 能看到该设备并启停 AP；`make release` 产物在干净 Debian 12 跑通 |
| **二：中继** | 扫描、STA 连接、AP+STA 共存与信道对齐、自动重连、中继页 | 拔掉有线，走 WiFi 上游仍可上网；重启上游路由后 30s 内自动恢复；UI 显示信号与切换事件 |
| **三：网口管理** | 角色分配（WAN/LAN）、WAN 静态、LAN 桥+子网、多 LAN+隔离矩阵、多口桥接、安全应用/自动回滚 | 双网口盒子改角色不断 SSH（确认/回滚机制生效）；家庭/IoT/访客三网段用两台终端互 ping 验证隔离；桥后 DHCP 正常发放 |
| **四：设备与限速** | 有线+无线设备聚合、别名、tc 限速、踢/拉黑/白名单、每设备流量 | 限速后 speedtest 结果≈设定值（±15%）；踢设备手机立刻掉线；黑名单设备重连被拒；换网口接入的设备来源标识正确 |
| **五：UI 完善 + SSE** | SSE 全量接入替代轮询；Chart.js 图表；全套 shadcn 风格组件；暗色模式；空/载/错状态；移动端适配；**按品质清单做 3 轮自检迭代**（每轮列出达标/未达标与改动） | 品质清单逐条核对并留记录；Lighthouse ≥95；375px 宽不崩；console 零报错；断网重连后 SSE 自动恢复 |
| **六：打磨** | 日志页完善、systemd 自启、install/uninstall 脚本、PPPoE、多 WAN 故障切换（负载均衡为实验特性）、README、性能与安全加固 | 断电重启全自动恢复；journald 与 UI 日志一致；双 WAN 拔线 15s 内切换；`nft list table inet apmanager` 可读且幂等 |

---

## 7. 风险与坑（附对策）

| # | 风险 | 检测 | 对策 |
|---|---|---|---|
| 1 | 网卡不支持 AP 模式 | `iw list` 的 `Supported interface modes` 是否含 `AP` | 启动时能力探测；不支持则 AP 页整页锁定并给出明确提示与建议（换网卡/仅中继） |
| 2 | AP+STA 共存的信道限制 | `iw list` 的 `valid interface combinations`：若 AP 与 STA 分列且 `#channels <= 1`，则仅同信道可共存 | Combo 探测；同信道型自动「跟随上游信道」对齐 AP（重启 AP 前征询）；不支持共存则给出明确提示。文档注明：5GHz 双频卡通常限制多，2.4G 或部分驱动更宽松 |
| 3 | 发行版差异 | 探测 hostapd 版本（SAE/多 BSS 能力）、NM/networkd 是否在管、nft 版本（动态 set counter 需 ≥0.9.4） | 只承诺 Debian 11+/Ubuntu 20.04+；install-deps.sh 统一装包；冲突时给出一键 unmanaged；能力降级 + UI 提示，而不是报错退出 |
| 4 | 权限与 capabilities | 启动时 `effective uid` 与所需 caps 自检 | 推荐 root + systemd；capabilities 最小集作为附录方案；`--dry-run` 让非 root 环境可演练 |
| 5 | 把自己的 SSH/管理连接搞断 | 客户端来源 IP 所在网段可识别 | **安全应用机制**：变更前自动生成回滚快照（接口配置 + nft 导出）→ 应用 → UI/命令行 60s 确认倒计时 → 未确认自动回滚（systemd-run 一次性 timer 兜底，即使守护进程被杀也回滚）；确认后固化。持有管理会话的网段对应的接口改角色需显式勾选「我知道风险」 |
| 6 | nftables 与 iptables 共存 | 启动时读 `nft list ruleset` 检测 iptables-legacy 残留与 ufw | 只操作自家表，绝不 flush 全局；检测到 ufw/legacy 规则给告警与建议（路由机上关闭 ufw 或放行 forward） |
| 7 | embed 路径/顺序 | 构建期检查 | embed 脚本与资源同目录；Makefile `build` 依赖 `css`；`make check` 校验 dist/app.css 存在且非空；CI 先 css 后 go build |
| 8 | 桥接后 DHCP/NAT 不生效 | — | 约定：成员口不配 IP，全部地址/池/规则挂桥名；dnsmasq `interface=br-x` + `bind-dynamic`；处理 `br_netfilter` 场景（若开启，forward 链内补「同桥内互访放行」规则避免二次过滤）；`nft list ruleset` 幂等可查 |
| 9 | 扫描/信道对齐打断在线客户端 | hostapd 客户端数 | 有客户端时默认禁止自动扫描；信道对齐在 UI 明示「会瞬断几秒」 |
| 10 | SD/eMMC 写磨损 | — | state.json 防抖 10s + 原子写；流量累计等高频数据仅内存 + 低频落盘；日志轮转限量 |
| 11 | rfkill / regdom 导致发不出 WiFi | `rfkill list`、`iw reg get` | 启动自检并 `rfkill unblock all`、按配置 `iw reg set`；被软锁时 UI 明确提示 |

---

## 8. 品质标准落实（获奖级 UI 的工程化保障）

**设计系统先行**（阶段一即建，阶段五精修）：一套 token（字号阶 12/14/16/20/24/30 + 对应行高字重；spacing 全程用 Tailwind scale 不出现魔法数；圆角统一 8px 衍生；阴影仅 shadow-xs/sm；图标 lucide 子集统一描边），所有组件只吃 token，杜绝「每个页面自己长样式」。

**自检迭代机制**（阶段五固定 3 轮，之后每次改 UI 顺手核对）：
- 第 1 轮「信息架构与层级」：概览五要素一眼可见（AP、上游、设备数、流量、网口）；导航当前位置；排版层级重排。
- 第 2 轮「状态与交互」：每个按钮有 loading/disabled、每个列表有空/载/错三态、每个危险操作有确认或撤销、Dialog/Toast 动效（150–200ms，ease-out）、表单即时校验文案。
- 第 3 轮「细节与可达性」：暗色两套全量核对对比度（正文 ≥4.5:1）、focus-visible 全覆盖、键盘走查（Tab 顺序/Enter 提交/Esc 关弹窗）、375px 与 1440px 双端截图走查、性能（首屏 <50KB CSS+JS gzip、无 CLS）。
- 每轮输出：改了什么 / 对照清单哪些达标 / 哪些未达标及取舍原因 / 下一轮计划——不写「已优化」四个字糊弄。

---

## 9. 附录

### 9.1 依赖安装（scripts/install-deps.sh 概要）

```bash
apt-get update && apt-get install -y hostapd dnsmasq nftables iw wireless-regdb ethtool
# 可选：ppp pppoe（PPPoE）、isc-dhcp-client（默认已有）
# NetworkManager 在管的网卡需 unmanaged（脚本检测后给指引或自动处理）
# Tailwind 独立 CLI 下载（仅构建机需要）
```

### 9.2 systemd unit（deploy/apmanager.service）

```ini
[Unit]
Description=AP Manager - wireless router daemon
After=network.target
[Service]
ExecStart=/usr/local/bin/apmanager -config /etc/apmanager/config.yaml
Restart=on-failure
RestartSec=3
# 可选加固：Capabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE / AmbientCapabilities=...
[Install]
WantedBy=multi-user.target
```

### 9.3 config.yaml 示例（最终形态）

```yaml
ap:
  enabled: true
  interface: wlan0
  ssid: Home
  password: "********"
  security: wpa2wpa3        # wpa2 | wpa3 | wpa2wpa3
  band: 5g                  # 2g | 5g | auto
  channel: auto             # auto | 1-13 / 36-165
  bandwidth: 80mhz          # 20mhz | 40mhz | 80mhz
  hidden: false
  max_clients: 32
  follow_upstream_channel: true   # 中继时对齐信道
relay:
  enabled: false
  networks:                 # 已存上游，按优先级
    - { ssid: UpA, password: "***", priority: 1 }
  reconnect: { backoff_max: 30s, rescan_after: 3 }
upstreams:
  priority: [eth-wan, wifi-relay]
  check: { interval: 5s, fail_threshold: 3 }
ports:
  - { name: eth0, role: wan, wan: { mode: dhcp } }
  - { name: eth1, role: lan, bridge: br-lan }
  - { name: wlan0, role: unused }
lan:
  - name: home
    bridge: br-lan
    subnet: 192.168.50.1/24
    dhcp: { pool: [192.168.50.100, 192.168.50.200], lease: 12h }
    isolation: false
  - name: guest
    bridge: br-guest
    subnet: 192.168.60.1/24
    dhcp: { pool: [192.168.60.10, 192.168.60.100], lease: 2h }
    isolation: true
zone_policy: { guest_to_lan: deny, iot_to_lan: deny, lan_to_lan: allow }
admin: { port: 8080 }
```

---

*下一步：等确认本规划后按阶段一开始实施。*
