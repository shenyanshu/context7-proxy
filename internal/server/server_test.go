// server 集成测试：httptest 假上游 + 真实临时 store，覆盖鉴权、
// REST 错误映射、缓存统计、管理 API 全流程与 SPA 静态。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"context7-proxy/internal/query"
	"context7-proxy/internal/store"
	"context7-proxy/web"
)

// fixture 组装被测 Server：假上游 + 真实临时 SQLite。
type fixture struct {
	t      *testing.T
	srv    *Server
	ts     *httptest.Server
	st     *store.Store
	upHits atomic.Int64
}

const testMasterKey = "test-master-key"

func newFixture(t *testing.T, upstream http.HandlerFunc) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/srv.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := &fixture{t: t, st: st}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.upHits.Add(1)
		upstream(w, r)
	}))
	t.Cleanup(up.Close)

	svc := query.New(st, st, up.URL, nil, time.Hour)
	// MCP 转发目标暂用同一假上游地址：本文件用例均不触达 /mcp，
	// 仅为满足装配参数；/mcp 专属用例在 mcp_test.go 自建 fixture
	f.srv = New(testMasterKey, up.URL, svc, st)
	f.ts = httptest.NewServer(f.srv.Handler())
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fixture) do(method, path, auth string, body string) *http.Response {
	f.t.Helper()
	// 用解析后的 URL 构造请求：httptest.NewRequest 会把整串当 RequestURI，
	// 客户端发送时报 "RequestURI can't be set"
	req, err := http.NewRequest(method, f.ts.URL+path, nil)
	if err != nil {
		f.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Body = io.NopCloser(strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

func (f *fixture) doJSON(method, path, body string) map[string]any {
	f.t.Helper()
	res := f.do(method, path, testMasterKey, body)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		f.t.Fatalf("%s %s: status %d, want 200", method, path, res.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		f.t.Fatalf("%s %s: decode: %v", method, path, err)
	}
	return out
}

func addTestKey(t *testing.T, st *store.Store, value string) int64 {
	t.Helper()
	added, err := st.AddKey(value)
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	return added.ID
}

// 鉴权：无/错 key 401，对 key 正常；/admin 静态无鉴权可访问。
func TestAuthMiddleware(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	addTestKey(t, f.st, "ctx7sk-authkey-1111")

	// 无凭证 → 401
	if res := f.do("GET", "/api/admin/overview", "", ""); res.StatusCode != http.StatusUnauthorized {
		res.Body.Close()
		t.Fatalf("no auth status = %d, want 401", res.StatusCode)
	} else {
		res.Body.Close()
	}
	// 错 key → 401
	if res := f.do("GET", "/api/admin/overview", "wrong-key", ""); res.StatusCode != http.StatusUnauthorized {
		res.Body.Close()
		t.Fatalf("wrong key status = %d, want 401", res.StatusCode)
	} else {
		res.Body.Close()
	}
	// 对 key → 200
	if res := f.do("GET", "/api/admin/overview", testMasterKey, ""); res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("correct key status = %d, want 200", res.StatusCode)
	} else {
		res.Body.Close()
	}
	// 401 body 契约
	res := f.do("GET", "/api/admin/overview", "", "")
	defer res.Body.Close()
	var e map[string]string
	if err := json.NewDecoder(res.Body).Decode(&e); err != nil || e["error"] != "unauthorized" {
		t.Fatalf("401 body = %v (err %v), want {\"error\":\"unauthorized\"}", e, err)
	}
	// /admin/ 静态不鉴权（/admin 本身是 301 规范化跳转）
	if res := f.do("GET", "/admin/", "", ""); res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("/admin/ without auth status = %d, want 200", res.StatusCode)
	} else {
		res.Body.Close()
	}
}

// REST：合法查询 200 透传上游 body 与 Content-Type；/api/v2 未知路径 404。
func TestRestHappyPathAndUnknownPath(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.custom+json")
		_, _ = w.Write([]byte(`{"up":"ok"}`))
	})
	addTestKey(t, f.st, "ctx7sk-restkey-2222")

	res := f.do("GET", "/api/v2/libs/search?libraryName=react&query=docs", testMasterKey, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/vnd.custom+json" {
		t.Fatalf("Content-Type = %q, want upstream passthrough", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body["up"] != "ok" {
		t.Fatalf("body = %v (err %v), want upstream passthrough", body, err)
	}

	// 未知子路径 404
	res2 := f.do("GET", "/api/v2/metrics?x=1", testMasterKey, "")
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown /api/v2 path status = %d, want 404", res2.StatusCode)
	}
	var e map[string]string
	if err := json.NewDecoder(res2.Body).Decode(&e); err != nil || e["error"] != "not found" {
		t.Fatalf("404 body = %v (err %v), want {\"error\":\"not found\"}", e, err)
	}
}

