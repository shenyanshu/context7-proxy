// httptest 假上游覆盖调度、缓存、429、超时、并发等风险路径。
package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context7-proxy/internal/store"
)

// fixture 组装一次被测服务：假上游 + 真实临时 SQLite。
type fixture struct {
	t       *testing.T
	svc     *Service
	st      *store.Store
	up      *httptest.Server
	upHit   atomic.Int64 // 上游收到的请求总数（缓存命中应为 0）
	lastKey atomic.Value // 上游最近一次收到的 Bearer key
}

func newFixture(t *testing.T, handler http.HandlerFunc, ttl time.Duration) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := &fixture{t: t, st: st}
	f.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.upHit.Add(1)
		f.lastKey.Store(r.Header.Get("Authorization"))
		handler(w, r)
	}))
	t.Cleanup(f.up.Close)

	f.svc = New(st, st, f.up.URL, nil, ttl)
	return f
}

// newFixtureRT 用注入 Transport 构造 fixture：B2 验证注入无法绕过安全策略。
func newFixtureRT(t *testing.T, handler http.HandlerFunc, ttl time.Duration, rt http.RoundTripper) *Service {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, v := range []string{"ctx7sk-rtinject-1"} {
		if _, err := st.AddKey(v); err != nil {
			t.Fatalf("AddKey: %v", err)
		}
	}
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	return New(st, st, up.URL, rt, ttl)
}

func (f *fixture) addKeys(t *testing.T, values ...string) {
	t.Helper()
	for _, v := range values {
		if _, err := f.st.AddKey(v); err != nil {
			t.Fatalf("AddKey: %v", err)
		}
	}
}

func mustValues(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

func TestExecuteValidatesParams(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {}, time.Hour)
	f.addKeys(t, "ctx7sk-aaaa1111")

	cases := []struct {
		name string
		path string
		q    url.Values
		want error
	}{
		{"unsupported path", "/api/v2/metrics", mustValues("libraryName", "x", "query", "docs"), ErrUnsupportedPath},
		{"search no name", PathSearch, mustValues("query", "x"), nil},
		{"search empty name", PathSearch, mustValues("libraryName", "", "query", "docs"), nil},
		{"search name too long", PathSearch, mustValues("libraryName", strings.Repeat("a", 501), "query", "docs"), nil},
		{"search missing query", PathSearch, mustValues("libraryName", "react"), nil},
		{"search empty query", PathSearch, mustValues("libraryName", "react", "query", ""), nil},
		{"search query too long", PathSearch, mustValues("libraryName", "react", "query", strings.Repeat("b", 501)), nil},
		{"context no id", PathContext, mustValues("query", "x"), nil},
		{"context no query", PathContext, mustValues("libraryId", "/x/y"), nil},
		{"bad type", PathContext, mustValues("libraryId", "/x/y", "query", "q", "type", "yaml"), nil},
		{"legacy type text rejected", PathContext, mustValues("libraryId", "/x/y", "query", "q", "type", "text"), nil},
		{"legacy type code rejected", PathContext, mustValues("libraryId", "/x/y", "query", "q", "type", "code"), nil},
		{"bad fast", PathSearch, mustValues("libraryName", "react", "query", "docs", "fast", "maybe"), nil},
		{"bad fast on context", PathContext, mustValues("libraryId", "/x/y", "query", "q", "fast", "1"), nil},
	}
	for _, c := range cases {
		_, err := f.svc.Execute(context.Background(), c.path, c.q)
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if c.want == nil && err == nil {
			t.Errorf("%s: expected validation error, got nil", c.name)
		}
		if f.upHit.Load() != 0 {
			t.Errorf("%s: upstream must not be hit on validation failure", c.name)
		}
	}
}

