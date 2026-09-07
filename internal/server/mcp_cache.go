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

// serveMCPCallRelayed 处理白名单 tools/call 的回源响应：完整缓冲（SSE
// 形态同样缓冲——单次 tools/call 的 SSE 流终会终止，不存在长连接订阅
// 语义）→ 200 且解析出非 isError 的 result 时写共享缓存 → 记账 → 原始
// 字节整块写回客户端，保持透传真度。缓存写失败与 REST 同口径让整次请求
// 失败：静默丢失会让“哪些响应实际入了缓存”不可对账。
func (s *Server) serveMCPCallRelayed(w http.ResponseWriter, resp *http.Response, meta *mcpCallMeta, tool string, start time.Time, keyID int64) {
	buf, rErr := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseBytes+1))
	if rErr != nil || len(buf) > maxMCPResponseBytes {
		// 响应尚未写给客户端，可以干净地 502，优于流式路径的截断 200
		_ = s.recordMCPRequest(tool, start, http.StatusBadGateway, keyID, false)
		writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	if resp.StatusCode == http.StatusOK {
		if result, ok := extractMCPCallResult(resp.Header.Get("Content-Type"), buf); ok {
			if cErr := s.store.SetCache(meta.cacheKey, result, "application/json", s.query.CacheTTL()); cErr != nil {
				_ = s.recordMCPRequest(tool, start, http.StatusInternalServerError, keyID, false)
				writeJSONError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if rErr := s.recordMCPRequest(tool, start, resp.StatusCode, keyID, false); rErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "log request failed")
		return
	}
	writeMCPBufferedResponse(w, resp, buf)
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

// extractMCPCallResult 从缓冲的响应体提取 tools/call 的 JSON-RPC result：
// SSE 形态逐条解析 data: 行的 JSON 消息，纯 JSON 形态整体解析。result
// 存在且 isError 非真才可缓存；出现 JSON-RPC error 或任一解析失败一律
// 保守放弃（宁少勿错：错误缓存的危害远大于少缓存一次）。
func extractMCPCallResult(contentType string, body []byte) (json.RawMessage, bool) {
	messages := [][]byte{body}
	if strings.HasPrefix(contentType, contentTypeSSE) {
		messages = sseDataMessages(body)
	}
	for _, msg := range messages {
		var m struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(msg, &m); err != nil {
			return nil, false
		}
		if len(m.Error) > 0 && string(m.Error) != "null" {
			return nil, false
		}
		if len(m.Result) > 0 && string(m.Result) != "null" {
			var content struct {
				IsError bool `json:"isError"`
			}
			if err := json.Unmarshal(m.Result, &content); err != nil {
				return nil, false
			}
			if content.IsError {
				return nil, false
			}
			return m.Result, true
		}
	}
	return nil, false
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
