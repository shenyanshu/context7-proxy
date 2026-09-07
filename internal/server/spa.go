// SPA 静态资源：从内嵌 dist 文件系统按扩展名输出，未知路径回退 index.html。
package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// distRoot 内嵌 FS 内 dist 的挂载前缀。
const distRoot = "dist"

// spaHandler 构造静态资源 handler。
// 前端使用 HashRouter + 相对 base（vite base:'./'），因此：
//   - /admin 必须先 301 到 /admin/——无尾斜杠时浏览器会把 ./assets/xx.js
//     解析成 /assets/xx.js（404，页面空白）；
//   - /admin/ 与子路径直接回 index.html 或静态文件；
//   - 未命中文件时回退 index.html——前端路由全部在 hash 段，无需 history rewrite。
func spaHandler(dist embed.FS) http.Handler {
	sub, err := fs.Sub(dist, distRoot)
	if err != nil {
		panic("web embed: dist missing: " + err.Error()) // 构建期约束被破坏，fail fast
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin" {
			http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/admin")
		name = strings.TrimPrefix(name, "/")
		if name == "" {
			serveIndex(w, r, sub)
			return
		}
		// 只允许文件名形式的访问，杜绝路径穿越（embed FS 本身已免疫，双保险）
		if strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(sub, name); err != nil {
			// 子路径未命中：HashRouter 前端自行解析 hash，统一回 index.html
			serveIndex(w, r, sub)
			return
		}
		// 命中真实文件：改写路径为 dist 内相对路径再交给 FileServerFS，
		// 它按 r.URL.Path 寻址，不改写会带着 /admin 前缀去找文件
		r.URL.Path = "/" + name
		fileServer.ServeHTTP(w, r)
	})
}

// serveIndex 输出前端入口 index.html。
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "index.html missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // 入口页不缓存：发版即生效
	_, _ = w.Write(index)
}
