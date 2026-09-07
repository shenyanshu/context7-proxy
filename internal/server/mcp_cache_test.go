// MCP tools/call 结果缓存的集成测试：复用 mcp_test.go 的假上游 fixture，
// 覆盖白名单命中/未命中、isError 不缓存、通知与非白名单透传、SSE 形态
// 入缓存、429 重试共存与 TTL 过期回源。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// mcpResolveCall 构造一条 resolve-library-id 的 tools/call 请求体。
func mcpResolveCall(id int) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
		`,"method":"tools/call","params":{"name":"resolve-library-id","arguments":{"libraryName":"React","query":"hooks"}}}`
}

// mcpCallResultJSON 标准成功结果体（tools/call 的 result 载荷）。
const mcpCallResultJSON = `{"content":[{"type":"text","text":"lib-id"}]}`

// decodeJSONRPC 解析响应体为可断言的 id/result 结构。
func decodeJSONRPC(t *testing.T, body []byte) (id, result json.RawMessage) {
	t.Helper()
	var m struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode JSON-RPC %s: %v", body, err)
	}
	return m.ID, m.Result
}

// 白名单命中：同参数两次 resolve-library-id（id 分别为 1/2、参数字段顺序
// 颠倒）只回源一次；第二次本地组帧返回，id 为本次请求 id，result 与首次
// 一致；记账两条，第二条 cached=true 且不记 key 字段。
func TestMCPCallCacheHit(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + mcpCallResultJSON + `}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcpcache-011")

	res1 := f.mcpDo(http.MethodPost, mcpResolveCall(1), nil)
	defer res1.Body.Close()
	if res1.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", res1.StatusCode)
	}
	got1, _ := io.ReadAll(res1.Body)
	// 未命中路径字节级透传：客户端拿到的就是上游原文
	if string(got1) != `{"jsonrpc":"2.0","id":1,"result":`+mcpCallResultJSON+`}` {
		t.Fatalf("first body = %s, want byte-identical upstream passthrough", got1)
	}

	// 第二次：id=2 且参数字段顺序颠倒——规范化键必须落到同一缓存条目
	const req2 = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"arguments":{"query":"hooks","libraryName":"React"},"name":"resolve-library-id"}}`
	res2 := f.mcpDo(http.MethodPost, req2, nil)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", res2.StatusCode)
	}
	if ct := res2.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("second Content-Type = %q, want application/json", ct)
	}
	got2, _ := io.ReadAll(res2.Body)
	id2, result2 := decodeJSONRPC(t, got2)
	if string(id2) != "2" {
		t.Fatalf("second id = %s, want 2 (this request's id, not cached frame)", id2)
	}
	_, result1 := decodeJSONRPC(t, got1)
	if string(result2) != string(result1) {
		t.Fatalf("second result = %s, want same as first %s", result2, result1)
	}

	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second served from cache)", len(bodies))
	}

	recs, err := f.st.RecentRequests()
	if err != nil || len(recs) != 2 {
		t.Fatalf("recent requests = %d (err %v), want 2", len(recs), err)
	}
	first, second := recs[1], recs[0] // 新的在前
	if first.Cached || first.KeyID == 0 {
		t.Fatalf("first record cached/keyID = %v/%d, want false/non-zero (went upstream)", first.Cached, first.KeyID)
	}
	if !second.Cached || second.KeyID != 0 || second.KeyMasked != "" ||
		second.Path != "resolve-library-id" || second.Status != http.StatusOK {
		t.Fatalf("second record = %+v, want cached=true key-empty path=resolve-library-id status=200", second)
	}
}

