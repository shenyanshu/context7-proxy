// /mcp 的 tools/call 结果缓存：与 REST 查询共享同一张缓存表（store 的
// SetCache/GetCache）与同一 TTL 配置（query.Service.CacheTTL），让 MCP
// 查询流量同样省 key 额度。可缓存范围是显式白名单——resolve-library-id
// 与 query-docs 的 POST 调用、请求带 id、HTTP 200 且 JSON-RPC result 存在
// 且 isError 非真；其余请求一律纯透传。缓存是透传路径的可选捷径，不是
// 第二套工具语义：回源响应始终原文透传，缓存只决定“是否还要回源”。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"context7-proxy/internal/store"
)

// MCP 缓存约束常量。
const (
	// mcpCacheKeyPrefix 缓存键前缀：与 REST 的哈希键空间天然隔离
	// （REST 键是十六进制哈希，"mcp:" 前缀不可能与之冲突）
	mcpCacheKeyPrefix = "mcp:"
	// maxMCPResponseBytes 回源 tools/call 响应的缓冲上限：与 REST 的
	// maxResponseBodyBytes 同口径（10 MiB），防异常上游导致无界读取
	maxMCPResponseBytes = 10 << 20
)

// mcpCacheableTools 可缓存工具白名单：这两个工具的返回是与 key 无关的
// 公共数据；其余工具（含官方未来新增的）语义未知，一律透传不缓存。
var mcpCacheableTools = map[string]bool{
	"resolve-library-id": true,
	"query-docs":         true,
}

// mcpCallMeta 是一次落在缓存白名单内的 tools/call 请求的判定元数据。
type mcpCallMeta struct {
	tool     string          // 白名单内的工具名
	id       json.RawMessage // 请求 id 原文：命中时回填本次 id，不能回放缓存响应原文
	cacheKey string          // 规范化缓存键
}

// parseMCPCacheCandidate 判定请求体是否可缓存：tools/call + 白名单工具 +
// 携带 id。任一条件不满足或解析失败都保守返回 nil，走纯透传——通知
// （无 id）既没有组帧依据也不该缓存。
func parseMCPCacheCandidate(body []byte) *mcpCallMeta {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	// id 缺失或为 null 即 JSON-RPC 通知：不期待响应，缓存命中也无从组帧
	if req.Method != "tools/call" || len(req.ID) == 0 || string(req.ID) == "null" {
		return nil
	}
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || !mcpCacheableTools[params.Name] {
		return nil
	}
	key, ok := mcpCacheKey(params.Name, req.Params)
	if !ok {
		return nil
	}
	return &mcpCallMeta{tool: params.Name, id: req.ID, cacheKey: key}
}

// mcpCacheKey 生成缓存键：工具名 + 规范化参数 JSON。参数重新解析为 map
// 再 Marshal（Go 对 map 键排序），同一参数集合无论字段顺序如何都落到
// 同一键；解析失败返回 false，调用方据此放弃缓存。
func mcpCacheKey(tool string, params json.RawMessage) (string, bool) {
	var normalized map[string]any
	if err := json.Unmarshal(params, &normalized); err != nil {
		return "", false
	}
	norm, err := json.Marshal(normalized)
	if err != nil {
		return "", false
	}
	return mcpCacheKeyPrefix + tool + ":" + string(norm), true
}

// serveMCPCacheHit 尝试缓存命中：命中则本地组帧返回并按 cached 口径记账，
// 返回 true 表示请求已处理完毕。缓存读的 store 故障与 REST 同口径让整次
// 请求失败（500），不降级为无缓存回源——那会让 key 额度消耗不可对账。
func (s *Server) serveMCPCacheHit(w http.ResponseWriter, meta *mcpCallMeta, start time.Time) bool {
	entry, err := s.store.GetCache(meta.cacheKey)
	if err != nil {
		if !errors.Is(err, store.ErrCacheMiss) {
			_ = s.recordMCPRequest(meta.tool, start, http.StatusInternalServerError, 0, false)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return true
		}
		return false
	}
	if rErr := s.recordMCPRequest(meta.tool, start, http.StatusOK, 0, true); rErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "log request failed")
		return true
	}
	writeMCPCacheHit(w, meta.id, entry.Body)
	return true
}

