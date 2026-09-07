// 本批强化项测试：singleflight 防击穿、参数拒绝、429 重读池快照、
// 轮询确定性、ctx 错误链保留。
package query

import (
	"context"
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

// 项 1：同一 cache key 并发 miss 只允许一次真实回源，其余等待者共享结果。
func TestSingleflightMergesConcurrentMiss(t *testing.T) {
	var hits atomic.Int64
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(50 * time.Millisecond) // 保证 32 个请求都落在 miss 窗口内
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":1}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	q := mustValues("libraryName", "unique-lib", "query", "docs")
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.svc.Execute(context.Background(), PathSearch, q); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent same-key Execute: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1 (singleflight must merge concurrent miss)", n)
	}
}

// 项 1 辅证：缓存命中路径不走 singleflight，也不会重复回源。
func TestSingleflightNotUsedOnCacheHit(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	q := mustValues("libraryName", "cached-lib", "query", "docs")
	if _, err := f.svc.Execute(context.Background(), PathSearch, q); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.svc.Execute(context.Background(), PathSearch, q); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.upHit.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (cache hit path must stay cached)", f.upHit.Load())
	}
}

// 项 2：重复参数必须被拒绝，且错误信息可定位参数名。
func TestRepeatedParamsRejected(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		q       func() url.Values
		wantSub string
	}{
		{"fast repeated", PathSearch, func() url.Values {
			v := mustValues("libraryName", "react", "query", "docs")
			v["fast"] = []string{"true", "false"}
			return v
		}, "fast"},
		{"type repeated", PathContext, func() url.Values {
			v := mustValues("libraryId", "/x/y", "query", "q")
			v["type"] = []string{"txt", "json"}
			return v
		}, "type"},
		{"query repeated", PathSearch, func() url.Values {
			v := url.Values{}
			v["libraryName"] = []string{"react"}
			v["query"] = []string{"a", "b"}
			return v
		}, "query"},
	}
	for _, c := range cases {
		_, err := validateParams(c.path, c.q())
		if err == nil {
			t.Errorf("%s: want rejection, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) || !strings.Contains(err.Error(), "repeated") {
			t.Errorf("%s: err = %v, want repeated-parameter error mentioning %q", c.name, err, c.wantSub)
		}
	}
}

// 项 2：未知参数必须被拒绝，错误信息含参数名。
func TestUnknownParamRejected(t *testing.T) {
	_, err := validateParams(PathSearch,
		mustValues("libraryName", "react", "query", "docs", "foo", "bar"))
	if err == nil || !strings.Contains(err.Error(), "foo") {
		t.Fatalf("err = %v, want error mentioning unknown param foo", err)
	}
	_, err = validateParams(PathContext,
		mustValues("libraryId", "/x/y", "query", "q", "libraryName", "react"))
	if err == nil || !strings.Contains(err.Error(), "libraryName") {
		t.Fatalf("cross-endpoint param must be rejected with param name, got %v", err)
	}
}

// 项 2：validateParams 不得修改调用方传入的 url.Values。
func TestValidateParamsDoesNotMutateInput(t *testing.T) {
	q := mustValues("libraryName", "react", "query", "docs", "fast", "")
	before := q.Encode()
	if _, err := validateParams(PathSearch, q); err != nil {
		t.Fatalf("validateParams: %v", err)
	}
	if after := q.Encode(); after != before {
		t.Fatalf("input mutated: before %q after %q", before, after)
	}
}

// 项 3：429 重试时重读池快照——首个 key 429 后，另一 key 恰被并发冷却，
// 重试绝不能选中冷却中的 key。
func Test429RetryRereadsPoolSnapshot(t *testing.T) {
	// 上游只对 keyaaaa 返回 429；若重试错误地再打冷却中的 keybbbb，
	// 会返回 200 且 upHit=2，断言即可识破
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), "keyaaaa") {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"wrong":"key"}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa", "ctx7sk-keybbbb")

	// 直接把 keybbbb 冷却到未来，模拟“重试瞬间它刚被其他请求冷却”
	if err := f.st.CooldownKey(2, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("CooldownKey: %v", err)
	}
	// keybbbb 已冷却，首次调用必然选 keyaaaa → 429 → 重读池快照后
	// keyaaaa 也已冷却、keybbbb 仍在冷却 → 无 key 可换，透传 429 结果
	res, err := f.svc.Execute(context.Background(), PathSearch,
		mustValues("libraryName", "retry", "query", "docs"))
	if err == nil {
		t.Fatalf("want 429 error, got %+v", res)
	}
	if res.Status != 429 || res.KeyID != 1 {
		t.Fatalf("res = %+v, want 429 from key 1 only", res)
	}
	// 全过程只允许打到 keyaaaa（含其 429），冷却中的 keybbbb 一次都不许被打
	keys, _ := f.st.ListKeys()
	for _, k := range keys {
		if k.ID == 2 && k.RequestCount != 0 {
			t.Fatalf("cooling key was hit %d times during retry", k.RequestCount)
		}
	}
	// 第二次同参调用：两 key 均冷却，必须返回 ErrAllCooling（而非再打上游）
	if _, err := f.svc.Execute(context.Background(), PathSearch,
		mustValues("libraryName", "retry", "query", "docs")); !errors.Is(err, ErrAllCooling) {
		t.Fatalf("second call err = %v, want ErrAllCooling", err)
	}
}

