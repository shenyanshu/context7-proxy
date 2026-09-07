// MCP 透明反向代理：/mcp 的 GET/POST/DELETE 全部转发到官方 MCP 端点
// （MCP_UPSTREAM + /mcp），请求体逐字节不动。本代理只负责三件事：多 key
// 负载（选 key、429 冷却换 key 重试一次）、master key 鉴权，以及白名单内
// tools/call 的结果缓存（见 mcp_cache.go，与 REST 共享缓存表与 TTL）——
// 除此之外工具行为与官方永远一致，本地不实现任何工具语义。
//
// 转发实现为手动 round-trip 而非 httputil.ReverseProxy：429 重试需要在
// 写回客户端之前重放请求，ReverseProxy 的 ModifyResponse 无法重放 body；
// SSE 流式等价于 FlushInterval:-1（逐块立即 flush），两种响应形态共用
// 这一条转发路径，不做双轨。
package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"context7-proxy/internal/query"
	"context7-proxy/internal/store"
)

// MCP 转发约束常量。
const (
	// mcpPath 官方 MCP streamable HTTP 固定入口路径
	mcpPath = "/mcp"
	// methodMCP 请求统计里 MCP 调用的 Method 标识：管理界面据此区分流量
	methodMCP = "MCP"
	// 转发请求体上限：JSON-RPC 请求体小，4 MiB 与此前 SDK 入口口径一致
	maxMCPBody = 4 << 20
	// 429 换 key 重试上限：只重试一次，与 REST 的 maxRetry429 同口径
	maxMCPRetry429 = 1
	// contentTypeSSE SSE 流式响应的媒体类型前缀
	contentTypeSSE = "text/event-stream"
)

// mcpClient 转发专用客户端：不跟随重定向——Bearer key 绝不能被发给
// 重定向目标（与 query.Service 同一安全底线）；不设整体超时——SSE 长连接
// 的生命周期由请求 ctx（客户端断开即取消）决定。
var mcpClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// mcpStripRequestHeaders 请求侧剥离集合：逐跳头之外还包含客户端对代理的
// 三种鉴权头——上游只认池内 key，客户端凭证绝不能透传出去。
var mcpStripRequestHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Authorization":       true,
	"X-Context7-Api-Key":  true,
	"Context7-Api-Key":    true,
}

// mcpStripResponseHeaders 响应侧只剥逐跳头：上游状态码/错误体一律原样。
var mcpStripResponseHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// newMCPHandler 构造 /mcp 端点：master key 鉴权 + 透明转发官方 MCP。
func (s *Server) newMCPHandler() http.Handler {
	return s.mcpAuth(http.HandlerFunc(s.serveMCP))
}

// serveMCP 单次转发全流程：读体 → 解析统计用工具名 → 缓存命中判定（在
// 选 key 之前）→ 选 key → 转发（429 冷却换 key 重试一次）→ 记账 → 按响应
// 形态写回。白名单内的 tools/call 走缓冲路径（解析入缓存），其余请求纯
// 透传，行为与缓存特性引入之前完全一致。
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	body, err := readMCPBody(r)
	if err != nil {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	tool := mcpToolName(body)

	// 缓存判定在选 key 之前：命中直接本地回包，不消耗池内 key 额度。
	// 只有 POST 的白名单工具调用可缓存；通知与非 tools/call 一律透传。
	var meta *mcpCallMeta
	if r.Method == http.MethodPost {
		meta = parseMCPCacheCandidate(body)
	}
	if meta != nil && s.serveMCPCacheHit(w, meta, start) {
		return
	}

	keyID, key, kErr := s.query.PickProxyKey()
	if kErr != nil {
		status, msg := mcpPickKeyError(kErr)
		// 无 key 也记账：503 是真实发生过的请求结果，不记会让统计失真
		_ = s.recordMCPRequest(tool, start, status, 0, false)
		writeJSONError(w, status, msg)
		return
	}

	// 白名单 tools/call 走缓冲路径：key 级错误检测（禁用/冷却+换 key
	// 重试一次）+ 缓冲入缓存 + 原文写回（见 serveMCPCallRelayed）；其余
	// 请求（GET 流订阅、通知、非白名单方法/工具）保持原透传路径，
	// GET 的 SSE 流式语义不受影响
	if meta != nil {
		s.serveMCPCallRelayed(w, r, body, meta, start, keyID)
		return
	}

	resp, usedKey, dErr := s.relayMCP(r, body, keyID, key)
	if dErr != nil {
		// 网络层失败与 REST 的 ErrUpstreamUnreachable 同映射 502
		_ = s.recordMCPRequest(tool, start, http.StatusBadGateway, usedKey, false)
		writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer resp.Body.Close()

	// 统计写失败让整次请求失败：与 REST 同口径，静默丢失会破坏对账
	if rErr := s.recordMCPRequest(tool, start, resp.StatusCode, usedKey, false); rErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "log request failed")
		return
	}
	_ = writeMCPResponse(w, resp)
}

