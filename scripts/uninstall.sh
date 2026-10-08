#!/usr/bin/env bash
# uninstall.sh —— 卸载 apmanager：停服务、撤 unit、删二进制。
# 配置与状态（/etc/apmanager、/var/lib/apmanager、日志）默认保留，--purge 一并删除。
set -euo pipefail

PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

log() { echo "[uninstall] $*"; }

systemctl disable --now apmanager.service 2>/dev/null || true
rm -f /etc/systemd/system/apmanager.service
systemctl daemon-reload
log "服务已停止并移除"

rm -f /usr/local/bin/apmanager
log "二进制已删除"

# 撤掉我们的 nft 表（只动自家表）
nft delete table inet apmanager 2>/dev/null || true

if [ "$PURGE" = "1" ]; then
    rm -rf /etc/apmanager /var/lib/apmanager /var/log/apmanager
    log "配置/状态/日志已删除（--purge）"
else
    log "保留 /etc/apmanager /var/lib/apmanager /var/log/apmanager（--purge 可清除）"
fi
