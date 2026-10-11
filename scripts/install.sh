#!/usr/bin/env bash
# install.sh —— 安装 apmanager 到本机：二进制 + systemd unit + 目录结构
# 用法：在项目根目录执行 ./scripts/install.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DST=/usr/local/bin/apmanager
UNIT_SRC="$ROOT/deploy/apmanager.service"
UNIT_DST=/etc/systemd/system/apmanager.service
CFG_DIR=/etc/apmanager
STATE_DIR=/var/lib/apmanager
LOG_DIR=/var/log/apmanager

log() { echo "[install] $*"; }

# 0. RTL8821CE 无线稳定性配置（实测：disable_aspm 消除随机掉卡；
#    disable_lps_deep 缓解深度省电导致的吞吐骤降，需重启生效）
if lspci -nn 2>/dev/null | grep -qi 'RTL8821CE'; then
    if ! grep -q 'rtw88_pci disable_aspm' /etc/modprobe.d/rtw88.conf 2>/dev/null; then
        cat > /etc/modprobe.d/rtw88.conf <<'EOF'
# apmanager：RTL8821CE 稳定性（2026-10 实测）
options rtw88_pci disable_aspm=1
options rtw88_core disable_lps_deep=Y
EOF
        log "已写入 /etc/modprobe.d/rtw88.conf（disable_lps_deep 需重启生效）"
    else
        log "rtw88.conf 已存在，跳过"
    fi
fi

# 1. 二进制（没有就现构建）
if [ ! -x "$ROOT/bin/apmanager" ]; then
    log "bin/apmanager 不存在，先构建…"
    make -C "$ROOT" build
fi
install -m 0755 "$ROOT/bin/apmanager" "$BIN_DST"
log "二进制 → $BIN_DST"

# 2. systemd unit
install -m 0644 "$UNIT_SRC" "$UNIT_DST"
log "unit → $UNIT_DST"

# 3. 目录（已存在则跳过；已有配置绝不覆盖）
mkdir -p "$CFG_DIR" "$STATE_DIR/run" "$LOG_DIR"
if [ ! -f "$CFG_DIR/config.yaml" ]; then
    log "无既有配置，首跑将由守护进程生成默认配置"
fi

# 4. 依赖自检（缺了给提示不阻塞）
for b in hostapd dnsmasq nft iw; do
    command -v "$b" >/dev/null || log "警告：$b 不可用，请先跑 scripts/install-deps.sh"
done

# 5. 使能并启动
systemctl daemon-reload
systemctl enable apmanager.service
systemctl restart apmanager.service
log "已启动：systemctl status apmanager"
log "管理界面：http://<本机IP>:$(grep -A2 '^admin:' "$CFG_DIR/config.yaml" 2>/dev/null | grep port | awk '{print $2}' || echo 8080)/"
log "首跑初始密码：$STATE_DIR/state.json.initial_password（首次登录强制修改）"
