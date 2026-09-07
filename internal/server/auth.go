// 鉴权中间件：Bearer master key，常量时间比较防时序侧信道。
package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// authErrorBody 与鉴权失败响应统一；凭证错误不给任何可探测差异。
const unauthorizedBody = `{"error":"unauthorized"}`

// requireAuth 盖住全部 /api/ 路径（查询与管理共用一个 master key：
// 控制台与代理入口是同一信任域，拆两套凭证只增加管理负担不增加安全性）。
// /admin 静态资源与 / 跳转不在此列：页面本身不含数据。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.masterKey)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(unauthorizedBody))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken 提取 "Bearer <token"；非 Bearer 方案一律拒绝。
func bearerToken(header string) (string, bool) {
	const scheme = "Bearer "
	if !strings.HasPrefix(header, scheme) {
		return "", false
	}
	token := strings.TrimRight(header[len(scheme):], "")
	if token == "" {
		return "", false
	}
	return token, true
}