// writeMCPCacheHit 命中缓存的本地组帧：id 必须回填本次请求的 id——不同
// 客户端的 id 序列不同，回放缓存响应原文会造成 id 错配。以 application/json
// 返回：MCP streamable HTTP 规范允许服务端在 JSON 与 SSE 两种响应形态中
// 任选其一，客户端必须两者都兼容；本地组帧天然是单个完整消息，选 JSON。
func writeMCPCacheHit(w http.ResponseWriter, id json.RawMessage, result []byte) {
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	buf.Write(id)
	buf.WriteString(`,"result":`)
	buf.Write(result)
	buf.WriteString("}")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// MCP key 级错误约束常量。
const (
	// mcpQuotaCooldown 额度类错误无库内 reset 时刻可用时的固定冷却时长
	mcpQuotaCooldown = 10 * time.Minute
	// mcpMaxKeyRetries 缓冲路径检测到 key 级错误后的换 key 重试上限（1 次）
	mcpMaxKeyRetries = 1
)

// mcpInvalidKeyPatterns "invalid api key" 类文案：命中即自动禁用该 key——
// 这类错误对该 key 是永久性的，继续调度只会白白烧请求。
var mcpInvalidKeyPatterns = []string{"invalid api key"}

// mcpQuotaErrorPatterns 额度/限流类文案：命中即冷却该 key 后换 key 重试。
// 依赖官方错误文案做子串匹配是脆弱的耦合：官方改文案时这里退化为普通错误
// 透传（fail-open），不会误禁用/误冷却，最坏只是少救活一次请求。
var mcpQuotaErrorPatterns = []string{"rate limit", "ratelimit", "quota", "too many requests", "429"}

// mcpCallFailure 提取出的 key 级错误分类。
type mcpCallFailure int

const (
	// mcpNoKeyFailure 非 key 级错误（或非 isError）：不处置直接透传
	mcpNoKeyFailure mcpCallFailure = iota
	// mcpFailureInvalidKey key 无效：禁用 + 可重试
	mcpFailureInvalidKey
	// mcpFailureQuota 额度/限流：冷却 + 可重试
	mcpFailureQuota
)

// classifyMCPCallFailure 对已缓冲的 tools/call 响应做 key 级错误分类：
// JSON-RPC result.isError==true 时取全部 text content 做不区分大小写子串
// 匹配。官方 MCP 网关的限流/鉴权失败表现为 HTTP 200 + isError 文案
// （观察所得），HTTP 状态层的失败仍由 relayMCP 的 429 分支处理。
func classifyMCPCallFailure(contentType string, buf []byte) mcpCallFailure {
	m := parseMCPJSONRPCMessage(contentType, buf)
	if m == nil || m.Result == nil {
		return mcpNoKeyFailure // 无 result / JSON-RPC error / 解析失败：不处置
	}
	var content struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(m.Result, &content); err != nil || !content.IsError {
		return mcpNoKeyFailure
	}
	text := strings.ToLower(mcpCallTextContent(m.Result))
	for _, p := range mcpInvalidKeyPatterns {
		if strings.Contains(text, p) {
			return mcpFailureInvalidKey
		}
	}
	for _, p := range mcpQuotaErrorPatterns {
		if strings.Contains(text, p) {
			return mcpFailureQuota
		}
	}
	return mcpNoKeyFailure
}

// mcpCallTextContent 提取 result.content 里全部 text 项的拼接文本：
// 错误文案可能跨多个 content 块，逐块拼接避免漏检。
func mcpCallTextContent(result []byte) string {
	var content struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &content); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range content.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

// serveMCPCallRelayed 处理白名单 tools/call 的回源响应：完整缓冲（SSE
// 形态同样缓冲——单次 tools/call 的 SSE 流终会终止，不存在长连接订阅
// 语义）→ key 级错误检测（无效 key 禁用 / 额度类冷却，各最多换 1 个 key
// 重试一次）→ 200 且解析出非 isError 的 result 时写共享缓存 → 记账 →
// 原始字节整块写回客户端，保持透传真度。失败尝试的响应绝不入缓存；无 key
// 可换或重试仍失败时把最后一次的上游响应原样返回（fail-open，不构造新
// 错误）。缓存写失败与 REST 同口径让整次请求失败：静默丢失会让"哪些响应
// 实际入了缓存"不可对账。
func (s *Server) serveMCPCallRelayed(w http.ResponseWriter, r *http.Request, body []byte, meta *mcpCallMeta, start time.Time, keyID int64) {
	resp, keyID, buf, failure := s.relayMCPCallWithKeyRetry(r, body, keyID)
	defer resp.Body.Close()
	if failure == mcpNoKeyFailure && resp.StatusCode == http.StatusOK {
		if result, ok := extractMCPCallResult(resp.Header.Get("Content-Type"), buf); ok {
			if cErr := s.store.SetCache(meta.cacheKey, result, "application/json", s.query.CacheTTL()); cErr != nil {
				writeJSONError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if rErr := s.recordMCPRequest(meta.tool, start, resp.StatusCode, keyID, false); rErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "log request failed")
		return
	}
	writeMCPBufferedResponse(w, resp, buf)
}

// relayMCPCallWithKeyRetry 执行带 key 级错误重试的缓冲转发：逐跳"转发
// （含 429 换 key）→ 缓冲 → 检测"，检测到可重试的 key 级错误时处置该 key
// 并换 key 重发同一请求体至多一次。返回最终响应、实际服务的 key、缓冲体
// 与最终跳的错误分类。网络失败/超限等无法判定 key 责任的情况一律 502
// 合成响应，不重试。
func (s *Server) relayMCPCallWithKeyRetry(r *http.Request, body []byte, keyID int64) (*http.Response, int64, []byte, mcpCallFailure) {
	for attempt := 0; ; attempt++ {
		key, gErr := s.keyValue(keyID)
		if gErr != nil {
			return mcpGatewayErrorResponse(), keyID, nil, mcpNoKeyFailure
		}
		resp, respKey, dErr := s.relayMCP(r, body, keyID, key)
		if dErr != nil {
			return mcpGatewayErrorResponse(), keyID, nil, mcpNoKeyFailure
		}
		keyID = respKey
		buf, rErr := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseBytes+1))
		resp.Body.Close()
		if rErr != nil || len(buf) > maxMCPResponseBytes {
			return mcpGatewayErrorResponse(), keyID, nil, mcpNoKeyFailure
		}
		failure := classifyMCPCallFailure(resp.Header.Get("Content-Type"), buf)
		if failure == mcpNoKeyFailure || attempt >= mcpMaxKeyRetries {
			return resp, keyID, buf, failure
		}
		if !s.handleMCPKeyFailure(failure, keyID) {
			return resp, keyID, buf, failure // 落盘失败：池状态未知，不重试
		}
		nextID, _, pErr := s.query.PickProxyKey()
		if pErr != nil {
			return resp, keyID, buf, failure // 无 key 可换：最后一跳原样返回
		}
		keyID = nextID
	}
}