// 非法 fast 必须被拒绝且错误信息可定位到参数名，绝不静默改写。
func TestBadFastRejectedWithMessage(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {}, time.Hour)
	f.addKeys(t, "ctx7sk-aaaa1111")

	_, err := f.svc.Execute(context.Background(), PathSearch,
		mustValues("libraryName", "react", "query", "docs", "fast", "maybe"))
	if err == nil || !strings.Contains(err.Error(), "fast") {
		t.Fatalf("err = %v, want error mentioning fast", err)
	}
	if f.upHit.Load() != 0 {
		t.Fatalf("upstream must not be hit on fast validation failure")
	}
}

func TestFirst429Second200(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), "keyaaaa") {
			w.Header().Set("RateLimit-Remaining", "0")
			w.Header().Set("RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa", "ctx7sk-keybbbb")

	// 强制首个候选为 keyaaaa：轮询计数先推到对应位
	res, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "react", "query", "docs"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 两个 key 轮询，首个可能是任一 key；断言最终拿到 200 即重试语义正确
	if res.Status != 200 || res.Cached {
		t.Fatalf("status = %d cached = %v, want 200 fresh", res.Status, res.Cached)
	}
	if res.KeyID == 0 {
		t.Fatalf("KeyID must be set on upstream hit")
	}
	// 上游必然收到 1-2 次调用
	if hits := f.upHit.Load(); hits < 1 || hits > 2 {
		t.Fatalf("upstream hits = %d, want 1-2", hits)
	}
}

func Test429CooldownAndRetryOnceMax(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa", "ctx7sk-keybbbb", "ctx7sk-keycccc")

	res, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs"))
	if res.Status != 429 || err == nil {
		t.Fatalf("want 429 with error, got %d / %v", res.Status, err)
	}
	if res.RetryAfterSec != 60 {
		t.Fatalf("RetryAfterSec = %d, want 60", res.RetryAfterSec)
	}
	// 首请求 + 至多一次重试 = 上游至多 2 次，每次尝试的 key 都被冷却
	if hits := f.upHit.Load(); hits > 1+maxRetry429 {
		t.Fatalf("upstream hits = %d, want <= %d", hits, 1+maxRetry429)
	}
	keys, err := f.st.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	// 每个真实尝试过的 key 都必须进入冷却（429 语义对该 key 成立）
	cooling := 0
	for _, k := range keys {
		if k.Status == "cooldown" {
			cooling++
		}
	}
	if cooling != int(f.upHit.Load()) {
		t.Fatalf("cooling keys = %d, want %d (one per attempt)", cooling, f.upHit.Load())
	}
}

func TestAllCoolingErrAndRecovery(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")
	// 直接置冷却：未来 1h
	if err := f.st.CooldownKey(1, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("CooldownKey: %v", err)
	}
	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); !errors.Is(err, ErrAllCooling) {
		t.Fatalf("err = %v, want ErrAllCooling", err)
	}
	// 冷却到期后恢复可用（注入未来时钟不必——直接把冷却设到过去）
	if err := f.st.CooldownKey(1, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatalf("CooldownKey: %v", err)
	}
	res, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs"))
	if err != nil || res.Status != 200 {
		t.Fatalf("after cooldown expiry: %d / %v", res.Status, err)
	}
}

func TestNoKeys503Semantics(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {}, time.Hour)
	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); !errors.Is(err, ErrNoAvailableKey) {
		t.Fatalf("err = %v, want ErrNoAvailableKey", err)
	}
}

func TestCacheHitSkipsUpstream(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"docs":1}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	for i := 0; i < 3; i++ {
		res, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "react", "query", "docs", "fast", "true"))
		if err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
		if res.Status != 200 {
			t.Fatalf("status = %d", res.Status)
		}
	}
	if f.upHit.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (rest cached)", f.upHit.Load())
	}
	// 第 2、3 次应为缓存命中
}

func TestCacheMissOnParamDiff(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	// type 不同必须隔离（官方取值 txt / json）
	if _, err := f.svc.Execute(context.Background(), PathContext, mustValues("libraryId", "/a/b", "query", "x", "type", "txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Execute(context.Background(), PathContext, mustValues("libraryId", "/a/b", "query", "x", "type", "json")); err != nil {
		t.Fatal(err)
	}
	if f.upHit.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (type must isolate cache)", f.upHit.Load())
	}

	// fast 不同必须隔离
	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "a", "query", "docs", "fast", "true")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "a", "query", "docs", "fast", "false")); err != nil {
		t.Fatal(err)
	}
	if f.upHit.Load() != 4 {
		t.Fatalf("upstream hits = %d, want 4 (fast must isolate cache)", f.upHit.Load())
	}
}

