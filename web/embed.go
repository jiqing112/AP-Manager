// web/embed.go —— 资源打进二进制。与资源同目录以规避 go:embed 不能引用上级的限制。
// 根即站点根：index.html、app.js、dist/app.css、vendor/*.js
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist all:vendor index.html app.js
var assets embed.FS

// Assets 只读站点文件系统（http.FileServerFS 直接用）
var Assets fs.FS = assets
