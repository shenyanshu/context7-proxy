// 缓存查看 API 测试：列表空数组非 null、字段契约、过期过滤、
// 特殊字符 key 的详情取回、404/401 形态。
package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"context7-proxy/internal/store"
)

// 空表 entries 必须是 [] 而非 null（json.RawMessage 保留原文断言）。
func TestCacheListEmpty(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	res := f.do("GET", "/api/admin/cache", testMasterKey, "")
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["entries"]) == "null" {
		t.Fatal("entries is null, want []")
	}
	if string(raw["entries"]) != "[]" {
		t.Fatalf("entries = %s, want []", raw["entries"])
	}
}

// 列表契约：key/size/contentType/expiresAt 字段正确、按 hash 升序、
// 过期条目不出现（与 GetCache 口径一致）。
func TestCacheListFieldsAndExpiryFilter(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})

	if err := f.st.SetCache("/api/v2/libs/search?libraryName=react&query=hooks",
		[]byte(`{"results":[1,2,3]}`), "application/json", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetCache("mcp:resolve-library-id:{\"libraryName\":\"react\",\"query\":\"hooks\"}",
		[]byte("plain"), "text/plain", -time.Minute); err != nil { // 已过期
		t.Fatal(err)
	}
	if err := f.st.SetCache("/api/v2/context?libraryId=/a/b&query=x&type=txt",
		[]byte(`{"docs":true}`), "application/json", 2*time.Hour); err != nil {
		t.Fatal(err)
	}

	res := f.do("GET", "/api/admin/cache", testMasterKey, "")
	defer res.Body.Close()
	var out struct {
		Entries []store.CacheListEntry `json:"entries"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (expired must be filtered)", len(out.Entries))
	}
	// 按 hash 升序："/api/v2/context..." < "/api/v2/libs/search..."（字节序）
	if out.Entries[0].Key != "/api/v2/context?libraryId=/a/b&query=x&type=txt" {
		t.Fatalf("order broken: first key = %q", out.Entries[0].Key)
	}
	if out.Entries[0].ContentType != "application/json" || out.Entries[0].Size != int64(len(`{"docs":true}`)) {
		t.Fatalf("entry 0 = %+v, want contentType json size = body bytes", out.Entries[0])
	}
	if out.Entries[1].Key != "/api/v2/libs/search?libraryName=react&query=hooks" ||
		out.Entries[1].Size != int64(len(`{"results":[1,2,3]}`)) {
		t.Fatalf("entry 1 = %+v, wrong key/size", out.Entries[1])
	}
	if out.Entries[0].ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiresAt = %d, must be future", out.Entries[0].ExpiresAt)
	}
}

// 详情：含 ?、&、冒号、花括号的 key 经 URL 编码请求能取回原文 body；
// 不存在/已过期 → 404；无鉴权 → 401。
func TestCacheDetailRoundtripAndErrors(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})

	keys := []string{
		`/api/v2/libs/search?libraryName=react&query=hooks`,
		`mcp:resolve-library-id:{"libraryName":"react","query":"hooks"}`,
	}
	for i, k := range keys {
		if err := f.st.SetCache(k, []byte(`{"round":`+string(rune('0'+i))+`}`), "application/json", time.Hour); err != nil {
			t.Fatal(err)
		}
		// encodeURIComponent 整个缓存键：? & : { } 全部转义
		res := f.do("GET", "/api/admin/cache/"+url.PathEscape(k), testMasterKey, "")
		if res.StatusCode != 200 {
			res.Body.Close()
			t.Fatalf("key %q: status = %d, want 200", k, res.StatusCode)
		}
		var d struct {
			Key         string `json:"key"`
			ContentType string `json:"contentType"`
			Body        string `json:"body"`
		}
		if err := json.NewDecoder(res.Body).Decode(&d); err != nil {
			res.Body.Close()
			t.Fatalf("decode: %v", err)
		}
		res.Body.Close()
		if d.Key != k || d.ContentType != "application/json" || d.Body != `{"round":`+string(rune('0'+i))+`}` {
			t.Fatalf("detail = %+v, want roundtrip of %q", d, k)
		}
	}

	// 不存在 → 404 统一错误体
	res := f.do("GET", "/api/admin/cache/"+url.PathEscape("no/such:key?"), testMasterKey, "")
	defer res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("missing entry status = %d, want 404", res.StatusCode)
	}
	var e map[string]string
	if err := json.NewDecoder(res.Body).Decode(&e); err != nil || e["error"] != "cache entry not found" {
		t.Fatalf("404 body = %v (err %v)", e, err)
	}

	// 已过期 → 404（详情口径与列表/GetCache 一致）
	if err := f.st.SetCache("expired:key", []byte("x"), "text/plain", -time.Minute); err != nil {
		t.Fatal(err)
	}
	res2 := f.do("GET", "/api/admin/cache/expired:key", testMasterKey, "")
	defer res2.Body.Close()
	if res2.StatusCode != 404 {
		t.Fatalf("expired entry status = %d, want 404", res2.StatusCode)
	}

	// 无鉴权 → 401（/api/ 前缀中间件覆盖新路由）
	res3 := f.do("GET", "/api/admin/cache", "", "")
	defer res3.Body.Close()
	if res3.StatusCode != 401 {
		t.Fatalf("no auth status = %d, want 401", res3.StatusCode)
	}
	res4 := f.do("GET", "/api/admin/cache/somekey", "", "")
	defer res4.Body.Close()
	if res4.StatusCode != 401 {
		t.Fatalf("no auth detail status = %d, want 401", res4.StatusCode)
	}
}
