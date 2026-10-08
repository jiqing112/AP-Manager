package api

import (
	"net/http"
	"strings"

	"github.com/skip2/go-qrcode"
)

// wifiEsc 按 WIFI: 负载规范转义特殊字符（\ ; , : " 前加反斜杠）
func wifiEsc(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`;`, `\;`,
		`,`, `\,`,
		`:`, `\:`,
		`"`, `\"`,
	).Replace(s)
}

// wifiPayload 生成手机相机可识别的 WiFi 连接字符串。
// WPA3 热点仍写 T:WPA——主流手机扫码后按 WPA/WPA3 自适应协商。
func wifiPayload(ssid, password, security string) string {
	if security == "open" {
		return "WIFI:T:nopass;S:" + wifiEsc(ssid) + ";;"
	}
	return "WIFI:T:WPA;S:" + wifiEsc(ssid) + ";P:" + wifiEsc(password) + ";;"
}

// handleAPQRCode 输出当前热点配置的连接二维码（PNG）。
// 内容始终取自服务端生效配置，改密后重新打开即为新码；需登录（同源 img 自动带 Cookie）。
func (s *Server) handleAPQRCode(w http.ResponseWriter, r *http.Request) {
	ap := s.cfg.AP
	png, err := qrcode.Encode(wifiPayload(ap.SSID, ap.Password, ap.Security), qrcode.Medium, 320)
	if err != nil {
		writeErr(w, 500, "二维码生成失败")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}