// query-docs 同样命中缓存：白名单第二项的命中路径。
func TestMCPQueryDocsCacheHit(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"docs"}]}}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcpcache-012")

	const req = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"query-docs","arguments":{"contextToken":"/react/hooks","topic":"useState"}}}`
	res1 := f.mcpDo(http.MethodPost, req, nil)
	res1.Body.Close()
	res2 := f.mcpDo(http.MethodPost, req, nil)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", res2.StatusCode)
	}
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1 (query-docs cached)", len(bodies))
	}
	got, _ := io.ReadAll(res2.Body)
	id, _ := decodeJSONRPC(t, got)
	if string(id) != "7" {
		t.Fatalf("second id = %s, want 7", id)
	}
}

// isError=true 的工具结果是业务失败：不入缓存，每次都回源。
func TestMCPCallIsErrorNotCached(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"no such library"}],"isError":true}}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcperr-013")

	for i := 1; i <= 2; i++ {
		res := f.mcpDo(http.MethodPost, mcpResolveCall(i), nil)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200 passthrough", i, res.StatusCode)
		}
		if !json.Valid(body) {
			t.Fatalf("call %d body = %s, want passthrough JSON", i, body)
		}
	}
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (isError must not cache)", len(bodies))
	}
	recs, err := f.st.RecentRequests()
	if err != nil || len(recs) != 2 {
		t.Fatalf("recent requests = %d (err %v), want 2", len(recs), err)
	}
	for _, rec := range recs {
		if rec.Cached {
			t.Fatalf("record %+v cached=true, want false", rec)
		}
	}
}

// tools/list 不在白名单：两次调用都回源，不产生缓存条目。
func TestMCPToolsListNotCached(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mcpToolsListResp))
	})
	addTestKey(t, f.st, "ctx7sk-mcplist-014")

	for i := 0; i < 2; i++ {
		res := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
		res.Body.Close()
	}
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (tools/list never cached)", len(bodies))
	}
}

// 白名单外的工具名：透传不缓存。
func TestMCPUnknownToolNotCached(t *testing.T) {
	const req = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"some-future-tool","arguments":{"x":"1"}}}`
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcpunk-015")

	for i := 0; i < 2; i++ {
		res := f.mcpDo(http.MethodPost, req, nil)
		res.Body.Close()
	}
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (non-whitelist tool passthrough)", len(bodies))
	}
	recs, err := f.st.RecentRequests()
	if err != nil || len(recs) != 2 {
		t.Fatalf("recent requests = %d (err %v), want 2", len(recs), err)
	}
	if recs[0].Cached || recs[1].Cached {
		t.Fatalf("records must not be cached: %+v", recs)
	}
}

// 无 id 的 tools/call 通知：正常透传不入缓存（无 id 无组帧依据）。
func TestMCPNotificationNotCached(t *testing.T) {
	const notify = `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"resolve-library-id","arguments":{"libraryName":"React","query":"hooks"}}}`
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + mcpCallResultJSON + `}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcpnotif-016")

	for i := 0; i < 2; i++ {
		res := f.mcpDo(http.MethodPost, notify, nil)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || string(body) != `{"jsonrpc":"2.0","id":1,"result":`+mcpCallResultJSON+`}` {
			t.Fatalf("notification %d passthrough = %d/%s, want 200/byte-identical", i, res.StatusCode, body)
		}
	}
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (notifications never cached)", len(bodies))
	}
}

// SSE 形态上游响应：未命中路径字节级透传原文（含 Content-Type）并正确
// 入缓存；下一次命中返回 JSON 等价的 result（id 换成本次请求 id）。
func TestMCPCallSSECache(t *testing.T) {
	const sseBody = "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":" + mcpCallResultJSON + "}\n\n"
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody))
	})
	addTestKey(t, f.st, "ctx7sk-mcpsse-017")

	res1 := f.mcpDo(http.MethodPost, mcpResolveCall(1), nil)
	defer res1.Body.Close()
	if res1.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", res1.StatusCode)
	}
	if ct := res1.Header.Get("Content-Type"); !strings.HasPrefix(ct, contentTypeSSE) {
		t.Fatalf("first Content-Type = %q, want text/event-stream passthrough", ct)
	}
	got1, _ := io.ReadAll(res1.Body)
	if string(got1) != sseBody {
		t.Fatalf("first body = %q, want byte-identical SSE original", got1)
	}

	res2 := f.mcpDo(http.MethodPost, mcpResolveCall(2), nil)
	defer res2.Body.Close()
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1 (SSE result was cached)", len(bodies))
	}
	if ct := res2.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("second Content-Type = %q, want application/json (local frame)", ct)
	}
	got2, _ := io.ReadAll(res2.Body)
	id2, result2 := decodeJSONRPC(t, got2)
	if string(id2) != "2" {
		t.Fatalf("second id = %s, want 2", id2)
	}
	if string(result2) != mcpCallResultJSON {
		t.Fatalf("second result = %s, want JSON-equivalent %s", result2, mcpCallResultJSON)
	}
}

// 429 冷却换 key 重试与缓存共存：第一次调用经 429→换 key→成功回源并入
// 缓存；第二次同参调用直接命中缓存，不再触达上游（也不受冷却影响）。
func TestMCP429RetryThenCacheHit(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			w.Header().Set("RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + mcpCallResultJSON + `}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcp429e-018")
	addTestKey(t, f.st, "ctx7sk-mcp429f-019")

	res1 := f.mcpDo(http.MethodPost, mcpResolveCall(1), nil)
	defer res1.Body.Close()
	if res1.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200 (429 retry succeeded)", res1.StatusCode)
	}

	res2 := f.mcpDo(http.MethodPost, mcpResolveCall(2), nil)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (cache hit)", res2.StatusCode)
	}
	// 上游恰好两次：第一跳 429 + 重试成功；缓存命中不再触达
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (429 + one success, then cached)", len(bodies))
	}
	got2, _ := io.ReadAll(res2.Body)
	id2, _ := decodeJSONRPC(t, got2)
	if string(id2) != "2" {
		t.Fatalf("second id = %s, want 2", id2)
	}

	recs, err := f.st.RecentRequests()
	if err != nil || len(recs) != 2 {
		t.Fatalf("recent requests = %d (err %v), want 2", len(recs), err)
	}
	first, second := recs[1], recs[0]
	// 回源记录挂在换 key 后实际使用的 key 上；命中记录不带 key
	if first.Cached || first.KeyID == 0 {
		t.Fatalf("first record cached/keyID = %v/%d, want false/non-zero", first.Cached, first.KeyID)
	}
	if !second.Cached || second.KeyID != 0 {
		t.Fatalf("second record cached/keyID = %v/%d, want true/0", second.Cached, second.KeyID)
	}
}