// keyValue 取 key 原文：重试跳的 key 由 ID 反查（首轮 key 从调度选出后
// 只传 ID，统一反查口径，避免两套参数传递形态）。
func (s *Server) keyValue(keyID int64) (string, error) {
	k, err := s.store.GetKeyByID(keyID)
	if err != nil {
		return "", err
	}
	return k.Value, nil
}

// mcpGatewayErrorResponse 合成 502 网关错误响应：缓冲路径读体失败时尚未
// 向客户端写任何字节，用合成响应统一"网络层失败"的返回形态，头与体和
// writeJSONError 的 502 契约一致。
func mcpGatewayErrorResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"upstream unavailable"}`))),
	}
}

// handleMCPKeyFailure 落盘处置失败的 key：禁用（invalid api key）或冷却
// （额度类，固定 10 分钟——DB 无额度重置时刻可用，见 mcpQuotaCooldown）。
// 落盘失败返回 false——池状态未知时宁可把错误透传给客户端，
// 也不在未确认的池状态上继续换 key 重试。
func (s *Server) handleMCPKeyFailure(failure mcpCallFailure, keyID int64) bool {
	switch failure {
	case mcpFailureInvalidKey:
		return s.store.DisableKey(keyID, "invalid api key (auto-detected)") == nil
	case mcpFailureQuota:
		until := time.Now().Add(mcpQuotaCooldown).Unix()
		return s.store.CooldownKey(keyID, until) == nil
	default:
		return false
	}
}

// writeMCPBufferedResponse 把已缓冲的完整上游响应写回客户端：头剥离规则
// 与流式路径一致，body 为上游原始字节——SSE 响应也整块写回，字节级透传。
func writeMCPBufferedResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	for k, vs := range resp.Header {
		if mcpStripResponseHeaders[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// mcpJSONRPCMessage 是单条已解析的 JSON-RPC 消息（result/error 均为可选）。
type mcpJSONRPCMessage struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// parseMCPJSONRPCMessage 解析缓冲体中的首条 JSON-RPC 消息：SSE 形态逐条
// 尝试 data: 行，纯 JSON 形态整体解析。任一消息无法解析返回 nil，调用方
// （缓存/分类）各自保守处置。
func parseMCPJSONRPCMessage(contentType string, body []byte) *mcpJSONRPCMessage {
	messages := [][]byte{body}
	if strings.HasPrefix(contentType, contentTypeSSE) {
		messages = sseDataMessages(body)
	}
	for _, msg := range messages {
		if len(msg) == 0 {
			continue
		}
		m := new(mcpJSONRPCMessage)
		if err := json.Unmarshal(msg, m); err != nil {
			return nil
		}
		return m
	}
	return nil
}

// extractMCPCallResult 从缓冲的响应体提取可缓存的 tools/call result：
// result 存在且 isError 非真才返回；JSON-RPC error 或任一解析失败一律
// 保守放弃（宁少勿错：错误缓存的危害远大于少缓存一次）。
func extractMCPCallResult(contentType string, body []byte) (json.RawMessage, bool) {
	m := parseMCPJSONRPCMessage(contentType, body)
	if m == nil || m.Result == nil || string(m.Result) == "null" {
		return nil, false
	}
	if len(m.Error) > 0 && string(m.Error) != "null" {
		return nil, false
	}
	var content struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(m.Result, &content); err != nil || content.IsError {
		return nil, false
	}
	return m.Result, true
}

// sseDataMessages 提取 SSE 流里全部 data: 行的载荷。MCP streamable HTTP
// 约定每个事件恰好一条 data: 行承载一个完整 JSON 消息（消息内含裸换行
// 被规范禁止），逐行收集即可；行内 JSON 是否合法交给上层解析保守判定。
func sseDataMessages(body []byte) [][]byte {
	var msgs [][]byte
	for _, line := range strings.Split(string(body), "\n") {
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			msgs = append(msgs, []byte(strings.TrimSpace(d)))
		}
	}
	return msgs
}