// REST：参数校验错 → 400，错误信息来自 query 核心。
func TestRestInvalidParams400(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	addTestKey(t, f.st, "ctx7sk-params-33333")

	res := f.do("GET", "/api/v2/libs/search?libraryName=react", testMasterKey, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	var e map[string]string
	if err := json.NewDecoder(res.Body).Decode(&e); err != nil || e["error"] == "" {
		t.Fatalf("400 body = %v (err %v), want error message", e, err)
	}
	if f.upHits.Load() != 0 {
		t.Fatalf("upstream hits = %d, want 0 on validation failure", f.upHits.Load())
	}
}

// REST：全部 key 冷却 → 429 + Retry-After 头。
func TestRestAllCooling429(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "77")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	id := addTestKey(t, f.st, "ctx7sk-cooling-444")
	if err := f.st.CooldownKey(id, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("CooldownKey: %v", err)
	}

	res := f.do("GET", "/api/v2/libs/search?libraryName=a&query=b", testMasterKey, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	// 全冷却的 Retry-After 按最早到期换算：key 冷却 1 小时，等待约 3600 秒
	ra, err := strconv.Atoi(res.Header.Get("Retry-After"))
	if err != nil || ra < 3500 || ra > 3600 {
		t.Fatalf("Retry-After = %q (err %v), want ~3600 (1h cooldown remaining)", res.Header.Get("Retry-After"), err)
	}
}

// REST：上游挂掉 → 502。
func TestRestUpstreamDown502(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/down.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	addTestKey(t, st, "ctx7sk-deadkey-5555")
	// 立即关闭假上游：连接必然失败
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upURL := up.URL
	up.Close()

	svc := query.New(st, st, upURL, nil, time.Hour)
	ts := httptest.NewServer(New(testMasterKey, upURL, svc, st).Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v2/libs/search?libraryName=a&query=b", nil)
	req.Header.Set("Authorization", "Bearer "+testMasterKey)
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res2.StatusCode)
	}
	var e map[string]string
	if err := json.NewDecoder(res2.Body).Decode(&e); err != nil || e["error"] == "" {
		t.Fatalf("502 body = %v (err %v)", e, err)
	}
}

// REST：缓存命中 + 统计——日志 2 条、上游 1 次；Cached/KeyID/时长记录正确，
// totalRequests/cacheHits 正确累计。
func TestRestCacheHitAndStats(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cached":"no"}`))
	})
	addTestKey(t, f.st, "ctx7sk-stats-6666")

	for i := 0; i < 2; i++ {
		res := f.do("GET", "/api/v2/libs/search?libraryName=react&query=docs", testMasterKey, "")
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d", i, res.StatusCode)
		}
	}
	if n := f.upHits.Load(); n != 1 {
		t.Fatalf("upstream hits = %d, want 1 (second must be cache hit)", n)
	}

	// 日志恰好 2 条：统计只在入口记一次，query 核心不重复计数
	recs, err := f.st.RecentRequests()
	if err != nil || len(recs) != 2 {
		t.Fatalf("recent requests = %d (err %v), want 2 (one per REST call)", len(recs), err)
	}
	first, second := recs[0], recs[1] // 新的在前：first=缓存命中，second=回源
	if !first.Cached || second.Cached {
		t.Fatalf("cached flags: hit=%v fresh=%v, want true then false", first.Cached, second.Cached)
	}
	if first.KeyID != 0 || first.KeyMasked != "" {
		t.Fatalf("cache hit record key fields = %d/%q, want 0/\"\" (no key on cache hit)", first.KeyID, first.KeyMasked)
	}
	if second.KeyID == 0 || second.KeyMasked == "" || second.KeyMasked == "ctx7sk-stats-6666" {
		t.Fatalf("fresh record key fields = %d/%q, want masked non-empty without raw key", second.KeyID, second.KeyMasked)
	}
	if first.DurationMs < 0 {
		t.Fatalf("durationMs = %d, want >= 0", first.DurationMs)
	}

	st, err := f.st.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if st.TotalRequests != 2 || st.CacheHits != 1 {
		t.Fatalf("stats = %+v, want total 2 / hits 1", st)
	}

	// overview 聚合口径：hitRate = 1/2
	ov := f.doJSON("GET", "/api/admin/overview", "")
	if ov["totalRequests"].(float64) != 2 || ov["cacheHits"].(float64) != 1 {
		t.Fatalf("overview counters = %v, want 2/1", ov)
	}
	if rate := ov["cacheHitRate"].(float64); rate != 0.5 {
		t.Fatalf("cacheHitRate = %v, want 0.5", rate)
	}
}

