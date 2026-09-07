// MCP 透明转发集成测试：httptest 假官方 MCP 上游 + 真实 query.Service +
// 临时 SQLite，覆盖鉴权、透传保真、429 冷却换 key 重试、流式透传与记账。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"context7-proxy/internal/query"
	"context7-proxy/internal/store"
)

// mcpFixture 组装被测 /mcp：假官方 MCP 上游（记录每次请求的鉴权头与体）
// + 真实临时 store。REST 假上游仅占位——MCP 用例不触达 REST 查询路径。
type mcpFixture struct {
	t       *testing.T
	ts      *httptest.Server
	st      *store.Store
	up      *httptest.Server
	mu      sync.Mutex
	auths   []string // 每次转发收到的 Authorization 头，按顺序
	bodies  []string // 每次转发收到的请求体原文
	methods []string
}

// newMCPFixture 用给定的假官方 MCP 上游 handler 组装 fixture（缓存 TTL
// 用与生产默认同量级的 1 小时）。
func newMCPFixture(t *testing.T, mcpUpstream http.HandlerFunc) *mcpFixture {
	return newMCPFixtureTTL(t, mcpUpstream, time.Hour)
}

// newMCPFixtureTTL 同 newMCPFixture 但允许注入缓存 TTL：Server 的缓存
// TTL 取自 query.Service，经此入口让 TTL 过期类用例可控。
func newMCPFixtureTTL(t *testing.T, mcpUpstream http.HandlerFunc, ttl time.Duration) *mcpFixture {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/mcp.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := &mcpFixture{t: t, st: st}
	f.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.bodies = append(f.bodies, string(body))
		f.methods = append(f.methods, r.Method)
		f.mu.Unlock()
		mcpUpstream(w, r)
	}))
	t.Cleanup(f.up.Close)

	svc := query.New(st, st, "http://rest.invalid", nil, ttl)
	f.ts = httptest.NewServer(New(testMasterKey, f.up.URL, svc, st).Handler())
	t.Cleanup(f.ts.Close)
	return f
}

// mcpDo 向 /mcp 发请求；hdr 为 nil 时默认携带 Bearer 鉴权与流式双 Accept。
func (f *mcpFixture) mcpDo(method, body string, hdr map[string]string) *http.Response {
	f.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.ts.URL+mcpPath, rd)
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if hdr == nil {
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
	} else {
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("do %s /mcp: %v", method, err)
	}
	return res
}

func (f *mcpFixture) upstreamCalls() (auths, bodies, methods []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.auths...), append([]string{}, f.bodies...),
		append([]string{}, f.methods...)
}

const mcpToolsListBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