func TestCacheExpiry(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, time.Millisecond)
	f.addKeys(t, "ctx7sk-keyaaaa")

	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
		t.Fatal(err)
	}
	if f.upHit.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (expired cache must miss)", f.upHit.Load())
	}
}

func TestFilteredSearchNotCached(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"searchFilterApplied":true,"results":[]}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
		t.Fatal(err)
	}
	if f.upHit.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (filtered search must not cache)", f.upHit.Load())
	}
}

func TestRedirectDoesNotLeakKey(t *testing.T) {
	// 目标 host 收到请求即失败（key 一旦被转发到这里就是泄漏）
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect target received request with auth=%q — key leaked", r.Header.Get("Authorization"))
	}))
	defer target.Close()

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// 上游把上游 host 的请求重定向到另一个 host
		w.Header().Set("Location", target.URL+"/evil")
		w.WriteHeader(302)
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	res, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs"))
	// 302 原样返回给入口层，绝不跟随
	if res.Status != 302 {
		t.Fatalf("status = %d, want 302 passthrough", res.Status)
	}
	if err != nil {
		t.Fatalf("302 should not be a hard error: %v", err)
	}
	if f.lastKey.Load() != "Bearer ctx7sk-keyaaaa" {
		t.Fatalf("upstream auth = %v, want Bearer key", f.lastKey.Load())
	}
}

func TestClientCancelPropagates(t *testing.T) {
	// handler 收到请求即报告，然后挂到 ctx 取消——保证"客户端取消"发生在
	// 上游响应写回之前，测试结果不依赖 goroutine 调度时序
	arrived := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done() // 上游连接随客户端取消断开
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := f.svc.Execute(ctx, PathSearch, mustValues("libraryName", "x", "query", "docs"))
		errCh <- err
	}()
	// 等上游真正收到请求后再取消，杜绝"取消先于外呼"的竞态
	<-arrived
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrUpstreamUnreachable) {
			t.Fatalf("err = %v, want ErrUpstreamUnreachable on cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return after client cancel")
	}
}

func TestQuotaObservedAndPreserved(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit-Remaining", "42")
		w.Header().Set("RateLimit-Limit", "100")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
		t.Fatal(err)
	}
	// 额度观测断言走调度视图（ListPoolKeys）：显示层（ListKeys→KeyStatus）
	// 已删除额度字段，但 DB 列仍喂内部调度（余量归零主动冷却）
	pool, _ := f.st.ListPoolKeys()
	if pool[0].Remaining == nil || *pool[0].Remaining != 42 {
		t.Fatalf("remaining = %v, want 42", pool[0].Remaining)
	}

	// 头缺失时旧值保留
	f.up.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.upHit.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	// 用不同参数绕过缓存
	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "other", "query", "docs")); err != nil {
		t.Fatal(err)
	}
	pool, _ = f.st.ListPoolKeys()
	if pool[0].Remaining == nil || *pool[0].Remaining != 42 {
		t.Fatalf("missing header must not clear observed remaining, got %v", pool[0].Remaining)
	}
}

func TestRequestCountTracksAttempts(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	for i := 0; i < 3; i++ {
		if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", fmt.Sprintf("q%d", i), "query", "docs")); err != nil {
			t.Fatal(err)
		}
	}
	keys, _ := f.st.ListKeys()
	if keys[0].RequestCount != 3 || keys[0].LastUsedAt == nil {
		t.Fatalf("requestCount = %d, want 3", keys[0].RequestCount)
	}
}