// 白名单工具调用重试额度用尽仍 429：原样透传且不缓存（非 200 不入缓存），
// 同参重发会再次完整走回源+重试路径。
func TestMCP429ExhaustedNotCached(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcp429g-020")
	addTestKey(t, f.st, "ctx7sk-mcp429h-021")
	addTestKey(t, f.st, "ctx7sk-mcp429i-022")
	addTestKey(t, f.st, "ctx7sk-mcp429j-023")

	for i := 0; i < 2; i++ {
		res := f.mcpDo(http.MethodPost, mcpResolveCall(i+1), nil)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("call %d status = %d, want 429 passthrough", i, res.StatusCode)
		}
		if !strings.Contains(string(body), "rate limited") {
			t.Fatalf("call %d body = %s, want upstream 429 body", i, body)
		}
	}
	// 每次请求各打两跳（初始 + 换 key 重试）且烧掉两个 key：4 个 key 恰好
	// 支撑两次完整重试轮；429 响应绝不入缓存
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 4 {
		t.Fatalf("upstream calls = %d, want 4 (two full retry rounds)", len(bodies))
	}
}

// TTL 过期后重新回源：注入 1 秒 TTL，条目过期后同参调用再次打上游，
// 回源结果重建缓存（第三次调用再次命中）。
func TestMCPCallCacheTTLExpiry(t *testing.T) {
	f := newMCPFixtureTTL(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + mcpCallResultJSON + `}`))
	}, time.Second)
	addTestKey(t, f.st, "ctx7sk-mcpttl-022")

	res1 := f.mcpDo(http.MethodPost, mcpResolveCall(1), nil)
	res1.Body.Close()
	res2 := f.mcpDo(http.MethodPost, mcpResolveCall(2), nil)
	res2.Body.Close()
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 1 {
		t.Fatalf("upstream calls before expiry = %d, want 1", len(bodies))
	}

	// store 缓存以整秒为过期粒度：1s TTL + 1.2s 等待确保条目已过期
	time.Sleep(1200 * time.Millisecond)

	res3 := f.mcpDo(http.MethodPost, mcpResolveCall(3), nil)
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("post-expiry status = %d, want 200", res3.StatusCode)
	}
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 2 {
		t.Fatalf("upstream calls after expiry = %d, want 2 (re-fetched)", len(bodies))
	}
	// 回源路径是原文透传：id 为上游回显值，代理只在命中组帧时改写 id，
	// 故此处只断言 result 一致
	got3, _ := io.ReadAll(res3.Body)
	_, result3 := decodeJSONRPC(t, got3)
	if string(result3) != mcpCallResultJSON {
		t.Fatalf("post-expiry result = %s, want %s", result3, mcpCallResultJSON)
	}

	// 回源后缓存已重建：第四次调用再次命中，且组帧 id 为本次请求 id
	res4 := f.mcpDo(http.MethodPost, mcpResolveCall(4), nil)
	got4, _ := io.ReadAll(res4.Body)
	res4.Body.Close()
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 2 {
		t.Fatalf("upstream calls after re-cache = %d, want 2 (fourth served from cache)", len(bodies))
	}
	id4, _ := decodeJSONRPC(t, got4)
	if string(id4) != "4" {
		t.Fatalf("post-re-cache id = %s, want 4 (cache hit reframes id)", id4)
	}
}
