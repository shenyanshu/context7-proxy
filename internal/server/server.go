// Package server 装配 HTTP 入口：路由注册、鉴权中间件、REST 查询、
// 管理 API 与 SPA 静态资源。只做协议适配——业务语义全部下沉在
// query/store 核心包，入口层不含调度、缓存或持久化逻辑。
package server

import (
	"net/http"
	"time"

	"context7-proxy/internal/query"
	"context7-proxy/internal/store"
	"context7-proxy/web"
)

// Server 持有全部依赖；所有 handler 都是其方法，装配一次即可服务并发。
type Server struct {
	masterKey   string
	mcpUpstream string
	query       *query.Service
	store       *store.Store
	spa         http.Handler
}

// New 构造 HTTP 服务。mcpUpstream 是官方 MCP 端点基址（/mcp 透明转发目标）。
func New(masterKey, mcpUpstream string, q *query.Service, st *store.Store) *Server {
	return &Server{
		masterKey:   masterKey,
		mcpUpstream: mcpUpstream,
		query:       q,
		store:       st,
		spa:         spaHandler(web.DistFS),
	}
}

// Handler 组装路由表并套上鉴权中间件。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// REST 查询入口：两个只读端点
	mux.HandleFunc("GET /api/v2/libs/search", s.handleQuery(query.PathSearch))
	mux.HandleFunc("GET /api/v2/context", s.handleQuery(query.PathContext))
	// /api/v2/ 其余路径/方法：拒绝借道（ServeMux 的 GET / 兜底给纯文本 404，
	// 与 API JSON 契约不符，必须显式兜底）
	mux.HandleFunc("/api/v2/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})

	// 管理 API：前端控制台的增删启停与总览
	mux.HandleFunc("GET /api/admin/overview", s.handleOverview)
	mux.HandleFunc("GET /api/admin/keys", s.handleListKeys)
	mux.HandleFunc("POST /api/admin/keys", s.handleAddKey)
	mux.HandleFunc("PATCH /api/admin/keys/{id}", s.handleSetKeyEnabled)
	mux.HandleFunc("DELETE /api/admin/keys/{id}", s.handleDeleteKey)

	// 缓存查看（只读）：列表不含 body；{key...} 承接含 ?/&/:/{} 的缓存键
	mux.HandleFunc("GET /api/admin/cache", s.handleListCache)
	mux.HandleFunc("GET /api/admin/cache/{key...}", s.handleCacheDetail)

	// SPA 静态：/admin 精确命中 301 规范化到 /admin/（前端是相对 base，
	// 无尾斜杠时 ./assets 会解析到 /assets 而 404）；/admin/{path...}
	// 覆盖带斜杠的全部子路径（含 /admin/ 本身），HashRouter 无需 history rewrite
	mux.Handle("GET /admin", s.spa)
	mux.Handle("GET /admin/{path...}", s.spa)

	// MCP 端点：透明转发官方 MCP（MCP_UPSTREAM + /mcp），代理只管多 key
	// 负载，工具行为与官方永远一致；鉴权独立于 /api/ 前缀中间件（见 mcpAuth）
	mux.Handle("/mcp", s.newMCPHandler())

	// 根跳转必须用 {$} 精确匹配：方法限定 "GET /" 会与全方法子树 "/api/v2/"
	// 冲突（ServeMux 视其为更通用的路径模式，注册期 panic）
	mux.HandleFunc("GET /{$}", s.handleRoot)

	// /api/v2 与 /api/admin 都要鉴权；静态资源与根跳转无需凭证——
	// 静态页不含数据，登录后由前端携带 Bearer 调 API
	return s.requireAuth(mux)
}

// handleRoot 把裸根路径跳到管理控制台。{$} 已保证只有 / 走到这里。
// 目标带尾斜杠：直连 /admin/ 避免再经过一次 /admin→/admin/ 规范化跳转。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// Server 顶层 HTTP 约束集中维护。
const (
	// 请求头读取上限：防慢速头部攻击（slowloris），读完整头部才有 body 处理
	readHeaderTimeout = 10 * time.Second
)

// NewHTTPServer 构造可监听的 http.Server：超时策略在此统一设置。
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: readHeaderTimeout}
}