// 管理：增删启停全流程 + 错误形态。
func TestAdminKeysLifecycle(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})

	// 空库列表：keys 必须是 [] 而非 null
	res := f.do("GET", "/api/admin/keys", testMasterKey, "")
	defer res.Body.Close()
	rawBytes, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(rawBytes), `"keys":[]`) {
		t.Fatalf("empty keys body = %s, want \"keys\":[] (never null)", rawBytes)
	}

	// 新增：201 + id/value（自用场景明文回显）
	res2 := f.do("POST", "/api/admin/keys", testMasterKey, `{"value":"ctx7sk-lifecycle-7777"}`)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d, want 201", res2.StatusCode)
	}
	var added map[string]any
	if err := json.NewDecoder(res2.Body).Decode(&added); err != nil {
		t.Fatalf("decode added: %v", err)
	}
	id := int64(added["id"].(float64))
	if added["value"] != "ctx7sk-lifecycle-7777" {
		t.Fatalf("value = %v, want full plaintext key", added["value"])
	}
	if _, has := added["masked"]; has {
		t.Fatalf("masked field must be gone from add response: %v", added)
	}

	// 重复 → 409 {"error":"duplicate key"}
	res3 := f.do("POST", "/api/admin/keys", testMasterKey, `{"value":"ctx7sk-lifecycle-7777"}`)
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", res3.StatusCode)
	}
	var dup map[string]string
	_ = json.NewDecoder(res3.Body).Decode(&dup)
	if dup["error"] != "duplicate key" {
		t.Fatalf("duplicate body = %v", dup)
	}

	// 非法格式 → 400
	res4 := f.do("POST", "/api/admin/keys", testMasterKey, `{"value":"bad-key"}`)
	defer res4.Body.Close()
	if res4.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid format status = %d, want 400", res4.StatusCode)
	}

	// 停用 → {"ok":true}；再启用
	res5 := f.do("PATCH", fmt.Sprintf("/api/admin/keys/%d", id), testMasterKey, `{"enabled":false}`)
	defer res5.Body.Close()
	if res5.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", res5.StatusCode)
	}
	var ok map[string]bool
	_ = json.NewDecoder(res5.Body).Decode(&ok)
	if !ok["ok"] {
		t.Fatalf("patch body = %v, want {\"ok\":true}", ok)
	}
	// 列表状态随之变化
	list := f.doJSON("GET", "/api/admin/keys", "")
	ks := list["keys"].([]any)
	if len(ks) != 1 || ks[0].(map[string]any)["enabled"] != false {
		t.Fatalf("keys after disable = %v, want one disabled", list)
	}
	res5b := f.do("PATCH", fmt.Sprintf("/api/admin/keys/%d", id), testMasterKey, `{"enabled":true}`)
	res5b.Body.Close()
	if res5b.StatusCode != http.StatusOK {
		t.Fatalf("re-enable status = %d, want 200", res5b.StatusCode)
	}

	// 不存在 id：PATCH/DELETE → 404
	if res := f.do("PATCH", "/api/admin/keys/99999", testMasterKey, `{"enabled":true}`); res.StatusCode != http.StatusNotFound {
		res.Body.Close()
		t.Fatalf("patch missing status = %d, want 404", res.StatusCode)
	} else {
		res.Body.Close()
	}
	if res := f.do("DELETE", "/api/admin/keys/99999", testMasterKey, ""); res.StatusCode != http.StatusNotFound {
		res.Body.Close()
		t.Fatalf("delete missing status = %d, want 404", res.StatusCode)
	} else {
		res.Body.Close()
	}

	// 删除 → 200；列表回空
	res6 := f.do("DELETE", fmt.Sprintf("/api/admin/keys/%d", id), testMasterKey, "")
	defer res6.Body.Close()
	if res6.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", res6.StatusCode)
	}
	list2 := f.doJSON("GET", "/api/admin/keys", "")
	if keys := list2["keys"].([]any); len(keys) != 0 {
		t.Fatalf("keys after delete = %v, want empty", list2)
	}
}

