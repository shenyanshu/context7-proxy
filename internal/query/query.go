// Package query 实现可被 REST 代理与 MCP（Go 官方 SDK）共用的查询服务：
// 校验参数 → 查共享缓存 → 从 key 池选一个启用 key → 带有限超时调上游 →
// 观察额度/冷却 → 更新缓存。入口层（HTTP/MCP handler）负责请求统计，本包不重复计数。
package query

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// 本服务只代理公共文档查询，固定两个只读端点；其余路径一律拒绝，
// 防止把 key 池能力借道给未审计的写操作或无关端点。
const (
	PathSearch  = "/api/v2/libs/search"
	PathContext = "/api/v2/context"

	// 公共池同权限共享缓存，key 从不参与缓存条目选择，因此入参长度上限
	// 只需防御滥用，与 Context7 官方限制对齐。
	maxQueryLen    = 500
	minQueryLen    = 1
	searchQueryKey = "libraryName"
	contextIDKey   = "libraryId"
)

// ErrUnsupportedPath 表示路径不在允许清单内。
var ErrUnsupportedPath = errors.New("unsupported path")

// ErrInvalidParams 表示参数校验失败（缺失/超长/非法取值/重复/未知参数）。
// 入口层（REST/MCP）用 errors.Is 映射为 400：参数错误与内部故障必须可区分，
// 靠错误字符串匹配不可靠。
var ErrInvalidParams = errors.New("invalid parameters")

// ErrNoAvailableKey 表示池中无启用且不在冷却中的 key。
var ErrNoAvailableKey = errors.New("no upstream keys available")

// ErrAllCooling 表示有启用 key 但全部处于冷却中。
var ErrAllCooling = errors.New("all upstream keys cooling down")

// allowedSearchType 是 /context 的 type 参数官方取值（txt=文本片段、json=JSON 片段），
// 白名单而非透传，避免任意字符串借道进入缓存 key。
var allowedSearchType = map[string]bool{"txt": true, "json": true}

// 两端点各自的允许参数集合：q.Get 只看首值而 Encode 会全量上送，
// 必须显式拒绝未知与重复参数，杜绝 "fast=true&fast=invalid" 这类
// 校验通过但上游语义不明的输入。
var allowedParams = map[string]map[string]bool{
	PathSearch:  {"libraryName": true, "query": true, "fast": true},
	PathContext: {"libraryId": true, "query": true, "type": true, "fast": true},
}

// validateParams 对两个端点做必填/取值/参数集合校验；返回规范化后的 query 编码，
// 保留全部会影响结果的参数（含 fast），供缓存 key 与上游请求共用。
// 校验在入参的副本上进行，调用方的 url.Values 绝不被修改。
func validateParams(path string, q url.Values) (string, error) {
	allowed, ok := allowedParams[path]
	if !ok {
		return "", ErrUnsupportedPath
	}
	// clone 而非原地 prune：调用方可能复用同一 url.Values，原地删改是副作用
	v := cloneValues(q)
	if err := rejectUnknownOrRepeated(allowed, v); err != nil {
		return "", err
	}
	switch path {
	case PathSearch:
		// 官方契约：search 的 libraryName 与 query 均必填，各 1-500 字符
		if !validLength(v.Get(searchQueryKey)) {
			return "", fmt.Errorf("%w: libraryName must be 1-500 characters", ErrInvalidParams)
		}
		if !validLength(v.Get("query")) {
			return "", fmt.Errorf("%w: query must be 1-500 characters", ErrInvalidParams)
		}
	case PathContext:
		if !validLength(v.Get(contextIDKey)) {
			return "", fmt.Errorf("%w: libraryId must be 1-500 characters", ErrInvalidParams)
		}
		if !validLength(v.Get("query")) {
			return "", fmt.Errorf("%w: query must be 1-500 characters", ErrInvalidParams)
		}
		if t := v.Get("type"); t != "" && !allowedSearchType[t] {
			return "", fmt.Errorf("%w: type must be txt or json", ErrInvalidParams)
		}
	}
	if err := validateFast(v); err != nil {
		return "", err
	}
	// 编码前去掉空值参数，保证 "a=1&b=" 与 "a=1" 落到同一缓存条目；
	// key 排序由 Encode() 保证，参数集合稳定即缓存 key 稳定。
	pruneEmpty(v)
	return v.Encode(), nil
}

// rejectUnknownOrRepeated 拒绝集合外参数与同参数多值：
// 多值时 Get() 只校验首值、Encode() 却全量上送，两者语义必然分叉，宁拒不错。
func rejectUnknownOrRepeated(allowed map[string]bool, v url.Values) error {
	for k, vs := range v {
		if !allowed[k] {
			return fmt.Errorf("%w: unsupported parameter %q", ErrInvalidParams, k)
		}
		if len(vs) > 1 {
			return fmt.Errorf("%w: repeated parameter %q", ErrInvalidParams, k)
		}
	}
	return nil
}

// cloneValues 深拷贝 url.Values，校验/修剪不触碰调用方数据。
func cloneValues(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, vs := range q {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// validateFast 校验 fast 参数：官方只接受 true/false，其余取值直接报错——
// 静默改写会擅自改变语义并污染缓存归类，宁拒不错。
func validateFast(q url.Values) error {
	if v := q.Get("fast"); v != "" && v != "true" && v != "false" {
		return fmt.Errorf("%w: fast must be true or false", ErrInvalidParams)
	}
	return nil
}

// pruneEmpty 删除取值为空的参数，统一缓存 key 形态。
func pruneEmpty(q url.Values) {
	for k, vs := range q {
		if len(vs) == 0 || (len(vs) == 1 && vs[0] == "") {
			q.Del(k)
		}
	}
}

func validLength(v string) bool {
	n := len(v)
	return n >= minQueryLen && n <= maxQueryLen
}

// cacheKey 由允许路径 + 规范化 query 拼接，不含 key：公共池权限一致是部署前提，
// 缓存按请求内容而非身份共享才能省额度。
func cacheKey(path, encodedQuery string) string {
	// 不引入 hash 依赖：path+query 本身长度有限，直接拼接即可充当主键，
	// 也让排查问题时可直接从 key 反解出请求参数。
	return path + "?" + encodedQuery
}

// upstreamURL 组装上游完整 URL，参数已是规范化编码，原样拼接避免二次编码。
func upstreamURL(baseURL, path, encodedQuery string) string {
	u := strings.TrimSuffix(baseURL, "/") + path
	if encodedQuery != "" {
		u += "?" + encodedQuery
	}
	return u
}