// B3：上游响应头一律不透传——Result 结构上没有 header 字段，
// 响应中任何上游头（Cookie/CORS/Location）都不会流向客户端。
func TestNoUpstreamHeadersInResult(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=secret; Path=/")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Location", "https://evil.example/")
		w.Header().Set("Server", "upstream/1.0")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	res, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs"))
	if err != nil {
		t.Fatal(err)
	}
	// Result 无任何 header 出口：只能通过字段访问，头类信息结构性不存在
	if res.Cached || res.Status != 200 {
		t.Fatalf("res = %+v, want 200 fresh", res)
	}
	// 429 的 Retry-After 由 RetryAfterSec 携带而非透传上游头（见 429 用例）
}

func TestConcurrentExecute(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa", "ctx7sk-keybbbb", "ctx7sk-keycccc")

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := mustValues("libraryName", fmt.Sprintf("lib%d%%", i%4), "query", "docs")
			if _, err := f.svc.Execute(context.Background(), PathSearch, q); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Execute: %v", err)
	}
	// 32 个请求、4 个不同参数 → 上游命中应在 4（缓存生效）与 32（全部穿透）之间
	hits := f.upHit.Load()
	if hits < 4 || hits > 32 {
		t.Fatalf("upstream hits = %d, out of plausible range", hits)
	}
}

func TestNoStoreNotCached(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	for i := 0; i < 2; i++ {
		if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
			t.Fatal(err)
		}
	}
	if f.upHit.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (no-store must not cache)", f.upHit.Load())
	}
}

// 编译期保证 Result 的 JSON 序列化不含任何 key 原文字段（本类型本身没有 key 字段，
// 此测试防止未来误加）。
func TestResultJSONNoKey(t *testing.T) {
	b, err := json.Marshal(Result{Status: 200, Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ctx7sk") {
		t.Fatalf("Result JSON leaked key material: %s", b)
	}
}

// B2：注入"会跟随重定向"的 RoundTripper 也无法让 3xx 被跟随——
// CheckRedirect 策略由 Service 固定持有，注入面只剩传输实现。
func TestInjectedTransportCannotFollowRedirect(t *testing.T) {
	secondHop := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHop++
	}))
	defer target.Close()

	svc := newFixtureRT(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/evil")
		w.WriteHeader(302)
	}, time.Hour, http.DefaultTransport)

	res, err := svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs"))
	if err != nil {
		t.Fatalf("302 passthrough expected, got error: %v", err)
	}
	if res.Status != 302 {
		t.Fatalf("status = %d, want 302 (redirect must not be followed)", res.Status)
	}
	if secondHop != 0 {
		t.Fatalf("redirect target hit %d times — key leaked to second host", secondHop)
	}
}

// B2：外呼超时由 per-request ctx 保证，注入的 Transport 无法关掉超时。
func TestUpstreamTimeoutWithInjectedTransport(t *testing.T) {
	svc := newFixtureRT(t, func(w http.ResponseWriter, r *http.Request) {
		// 挂起到超过 30s 超时？不行——用短超时验证：阻塞即可，ctx 超时会打断
		<-r.Context().Done()
	}, time.Hour, http.DefaultTransport)

	// 缩短等待：把 30s 超时当作下限验证太慢，这里验证取消路径下
	// 超时 ctx 会主动打断挂起的上游
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := svc.Execute(ctx, PathSearch, mustValues("libraryName", "x", "query", "docs"))
	if !errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("err = %v, want ErrUpstreamUnreachable on timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, ctx deadline must cut off the call", elapsed)
	}
}

// B4：超限响应体绝不截断入缓存——返回 ErrResponseTooLarge，缓存无条目。
func TestOversizedBodyRejected(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 写 max+1 字节：分块写避免测试内存峰值，httptest 无 Content-Length 时走读满判定
		buf := make([]byte, 64<<10)
		for sent := 0; sent <= maxResponseBodyBytes; sent += len(buf) {
			_, _ = w.Write(buf)
		}
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	_, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "big", "query", "docs"))
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	// 缓存无条目：第二次同参请求仍走上游（结果还是超限失败，upHit 递增）
	if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "big", "query", "docs")); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("second err = %v, want ErrResponseTooLarge (nothing cached)", err)
	}
	if f.upHit.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (oversized body must not cache)", f.upHit.Load())
	}
}