// overview：空库时 keys/recentRequests 为 [] 且 cacheHitRate=0。
func TestOverviewEmptyArrays(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})

	res := f.do("GET", "/api/admin/overview", testMasterKey, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// json.RawMessage 保留原文：断言非 null
	if string(raw["keys"]) == "null" {
		t.Fatal("keys is null, want []")
	}
	if string(raw["recentRequests"]) == "null" {
		t.Fatal("recentRequests is null, want []")
	}
	if string(raw["keys"]) != "[]" || string(raw["recentRequests"]) != "[]" {
		t.Fatalf("keys=%s recentRequests=%s, want empty arrays", raw["keys"], raw["recentRequests"])
	}
	var rate float64
	if err := json.Unmarshal(raw["cacheHitRate"], &rate); err != nil || rate != 0 {
		t.Fatalf("cacheHitRate = %v (err %v), want 0 on empty stats", rate, err)
	}
}

// firstJSAsset 从 index.html 提取首个 JS 资产相对路径。
// 前端产物文件名带构建 hash，硬编码会随前端重建失效，动态提取保证测试对任意构建稳定。
func firstJSAsset(indexHTML string) string {
	const marker = `./assets/`
	i := strings.Index(indexHTML, marker)
	if i < 0 {
		return ""
	}
	rest := indexHTML[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return "assets/" + rest[:j]
}

// SPA：/admin 301 规范化到 /admin/（相对 base 的资产路径依赖尾斜杠，缺失会 404 空白页）；
// /admin/ 与未知子路径回退 index.html；静态资产按扩展名给 Content-Type；/ → 302 /admin/。
func TestSPAStatic(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})

	// 断言重定向本身需要禁用自动跟随（/admin 301 与根 302 共用）
	noRedirect := *http.DefaultClient
	noRedirect.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// /admin 无尾斜杠 → 301 /admin/
	reqAdmin, _ := http.NewRequest("GET", f.ts.URL+"/admin", nil)
	res301, err := noRedirect.Do(reqAdmin)
	if err != nil {
		t.Fatalf("get /admin: %v", err)
	}
	defer res301.Body.Close()
	if res301.StatusCode != http.StatusMovedPermanently || res301.Header.Get("Location") != "/admin/" {
		t.Fatalf("/admin = %d Location=%q, want 301 /admin/", res301.StatusCode, res301.Header.Get("Location"))
	}

	// /admin/ 带尾斜杠 → index.html（含前端构建标识）
	res := f.do("GET", "/admin/", "", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/admin/ status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("/admin/ Content-Type = %q, want text/html", ct)
	}
	indexBytes, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(indexBytes), "root") {
		t.Fatalf("/admin/ body missing frontend mount point")
	}

	// 未知子路径回退 index.html（HashRouter 无需 history rewrite）
	res2 := f.do("GET", "/admin/some/unknown/route", "", "")
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK || !strings.HasPrefix(res2.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("fallback status = %d ct = %q, want 200 text/html", res2.StatusCode, res2.Header.Get("Content-Type"))
	}

	// 真实资产文件按扩展名输出。资产名带构建 hash、每次构建都变，
	// 从 index.html 动态提取首个 JS 引用，杜绝测试硬编码产物文件名
	asset := firstJSAsset(string(indexBytes))
	if asset == "" {
		t.Fatalf("index.html missing js asset reference")
	}
	res3 := f.do("GET", "/admin/"+asset, "", "")
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("asset %s status = %d, want 200", asset, res3.StatusCode)
	}
	if ct := res3.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("js asset Content-Type = %q, want javascript", ct)
	}

	// / → 302 /admin/（直连带尾斜杠目标，避免二次规范化跳转）
	reqRoot, _ := http.NewRequest("GET", f.ts.URL+"/", nil)
	res4, err := noRedirect.Do(reqRoot)
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer res4.Body.Close()
	if res4.StatusCode != http.StatusFound || res4.Header.Get("Location") != "/admin/" {
		t.Fatalf("root = %d Location=%q, want 302 /admin/", res4.StatusCode, res4.Header.Get("Location"))
	}
}

// 嵌入 FS 烟雾验证：dist 存在且含入口（构建期约束，若前端未构建此处即失败）。
func TestEmbedDistPresent(t *testing.T) {
	data, err := web.DistFS.ReadFile("dist/index.html")
	if err != nil {
		t.Fatalf("embedded dist/index.html missing: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("embedded index.html is empty")
	}
}