const mcpToolsListResp = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"resolve-library-id"},{"name":"query-docs"}]}}`

// MCP 鉴权：无头 401；三种合法形态各自通过；错误自定义头 401。
func TestMCPAuth(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mcpToolsListResp))
	})
	addTestKey(t, f.st, "ctx7sk-mcpauth-0001")

	// 无鉴权头 → 401 + 统一错误契约（不触达上游）
	res := f.mcpDo(http.MethodPost, mcpToolsListBody, map[string]string{})
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no auth status = %d, want 401", res.StatusCode)
	}
	var e map[string]string
	if err := json.NewDecoder(res.Body).Decode(&e); err != nil || e["error"] != "unauthorized" {
		t.Fatalf("401 body decode = %v (err %v), want {\"error\":\"unauthorized\"}", e, err)
	}
	if _, bodies, _ := f.upstreamCalls(); len(bodies) != 0 {
		t.Fatalf("unauthorized request reached upstream (%d calls)", len(bodies))
	}

	// 三种合法凭证形态各一
	for name, hdr := range map[string]string{
		"Authorization":      "Authorization: Bearer " + testMasterKey,
		"x-context7-api-key": "x-context7-api-key: " + testMasterKey,
		"context7-api-key":   "context7-api-key: " + testMasterKey,
	} {
		k, v, _ := strings.Cut(hdr, ": ")
		res := f.mcpDo(http.MethodPost, mcpToolsListBody, map[string]string{k: v})
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			t.Fatalf("%s: status = %d, want 200 (forwarded)", name, res.StatusCode)
		}
		res.Body.Close()
	}

	// 自定义头携带错误值 → 401
	res2 := f.mcpDo(http.MethodPost, mcpToolsListBody, map[string]string{"x-context7-api-key": "wrong"})
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong custom header status = %d, want 401", res2.StatusCode)
	}
}

// 透传保真：上游收到选中 key 的 Bearer 与逐字节不变的请求体；
// 客户端鉴权头被剥掉不透传；响应体原样回到客户端。
func TestMCPPassthroughFidelity(t *testing.T) {
	const keyVal = "ctx7sk-mcppass-02"
	var sawClientAuthHeaders bool
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Context7-Api-Key") != "" ||
			r.Header.Get("Context7-Api-Key") != "" ||
			strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+testMasterKey) {
			sawClientAuthHeaders = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mcpToolsListResp))
	})
	addTestKey(t, f.st, keyVal)

	res := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	got, _ := io.ReadAll(res.Body)
	if string(got) != mcpToolsListResp {
		t.Fatalf("client body = %s, want exact upstream body", got)
	}

	auths, bodies, _ := f.upstreamCalls()
	if len(auths) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(auths))
	}
	if auths[0] != "Bearer "+keyVal {
		t.Fatalf("upstream Authorization = %q, want Bearer %q (selected key)", auths[0], keyVal)
	}
	if bodies[0] != mcpToolsListBody {
		t.Fatalf("upstream body = %q, want byte-identical client body", bodies[0])
	}
	if sawClientAuthHeaders {
		t.Fatal("client master-key auth headers leaked to upstream")
	}
}

// 上游 429（带 RateLimit-Reset）：冷却第一个 key，换 key 重试一次成功；
// 第一个 key 冷却落盘（后续调度不再选它）。
func TestMCP429RetryWithCooldown(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			// 第一跳：网关限流，给出未来 1 小时的 RateLimit-Reset
			w.Header().Set("RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mcpToolsListResp))
	})
	addTestKey(t, f.st, "ctx7sk-mcp429a-003")
	addTestKey(t, f.st, "ctx7sk-mcp429b-004")

	res := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (retry succeeded)", res.StatusCode)
	}
	auths, _, _ := f.upstreamCalls()
	if len(auths) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (429 then retry)", len(auths))
	}
	if auths[0] == auths[1] {
		t.Fatalf("retry reused the same key %q, want rotated to second key", auths[0])
	}

	// 第一个 key 冷却落盘：1 小时窗口，管理视图 status=cooldown
	keys, err := f.st.ListKeys()
	if err != nil || len(keys) != 2 {
		t.Fatalf("ListKeys = %v (err %v), want 2 keys", keys, err)
	}
	cooled := 0
	for _, k := range keys {
		if k.Status == "cooldown" {
			cooled++
		}
	}
	if cooled != 1 {
		t.Fatalf("cooldown keys = %d, want exactly 1 (the 429'd key)", cooled)
	}

	// 冷却生效：后续转发只剩第二个 key 可选
	res2 := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	defer res2.Body.Close()
	auths2, _, _ := f.upstreamCalls()
	if len(auths2) != 3 || auths2[2] != auths[1] {
		t.Fatalf("post-cooldown picks = %v, want third call on second key only", auths2)
	}
}

// 重试仍 429：原样返回客户端，两个 key 都冷却。
func TestMCP429RetryExhausted(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcp429c-005")
	addTestKey(t, f.st, "ctx7sk-mcp429d-006")

	res := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passthrough", res.StatusCode)
	}
	auths, _, _ := f.upstreamCalls()
	if len(auths) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (initial + one retry)", len(auths))
	}
	keys, err := f.st.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	for _, k := range keys {
		if k.Status != "cooldown" {
			t.Fatalf("key %d status = %q, want cooldown after 429s", k.ID, k.Status)
		}
	}
}

// 无 key 与全冷却 → 503，不触达上游。
func TestMCPNoKeyAndAllCooling(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no-key request must not reach upstream")
	})

	// 空池 → 503 no upstream keys available
	res := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("empty pool status = %d, want 503", res.StatusCode)
	}
	var e map[string]string
	_ = json.NewDecoder(res.Body).Decode(&e)
	if e["error"] != "no upstream keys available" {
		t.Fatalf("empty pool body = %v", e)
	}

	// 全冷却 → 503
	id := addTestKey(t, f.st, "ctx7sk-mcpcool-007")
	if err := f.st.CooldownKey(id, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("CooldownKey: %v", err)
	}
	res2 := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("all cooling status = %d, want 503", res2.StatusCode)
	}
	var e2 map[string]string
	_ = json.NewDecoder(res2.Body).Decode(&e2)
	if e2["error"] != "all upstream keys cooling down" {
		t.Fatalf("all cooling body = %v", e2)
	}
}

// SSE 流式响应直接透传：Content-Type 与 body 原样到达客户端。
func TestMCPStreamPassthrough(t *testing.T) {
	const sseBody = "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody))
	})
	addTestKey(t, f.st, "ctx7sk-mcpsse-008")

	res := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, contentTypeSSE) {
		t.Fatalf("Content-Type = %q, want text/event-stream passthrough", ct)
	}
	got, _ := io.ReadAll(res.Body)
	if string(got) != sseBody {
		t.Fatalf("streamed body = %q, want identical to upstream", got)
	}
}

// GET/DELETE 透传：上游 405 原样返回（含方法语义，不由代理代答）。
func TestMCPGetDeletePassthrough(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	})
	addTestKey(t, f.st, "ctx7sk-mcpgd-009")

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		res := f.mcpDo(method, "", nil)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405 from upstream", method, res.StatusCode)
		}
		if !strings.Contains(string(body), "Method Not Allowed") {
			t.Fatalf("%s body = %q, want upstream 405 body", method, body)
		}
	}
	_, _, methods := f.upstreamCalls()
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodDelete {
		t.Fatalf("forwarded methods = %v, want [GET DELETE]", methods)
	}
}

// 记账：tools/call 的 Path 记工具名并计入统计；协议开销流量（tools/list
// 等解析不出工具名的请求）零额度消耗，不产生日志也不计入 totalRequests。
func TestMCPRecordRequest(t *testing.T) {
	f := newMCPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	})
	addTestKey(t, f.st, "ctx7sk-mcpstat-010")

	list := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	list.Body.Close()
	list2 := f.mcpDo(http.MethodPost, mcpToolsListBody, nil)
	list2.Body.Close()
	call := f.mcpDo(http.MethodPost,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"resolve-library-id","arguments":{"libraryName":"React","query":"hooks"}}}`,
		nil)
	call.Body.Close()

	recs, err := f.st.RecentRequests()
	if err != nil || len(recs) != 1 {
		t.Fatalf("recent requests = %d (err %v), want 1 (tools/list not recorded)", len(recs), err)
	}
	rec := recs[0]
	if rec.Method != "MCP" || rec.Path != "resolve-library-id" {
		t.Fatalf("tools/call record method/path = %q/%q, want MCP/resolve-library-id", rec.Method, rec.Path)
	}
	if rec.Status != 200 || rec.Cached {
		t.Fatalf("record status/cached = %d/%v, want 200/false", rec.Status, rec.Cached)
	}
	if rec.KeyID == 0 || rec.KeyMasked == "" || rec.KeyMasked == "ctx7sk-mcpstat-010" {
		t.Fatalf("record key fields = %d/%q, want masked non-empty without raw key", rec.KeyID, rec.KeyMasked)
	}
	if rec.DurationMs < 1 {
		t.Fatalf("record durationMs = %d, want >= 1", rec.DurationMs)
	}

	// 统计口径：两发 tools/list 不进分母，totalRequests 只含 tools/call 那 1 条
	st, err := f.st.GetStats()
	if err != nil || st.TotalRequests != 1 {
		t.Fatalf("totalRequests = %d (err %v), want 1 (only the tools/call)", st.TotalRequests, err)
	}
}

// mcpToolName 单元：tools/call 提取工具名，其余/畸形体回退 /mcp。
func TestMCPToolNameParsing(t *testing.T) {
	if got := mcpToolName([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query-docs","arguments":{}}}`)); got != "query-docs" {
		t.Fatalf("tool name = %q, want query-docs", got)
	}
	for _, bad := range []string{
		`{"jsonrpc":"2.0","method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{}}`,
		`not json`,
	} {
		if got := mcpToolName([]byte(bad)); got != mcpPath {
			t.Fatalf("mcpToolName(%s) = %q, want %q", bad, got, mcpPath)
		}
	}
}
