// 管理后台静态资源（Vue3 + Element Plus 单页应用，构建后嵌入）
package api

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

//go:embed web
var webFS embed.FS

// handleAdminWeb 提供管理后台页面与静态资源
func (s *Server) handleAdminWeb(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if strings.HasPrefix(name, "api/") {
		writeErr(w, http.StatusNotFound, CodeNotFound, "接口不存在")
		return
	}
	data, err := fs.ReadFile(webFS, filepath.ToSlash(filepath.Join("web", filepath.Clean(name))))
	if err != nil {
		// SPA 路由回退到 index.html
		data, err = fs.ReadFile(webFS, "web/index.html")
		if err != nil {
			writeErr(w, http.StatusNotFound, CodeNotFound, "页面不存在")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