// relayMCP 转发并处理 429 重试：至多两次尝试。429 时先冷却当前 key，
// 冷却落盘后重选 key——池过滤自然排除冷却中的 key，无需显式排除参数；
// 冷却失败或换不出 key 则原样返回该 429（宁可保守也不在池状态未知时烧 key）。
// 返回最终响应与实际使用的 key ID。
func (s *Server) relayMCP(r *http.Request, body []byte, keyID int64, key string) (*http.Response, int64, error) {
	for attempt := 0; ; attempt++ {
		resp, err := s.mcpUpstreamRoundTrip(r, body, key)
		if err != nil {
			return nil, keyID, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, keyID, nil
		}
		// 429 一律冷却当前 key（含重试后仍 429 的最后一跳）：
		// 限流是既成事实，落盘才能让后续调度避开
		if cErr := s.query.CooldownProxyKey(keyID, resp.Header); cErr != nil {
			return resp, keyID, nil
		}
		if attempt >= maxMCPRetry429 {
			return resp, keyID, nil // 重试额度用尽：429 原样返回
		}
		nextID, nextKey, pErr := s.query.PickProxyKey()
		if pErr != nil {
			return resp, keyID, nil // 无 key 可换：429 原样返回
		}
		_ = resp.Body.Close()
		keyID, key = nextID, nextKey
	}
}

// mcpUpstreamRoundTrip 构造并发出一次上游请求。
func (s *Server) mcpUpstreamRoundTrip(r *http.Request, body []byte, key string) (*http.Response, error) {
	req, err := s.buildMCPUpstreamRequest(r, body, key)
	if err != nil {
		return nil, fmt.Errorf("build MCP request: %w", err)
	}
	return mcpClient.Do(req)
}

// buildMCPUpstreamRequest 构造上游请求：方法/路径固定，头复制自客户端
// （剥鉴权与逐跳头）后注入池内 key，body 逐字节复用原文。
func (s *Server) buildMCPUpstreamRequest(r *http.Request, body []byte, key string) (*http.Request, error) {
	target := strings.TrimSuffix(s.mcpUpstream, "/") + mcpPath
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, rd)
	if err != nil {
		return nil, err
	}
	strip := maps.Clone(mcpStripRequestHeaders)
	// Connection 头点名的头也是逐跳头，按 RFC 7230 一并剥离
	for _, conn := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(conn, ",") {
			strip[http.CanonicalHeaderKey(strings.TrimSpace(tok))] = true
		}
	}
	for k, vs := range r.Header {
		if strip[k] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return req, nil
}

// writeMCPResponse 把上游响应写回客户端：SSE 响应逐块立即 flush 保流式
// （等价 FlushInterval:-1）；其余响应体小，整读缓冲后写回。
func writeMCPResponse(w http.ResponseWriter, resp *http.Response) error {
	for k, vs := range resp.Header {
		if mcpStripResponseHeaders[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), contentTypeSSE) {
		return streamMCPBody(w, resp.Body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// streamMCPBody 逐块转发并即时 flush：SSE 每个事件必须立刻到达客户端，
// 缓冲到响应结束再写会破坏流式语义。
func streamMCPBody(w http.ResponseWriter, src io.Reader) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, 8192)
	for {
		n, rErr := src.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return wErr // 客户端断开：转发终止
			}
			_ = rc.Flush()
		}
		if rErr != nil {
			if rErr == io.EOF {
				return nil
			}
			return rErr
		}
	}
}

