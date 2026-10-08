#!/usr/bin/env bash
# install-deps.sh —— 在目标机安装运行时依赖（hostapd/dnsmasq/nftables/iw 等）。
# 探测 apt/dnf/pacman/zypper，兼容常见发行版；Tailwind CLI 仅构建机需要，单独下载。
set -euo pipefail

log() { echo "[install-deps] $*"; }

have() { command -v "$1" >/dev/null 2>&1; }

PKGS_COMMON="hostapd dnsmasq nftables iw wireless-regdb ethtool"
PKGS_OPTIONAL="ppp rfkill"   # PPPoE / 无线软开关（缺了也能跑，给提示）

if have apt-get; then
    log "检测到 apt（Debian/Ubuntu 系）"
    apt-get update
    # apt 包名映射
    apt-get install -y hostapd dnsmasq nftables iw wireless-regdb ethtool ppp rfkill || true
elif have dnf; then
    log "检测到 dnf（Fedora/RHEL 系）"
    dnf install -y hostapd dnsmasq nftables iw wireless-regdb ethtool ppp rfkill || true
elif have pacman; then
    log "检测到 pacman（Arch 系）"
    pacman -Sy --needed --noconfirm hostapd dnsmasq nftables iw crda ethtool ppp rfkill || true
elif have zypper; then
    log "检测到 zypper（openSUSE 系）"
    zypper --non-interactive install hostapd dnsmasq nftables iw wireless-regdb ethtool ppp rfkill || true
else
    log "未识别的包管理器，请手动安装：$PKGS_COMMON $PKGS_OPTIONAL"
fi

# 关键依赖自检
miss=()
for b in hostapd dnsmasq nft iw; do
    have "$b" || miss+=("$b")
done
if [ ${#miss[@]} -gt 0 ]; then
    log "警告：以下关键命令不可用：${miss[*]}"
    exit 1
fi

# NetworkManager 占用检测：若它在管无线网卡，给出 unmanaged 指引
if systemctl is-active NetworkManager >/dev/null 2>&1; then
    log "检测到 NetworkManager 正在运行。若其管理了目标无线网卡，会导致 hostapd 起不来。"
    log "处理方式（二选一）："
    log "  1) /etc/NetworkManager/conf.d/apmanager.conf 写入："
    log "       [keyfile]"
    log "       unmanaged-devices=interface-name:wlp1s0"
    log "     然后 systemctl restart NetworkManager"
    log "  2) systemctl disable --now NetworkManager"
fi

log "依赖安装完成。"