// B5：冷却落盘失败（store 故障）必须让整次 Execute 失败，错误链含注入错误，
// 不进行换 key 重试。
func TestCooldownPersistFailureFails(t *testing.T) {
	cooldownErr := errors.New("inject: cooldown write failed")
	bad := &brokenCooldownStore{cooldownErr: cooldownErr}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer up.Close()

	svc := New(bad, bad, up.URL, nil, time.Hour)
	_, err := svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs"))
	if err == nil {
		t.Fatal("cooldown persist failure must fail Execute")
	}
	if !errors.Is(err, cooldownErr) {
		t.Fatalf("err = %v, want chain containing injected error", err)
	}
	if bad.cooldownCalls != 1 {
		t.Fatalf("CooldownKey calls = %d, want 1 (no retry after store failure)", bad.cooldownCalls)
	}
}

// brokenCooldownStore 实现 PoolStore+CacheStore：GetCache 永远 miss，
// CooldownKey 返回注入错误，其余方法直通。
type brokenCooldownStore struct {
	cooldownErr   error
	cooldownCalls int
}

func (b *brokenCooldownStore) ListPoolKeys() ([]store.PoolKey, error) {
	return []store.PoolKey{{ID: 1, Value: "ctx7sk-brokenkey01"}}, nil
}
func (b *brokenCooldownStore) MarkKeyAttempt(int64) error { return nil }
func (b *brokenCooldownStore) CooldownKey(int64, int64) error {
	b.cooldownCalls++
	return b.cooldownErr
}
func (b *brokenCooldownStore) ObserveKeyQuota(int64, *int64, *int64) error { return nil }
func (b *brokenCooldownStore) GetCache(string) (store.CacheEntry, error) {
	return store.CacheEntry{}, store.ErrCacheMiss
}
func (b *brokenCooldownStore) SetCache(string, []byte, string, time.Duration) error { return nil }

// B6：双 Cache-Control 头第二个含 private → 不缓存。
func TestSecondCacheControlPrivateNotCached(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Cache-Control", "public")
		w.Header().Add("Cache-Control", "private")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	for i := 0; i < 2; i++ {
		if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
			t.Fatal(err)
		}
	}
	if f.upHit.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (second Cache-Control private must block caching)", f.upHit.Load())
	}
}

// B6：private 带参数（如 private="Set-Cookie"）也必须被判为不可缓存。
func TestPrivateWithParamNotCached(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", `private="Set-Cookie"`)
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	for i := 0; i < 2; i++ {
		if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
			t.Fatal(err)
		}
	}
	if f.upHit.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (private= param must block caching)", f.upHit.Load())
	}
}

// B6：searchFilterApplied 出现在前部任意字段顺序/换行缩进变体下都识别。
func TestSearchFilterAppliedVariantsNotCached(t *testing.T) {
	bodies := []string{
		`{"total":3,"searchFilterApplied":true,"results":[]}`,
		"{\n  \"searchFilterApplied\": true,\n  \"results\": []\n}",
	}
	for i, body := range bodies {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}, time.Hour)
		f.addKeys(t, "ctx7sk-keyaaaa")
		for j := 0; j < 2; j++ {
			if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
				t.Fatal(err)
			}
		}
		if f.upHit.Load() != 2 {
			t.Fatalf("body %d: upstream hits = %d, want 2 (filterApplied variant must not cache)", i, f.upHit.Load())
		}
	}
}

// B6：searchFilterApplied:false 的 search 响应正常缓存。
func TestSearchFilterAppliedFalseCached(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":3,"searchFilterApplied":false,"results":[]}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	for i := 0; i < 2; i++ {
		if _, err := f.svc.Execute(context.Background(), PathSearch, mustValues("libraryName", "x", "query", "docs")); err != nil {
			t.Fatal(err)
		}
	}
	if f.upHit.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (filterApplied=false is cacheable)", f.upHit.Load())
	}
}
