// REST 查询入口：query.Execute 的 HTTP 适配、错误映射与统一请求统计。
// 统计规则：每个 REST 请求恰好记一条 RequestRecord——成功、上游错误、
// 缓存命中都必须记；RecordRequest 只在这一处调用，query 核心不重复计数。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"context7-proxy/internal/query"
	"context7-proxy/internal/store"
)

// handleQuery 返回指定端点的 REST handler。
func (s *Server) handleQuery(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		res, err := s.query.Execute(r.Context(), path, r.URL.Query())

		record := store.RequestRecord{
			Time:   start.Unix(),
			Method: http.MethodGet,
			Path:   r.URL.Path,
			Status: statusFor(err, res),
			Cached: res.Cached,
			// 亚毫秒请求 floor 到 1：durationMs=0 与"未测量"无法区分，
			// 前端监控口径要求非零耗时
			DurationMs: max(int64(1), time.Since(start).Milliseconds()),
		}
		if !res.Cached && res.KeyID != 0 {
			record.KeyID = res.KeyID
			record.KeyMasked = s.maskedKey(res.KeyID)
		}
		// 统计写失败让整次请求失败：日志与统计计数是验收口径（事务保证二者一致），
		// 静默丢失会让 overview 数字与真实流量不可对账
		if rErr := s.store.RecordRequest(record); rErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "log request failed")
			return
		}
		s.writeQueryResult(w, res, err)
	}
}

// writeQueryResult 把 query.Result 与错误映射为 HTTP 响应。
// 429 是软错误（err 与 res 同时非零）：状态码/Retry-After 来自 res。
func (s *Server) writeQueryResult(w http.ResponseWriter, res query.Result, err error) {
	switch {
	case err == nil:
		// 成功 200；上游非 200 状态（3xx/4xx/5xx）原样透传状态码+body+Content-Type
		contentType := res.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Body)
	case errors.Is(err, query.ErrInvalidParams):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, query.ErrUnsupportedPath):
		writeJSONError(w, http.StatusNotFound, "not found")
	case errors.Is(err, query.ErrNoAvailableKey):
		writeJSONError(w, http.StatusServiceUnavailable, "no upstream keys available")
	case errors.Is(err, query.ErrAllCooling):
		// Retry-After 由代理按冷却截止换算，杜绝上游头直通客户端
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterForAllCooling(err), 10))
		writeJSONError(w, http.StatusTooManyRequests, "all upstream keys cooling down")
	case errors.Is(err, query.ErrUpstreamUnreachable), errors.Is(err, query.ErrResponseTooLarge):
		writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
	default:
		// 未知内部错误不透出细节，防泄漏内部实现
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// statusFor 计算 RequestRecord.Status：软错误（429）取结果状态，
// 其余错误按映射表折算，成功取上游状态。
func statusFor(err error, res query.Result) int {
	if err == nil {
		return res.Status
	}
	if res.Status != 0 {
		return res.Status // 429 软错误：记录真实上游状态
	}
	switch {
	case errors.Is(err, query.ErrInvalidParams):
		return http.StatusBadRequest
	case errors.Is(err, query.ErrUnsupportedPath):
		return http.StatusNotFound
	case errors.Is(err, query.ErrNoAvailableKey):
		return http.StatusServiceUnavailable
	case errors.Is(err, query.ErrAllCooling):
		return http.StatusTooManyRequests
	case errors.Is(err, query.ErrUpstreamUnreachable), errors.Is(err, query.ErrResponseTooLarge):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// retryAfterForAllCooling 从 ErrAllCoolingInfo 的最早到期时刻换算等待秒数；
// 未知或已过期时给 1 秒下限——Retry-After: 0 等于指示客户端立即重试，
// 全冷却场景只会再撞一次墙。
func retryAfterForAllCooling(err error) int64 {
	var info *query.ErrAllCoolingInfo
	if errors.As(err, &info) && info.EarliestReset > 0 {
		if wait := info.EarliestReset - time.Now().Unix(); wait > 0 {
			return wait
		}
	}
	return 1
}

// maskedKey 按 ID 查 key 原文做脱敏；查不到（key 刚被并发删除）退化为
// 占位脱敏，不让日志写入因脱敏失败而失败。
func (s *Server) maskedKey(id int64) string {
	k, err := s.store.GetKeyByID(id)
	if err != nil {
		return "***"
	}
	return store.MaskedKey(k.Value)
}

// writeJSONError 输出统一错误契约 {"error":"..."}。
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	body, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