// readMCPBody 读取请求体；GET/DELETE 读到空体。超限拒绝 413——转发
// 前必须持住完整 body（429 重试要重放），无法流式转发。
func readMCPBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxMCPBody {
		return nil, errMCPBodyTooLarge
	}
	return body, nil
}

var errMCPBodyTooLarge = errors.New("MCP request body exceeds size limit")

// mcpToolName 从 JSON-RPC 请求体提取 tools/call 的工具名作为统计 Path，
// 管理界面最近请求里 MCP 调用清晰可辨；非 tools/call 或解析失败记 "/mcp"——
// 统计口径宁可保守也不因格式假设误报。
func mcpToolName(body []byte) string {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return mcpPath
	}
	if req.Method != "tools/call" || req.Params.Name == "" {
		return mcpPath
	}
	return req.Params.Name
}

// recordMCPRequest 与 REST 同规则的记账：Method="MCP"，Status 为最终
// 上游状态（无 key 503 / 网络失败 502 照实）。缓存命中时 Cached=true 且
// 不记 key 字段——命中路径不消耗池内 key；KeyID/KeyMasked 的留空口径
// 与 REST 的 handleQuery 一致。
// 统计口径：协议开销流量（tools/list/initialize/通知，即解析不出工具名、
// Path 回退 /mcp 的请求）零额度消耗，计入统计会虚增总请求量并稀释缓存
// 命中率，一律跳过。stats 表历史数据不迁移——旧数据含少量协议流量，
// 口径切换从本版生效，误差只减不增。
func (s *Server) recordMCPRequest(tool string, start time.Time, status int, keyID int64, cached bool) error {
	if tool == mcpPath {
		return nil
	}
	record := store.RequestRecord{
		Time:       start.Unix(),
		Method:     methodMCP,
		Path:       tool,
		Status:     status,
		Cached:     cached,
		DurationMs: max(int64(1), time.Since(start).Milliseconds()),
	}
	if !cached && keyID != 0 {
		record.KeyID = keyID
		record.KeyMasked = s.maskedKey(keyID)
	}
	return s.store.RecordRequest(record)
}

// mcpPickKeyError 把选 key 失败映射为 503/500：无 key 与全冷却都是
// 池不可用（503），其余（store 故障）是内部错误（500），不透出细节。
func mcpPickKeyError(err error) (int, string) {
	switch {
	case errors.Is(err, query.ErrNoAvailableKey):
		return http.StatusServiceUnavailable, "no upstream keys available"
	case errors.Is(err, query.ErrAllCooling):
		return http.StatusServiceUnavailable, "all upstream keys cooling down"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// mcpAuth 是 /mcp 的独立鉴权中间件：鉴权语义与 /api/ 相同（master key），
// 但额外接受两个自定义头，方便只支持自定义 header 的 MCP 客户端配置。
func (s *Server) mcpAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.mcpAuthorized(r.Header) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(unauthorizedBody))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mcpAuthorized 收集三种凭证形态逐一常量时间比较：空串跳过——master key
// 在配置层强制非空，空 token 没有比对意义，且比对空串会引入长度为零的
// 短路路径。
func (s *Server) mcpAuthorized(h http.Header) bool {
	tokens := make([]string, 0, 3)
	if t, ok := bearerToken(h.Get("Authorization")); ok {
		tokens = append(tokens, t)
	}
	tokens = append(tokens, h.Get("x-context7-api-key"), h.Get("context7-api-key"))
	key := []byte(s.masterKey)
	authorized := false
	for _, t := range tokens {
		if t != "" && subtle.ConstantTimeCompare([]byte(t), key) == 1 {
			authorized = true
		}
	}
	return authorized
}