// 项 3 补充：重读快照后剩余可用 key 正常工作——keyaaaa 429、keybbbb 冷却、
// keycccc 可用，重试必须落到 keycccc 而不是失败。
func Test429RetryPicksFreshKeyAfterSnapshot(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), "keyaaaa") {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa", "ctx7sk-keybbbb", "ctx7sk-keycccc")
	if err := f.st.CooldownKey(2, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("CooldownKey: %v", err)
	}

	// keyaaaa 与 keycccc 交替轮询；多次调用保证覆盖“先 429 再重试”路径
	saw200 := false
	for i := 0; i < 4; i++ {
		res, err := f.svc.Execute(context.Background(), PathSearch,
			mustValues("libraryName", fmt.Sprintf("pick%d", i), "query", "docs"))
		if err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
		if res.Status == 200 && res.KeyID == 3 {
			saw200 = true
		}
	}
	if !saw200 {
		t.Fatal("retry must be able to land on fresh key 3 after snapshot reread")
	}
}

// 项 4：固定 3 key 池连续调用，轮询分布确定——每 key 次数差不超过 1。
func TestRoundRobinPerServiceInstance(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa", "ctx7sk-keybbbb", "ctx7sk-keycccc")

	counts := map[int64]int{}
	for i := 0; i < 6; i++ {
		res, err := f.svc.Execute(context.Background(), PathSearch,
			mustValues("libraryName", fmt.Sprintf("rr%d", i), "query", "docs"))
		if err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
		counts[res.KeyID]++
	}
	if len(counts) != 3 {
		t.Fatalf("distinct keys used = %d, want 3", len(counts))
	}
	for id, n := range counts {
		if n != 2 {
			t.Fatalf("key %d picked %d times, want exactly 2 (round-robin must be even)", id, n)
		}
	}
}

// 项 4：两个 Service 实例轮询计数互不干扰（包级全局已移除的结构性验证）。
func TestRoundRobinIsolatedBetweenInstances(t *testing.T) {
	mk := func() (*Service, *[]int64) {
		st, err := store.Open(t.TempDir() + "/iso.db")
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		for _, v := range []string{"ctx7sk-iso-aaaa111", "ctx7sk-iso-bbbb222"} {
			if _, err := st.AddKey(v); err != nil {
				t.Fatalf("AddKey: %v", err)
			}
		}
		var picked []int64
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(up.Close)
		svc := New(st, st, up.URL, nil, time.Hour)
		return svc, &picked
	}
	// 两实例各自从零起轮：若仍共享包级计数，两实例的首选 key 会错开；
	// 实例化后各自独立，两实例第一次都必须选到同一个（池内序首个）位置
	svcA, pickedA := mk()
	svcB, pickedB := mk()
	for i := 0; i < 2; i++ {
		for _, tc := range []struct {
			svc    *Service
			picked *[]int64
		}{{svcA, pickedA}, {svcB, pickedB}} {
			res, err := tc.svc.Execute(context.Background(), PathSearch,
				mustValues("libraryName", fmt.Sprintf("iso%d", i), "query", "docs"))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			*tc.picked = append(*tc.picked, res.KeyID)
		}
	}
	if (*pickedA)[0] != (*pickedB)[0] {
		t.Fatalf("first picks differ between fresh instances: %d vs %d — state leaked across instances",
			(*pickedA)[0], (*pickedB)[0])
	}
}

// 项 5：取消场景下错误链双成立——ErrUpstreamUnreachable 与 context.Canceled
// 必须同时可被 errors.Is 判定。
func TestCancelErrorChainPreserved(t *testing.T) {
	arrived := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := f.svc.Execute(ctx, PathSearch, mustValues("libraryName", "chain", "query", "docs"))
		errCh <- err
	}()
	<-arrived
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrUpstreamUnreachable) {
			t.Fatalf("errors.Is(err, ErrUpstreamUnreachable) = false, err = %v", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("errors.Is(err, context.Canceled) = false, err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return after cancel")
	}
}

// 项 5：超时场景下 DeadlineExceeded 同样保留在错误链中。
func TestDeadlineErrorChainPreserved(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}, time.Hour)
	f.addKeys(t, "ctx7sk-keyaaaa")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := f.svc.Execute(ctx, PathSearch, mustValues("libraryName", "dl", "query", "docs"))
	if !errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("errors.Is(err, ErrUpstreamUnreachable) = false, err = %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false, err = %v", err)
	}
}
