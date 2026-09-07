// Service 装配与执行入口。
package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"context7-proxy/internal/store"
	"golang.org/x/sync/singleflight"
)

// 外呼与调度约束具名常量。
const (
	// 上游单请求默认超时：文档查询是秒级操作，给足余量但不允许无限挂起。
	// 用 per-request context.WithTimeout 实现而非 client.Timeout，
	// 这样注入的 client 无法绕过超时策略。
	upstreamTimeout = 30 * time.Second
	// 响应体上限：文档查询响应有限，防止异常上游导致无界读取
	maxResponseBodyBytes = 10 << 20 // 10 MiB
	// 上游 429 且未提供 RateLimit-Reset/Retry-After 时的兜底冷却时长
	fallbackCooldown = 5 * time.Minute
	// 429 换 key 重试上限：只重试一次
	maxRetry429 = 1
)

// HTTP 状态常量集中定义，避免魔法数字散落。
const (
	statusOK  = 200
	status429 = 429
)

var (
	// ErrUpstreamUnreachable 网络层失败（DNS/连接/超时/取消），入口层映射为 502
	ErrUpstreamUnreachable = errors.New("upstream unreachable")
	// ErrResponseTooLarge 上游响应体超过 maxResponseBodyBytes 或声明了超限
	// Content-Length：绝不截断入缓存，宁可整条失败
	ErrResponseTooLarge = errors.New("upstream response exceeds size limit")
)

// Result 是入口层（HTTP handler / MCP tool）消费的统一响应契约。
// 不透传任何上游响应头（额度/Cookie/Server 等一律不出现在 Result）：
// 代理对客户端的响应头由入口层按 Result 字段自行生成，429 的 Retry-After
// 由代理按冷却截止换算（RetryAfterSec），杜绝上游头直通客户端。
// 原始 key 永不出现在 Result 中。
type Result struct {
	Status      int
	ContentType string
	Body        []byte
	KeyID       int64
	Cached      bool
	// RetryAfterSec 仅 429 时填入建议客户端等待秒数（来自上游头或 fallback）
	RetryAfterSec int64
}

// Service 是查询服务，并发安全；轮询计数归属实例而非包级全局，
// 多实例（或测试）之间互不干扰。
type Service struct {
	pool     PoolStore
	cache    CacheStore
	upstream string
	client   *http.Client // 固定策略：拒绝跟随重定向，超时由 per-request ctx 保证
	ttl      time.Duration
	now      func() time.Time // 注入时钟：测试精确控制冷却到期，不靠 sleep
	// robin 轮询计数：跨请求保持轮转位置，原子递增打散上游负载
	robin atomic.Int64
	// sf 合并同 cache key 的并发回源：防缓存击穿（同一 miss 只回源一次）
	sf singleflight.Group
}

// PoolStore 是 key 池依赖面（生产传 *store.Store）。
type PoolStore interface {
	ListPoolKeys() ([]store.PoolKey, error)
	MarkKeyAttempt(id int64) error
	CooldownKey(id int64, until int64) error
	ObserveKeyQuota(id int64, remaining, limit *int64) error
}

// CacheStore 是缓存依赖面；hash 由本包计算（cacheKey）。
type CacheStore interface {
	GetCache(hash string) (store.CacheEntry, error)
	SetCache(hash string, body []byte, contentType string, ttl time.Duration) error
}

// New 构造服务。upstream 形如 https://context7.com；rt 传 nil 用 http.DefaultTransport。
// rt 只替换传输实现（连接池/代理等），重定向策略与超时始终由 Service 固定持有，
// 注入方无法通过自定义 http.Client 绕过安全策略。
func New(pool PoolStore, cache CacheStore, upstream string, rt http.RoundTripper, ttl time.Duration) *Service {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &Service{
		pool: pool, cache: cache, upstream: upstream, ttl: ttl,
		client: &http.Client{
			Transport: rt,
			// 上游 host 固定，任何 3xx 都按错误响应原样返回，绝不跟随：
			// 跟随会把 Bearer key 发给重定向目标，是 key 泄漏面
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
}

// CacheTTL 暴露共享缓存 TTL：MCP 入口的 tools/call 缓存与 REST 查询缓存
// 必须共用同一配置值，暴露访问器而非让入口层再持有一份，杜绝口径漂移。
func (s *Service) CacheTTL() time.Duration { return s.ttl }

// Execute 处理一次公共文档查询。
// 校验 → 缓存命中直接回（不选 key 不耗额度）→ singleflight 按 cache key 合并
// 并发回源 → 选 key 调上游 → 429 冷却该 key 并（GET 幂等）重读池快照换 key
// 重试至多一次 → 200 且允许时写共享缓存。
func (s *Service) Execute(ctx context.Context, path string, q url.Values) (Result, error) {
	encoded, err := validateParams(path, q)
	if err != nil {
		return Result{}, err
	}

	ck := cacheKey(path, encoded)
	if entry, err := s.cache.GetCache(ck); err == nil {
		// 缓存条目只含 body/content-type，结构上不存在历史额度头可重放
		return Result{Status: statusOK, ContentType: entry.ContentType,
			Body: entry.Body, Cached: true}, nil
	} else if !errors.Is(err, store.ErrCacheMiss) {
		return Result{}, fmt.Errorf("cache read: %w", err) // store 错误不吞
	}

	// 防缓存击穿：同一 cache key 并发 miss 只由首个请求真正回源，等待者共享
	// 同一 Result。取舍：singleflight.Do 等待者不受自身 ctx 取消保护、也不中断
	// 执行者（执行者 ctx 独立）；若执行者失败（含其 ctx 取消），所有等待者拿到
	// 同一错误——宁可共同失败也不并发烧穿 key 池额度。
	// 429 是软错误：err 与 Result 同时非零（Result 携带状态/KeyID/RetryAfter），
	// 等待者同样共享该 Result，与执行者的透传语义完全一致。
	v, err, _ := s.sf.Do(ck, func() (any, error) {
		return s.fetch(ctx, path, encoded)
	})
	if err != nil {
		if res, ok := v.(Result); ok {
			return res, err
		}
		return Result{}, err
	}
	return v.(Result), nil
}

// fetch 从 key 池选 key 调上游，429 时冷却并重读池状态换 key 重试至多一次。
// 重试不沿用调用开始的候选快照：其他请求可能在此期间冷却了 key，
// 必须以重读到的实时池状态为准，避免击打冷却中的 key。
func (s *Service) fetch(ctx context.Context, path, encoded string) (Result, error) {
	k, err := s.pickKey()
	if err != nil {
		return Result{}, err
	}
	for attempt := 0; ; attempt++ {
		res, callErr := s.callUpstream(ctx, path, encoded, k)
		if callErr == nil || res.Status != status429 {
			return res, callErr
		}
		if attempt >= maxRetry429 {
			return res, callErr
		}
		next, err := s.pickKeyExcluding(k.ID)
		if err != nil {
			return res, callErr // 无 key 可换，透传 429
		}
		k = next
	}
}

// pickKey 读取当前池状态并轮询选一个可用 key。
func (s *Service) pickKey() (store.PoolKey, error) {
	candidates, err := s.availableCandidates()
	if err != nil {
		return store.PoolKey{}, err
	}
	return s.pickRoundRobin(candidates), nil
}

// pickKeyExcluding 重读池状态（过滤冷却）后排除已试 key 再选一个。
func (s *Service) pickKeyExcluding(excludeID int64) (store.PoolKey, error) {
	candidates, err := s.availableCandidates()
	if err != nil {
		return store.PoolKey{}, err
	}
	for {
		k := s.pickRoundRobin(candidates)
		if k.ID != excludeID {
			return k, nil
		}
		// 候选只剩被排除者时返回错误，交由调用方透传 429
		if len(candidates) == 1 {
			return store.PoolKey{}, ErrNoAvailableKey
		}
	}
}

// ErrAllCoolingInfo 携带全冷却场景给入口层的信息：最早到期的冷却截止
// （Unix 秒），入口据此换算 Retry-After，让客户端等到真有 key 恢复。
// 包装哨兵而非改哨兵：errors.Is(err, ErrAllCooling) 对新旧调用方都成立。
type ErrAllCoolingInfo struct {
	// EarliestReset 最早冷却到期 Unix 秒；0 表示未知
	EarliestReset int64
}

func (e *ErrAllCoolingInfo) Error() string { return ErrAllCooling.Error() }

func (e *ErrAllCoolingInfo) Unwrap() error { return ErrAllCooling }

// availableCandidates 列出启用且不在冷却中的 key，按当前时刻判定。
func (s *Service) availableCandidates() ([]availableKey, error) {
	keys, err := s.pool.ListPoolKeys()
	if err != nil {
		return nil, fmt.Errorf("list pool keys: %w", err)
	}
	candidates := filterAvailable(keys, s.now().Unix())
	if len(candidates) == 0 {
		if len(keys) > 0 {
			// 全冷却时扫出最早到期时刻，入口层可换算 Retry-After
			var earliest int64
			for _, k := range keys {
				if k.CooldownUntil != nil && (earliest == 0 || *k.CooldownUntil < earliest) {
					earliest = *k.CooldownUntil
				}
			}
			return nil, &ErrAllCoolingInfo{EarliestReset: earliest}
		}
		return nil, ErrNoAvailableKey
	}
	return candidates, nil
}

// callUpstream 用指定 key 发一次上游调用：计数尝试、观察额度、按 429 冷却、
// 200 且可缓存时写缓存。
func (s *Service) callUpstream(ctx context.Context, path, encodedQuery string, k store.PoolKey) (Result, error) {
	if err := s.pool.MarkKeyAttempt(k.ID); err != nil {
		return Result{}, fmt.Errorf("mark key attempt: %w", err)
	}

	// 超时绑定在本次外呼的 ctx 上：与注入的 Transport 无关，不可绕过
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		upstreamURL(s.upstream, path, encodedQuery), nil)
	if err != nil {
		return Result{}, fmt.Errorf("build upstream request: %w", err)
	}
	// 新建请求而非 clone 客户端请求：客户端的 Cookie/Authorization/Forwarded
	// 从构造上就不存在，杜绝任何认证信息透传给上游
	req.Header.Set("Authorization", "Bearer "+k.Value)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		// 用 %w wrap 保留 ctx 错误链：errors.Is(err, context.Canceled) 与
		// errors.Is(err, ErrUpstreamUnreachable) 必须同时成立，入口层据此
		// 区分客户端主动取消与真实网络故障。ctx 取消时 doErr 链上已含
		// context.Canceled/DeadlineExceeded，直接 wrap 即可；非取消的网络
		// 错误 wrap 后不匹配 errors.Is(ctx 错误)，同样成立。
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrUpstreamUnreachable, ctxErr)
		}
		return Result{}, fmt.Errorf("%w: %w", ErrUpstreamUnreachable, err)
	}
	defer resp.Body.Close()

	body, err := readBodyLimited(resp)
	if err != nil {
		return Result{}, err // 超限/读失败已带语义，绝不让截断数据流向缓存
	}

	if err := s.observeQuota(k.ID, resp.Header); err != nil {
		return Result{}, fmt.Errorf("observe quota: %w", err)
	}

	if resp.StatusCode == status429 {
		wait, err := s.cooldownFor429(k.ID, resp.Header)
		if err != nil {
			// 冷却落盘失败是 store 故障：直接失败整次 Execute，
			// 不换 key 重试、不当作普通 429 透传——否则该 key 会继续被调度
			return Result{}, err
		}
		return Result{Status: status429, Body: body, ContentType: resp.Header.Get("Content-Type"),
				KeyID: k.ID, RetryAfterSec: wait},
			fmt.Errorf("upstream 429, key %d cooling", k.ID)
	}

	result := Result{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"),
		Body: body, KeyID: k.ID}
	// 非 429 错误（401/403/5xx）原样透传给入口层，绝不据此永久禁用 key：
	// 单次鉴权失败与池级配置问题语义不同，永久禁用需人工在管理端操作
	if resp.StatusCode == statusOK && s.cacheable(path, resp.Header, body) {
		if err := s.cache.SetCache(cacheKey(path, encodedQuery), body, result.ContentType, s.ttl); err != nil {
			return Result{}, fmt.Errorf("cache write: %w", err)
		}
	}
	return result, nil
}

// readBodyLimited 读取响应体并保证不静默截断：
// 声明超限 Content-Length 直接拒绝；否则读 max+1 字节，读满说明实际超限。
func readBodyLimited(resp *http.Response) ([]byte, error) {
	if resp.ContentLength > maxResponseBodyBytes {
		return nil, fmt.Errorf("%w: content-length %d", ErrResponseTooLarge, resp.ContentLength)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrUpstreamUnreachable, err)
	}
	if len(body) > maxResponseBodyBytes {
		return nil, fmt.Errorf("%w: body %d bytes", ErrResponseTooLarge, len(body))
	}
	return body, nil
}

// observeQuota 解析可选 RateLimit-* 头并持久化；任一头缺失则传 nil，
// store 端 COALESCE 保留旧值——上游不保证每次都带头，缺失不等于额度清零。
func (s *Service) observeQuota(keyID int64, h http.Header) error {
	var remaining, limit *int64
	if v, ok := atoiNonNegative(h.Get("RateLimit-Remaining")); ok {
		remaining = &v
	}
	if v, ok := atoiNonNegative(h.Get("RateLimit-Limit")); ok {
		limit = &v
	}
	return s.pool.ObserveKeyQuota(keyID, remaining, limit)
}

// cooldownFor429 计算并落盘冷却截止时刻，返回建议等待秒数。
// 优先级：RateLimit-Reset（未来 Unix 时间戳）> Retry-After（秒数或 HTTP 日期）> fallback。
// 落盘失败返回错误——冷却状态是调度正确性的前提，失败必须上抛。
func (s *Service) cooldownFor429(keyID int64, h http.Header) (int64, error) {
	now := s.now()
	var until time.Time
	if v, ok := atoiNonNegative(h.Get("RateLimit-Reset")); ok && v > now.Unix() {
		until = time.Unix(v, 0)
	} else if t, ok := parseRetryAfter(h.Get("Retry-After"), now); ok {
		until = t
	}
	if until.IsZero() {
		until = now.Add(fallbackCooldown)
	}
	if err := s.pool.CooldownKey(keyID, until.Unix()); err != nil {
		return 0, fmt.Errorf("persist cooldown for key %d: %w", keyID, err)
	}
	return int64(until.Sub(now).Seconds()), nil
}

// parseRetryAfter 解析 Retry-After 的两种合法形态：秒数或 HTTP 日期。
func parseRetryAfter(v string, now time.Time) (time.Time, bool) {
	if secs, ok := atoiNonNegative(v); ok {
		return now.Add(time.Duration(secs) * time.Second), true
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t, true
	}
	return time.Time{}, false
}

// cacheable 判定 200 响应是否可入共享缓存。
// Cache-Control 遍历全部头的全部值，按指令名（= 前部分）判断：
// no-store / private / max-age=0 任一命中即不可缓存——private="Set-Cookie"
// 这类带参数的写法指令名仍是 private，必须被识别。
func (s *Service) cacheable(path string, h http.Header, body []byte) bool {
	for _, cc := range h.Values("Cache-Control") {
		for _, dir := range strings.Split(cc, ",") {
			name := strings.TrimSpace(strings.ToLower(strings.SplitN(dir, "=", 2)[0]))
			switch name {
			case "no-store", "private":
				return false
			case "max-age":
				if v := strings.Trim(strings.TrimSpace(strings.SplitN(dir, "=", 2)[1]), `"`); v == "0" {
					return false
				}
			}
		}
	}
	// searchFilterApplied=true 的响应是按过滤词裁剪的个性化子集而非公共全集，
	// 共享会造成错配；仅 search 端点有此语义
	if path == PathSearch && searchFilterApplied(body) {
		return false
	}
	return true
}

// searchFilterApplied 用结构化 JSON 解析识别标记，杜绝手写字节扫描漏掉
// 换行/缩进/字段顺序等格式变体；解析失败保守不缓存（宁少勿错）。
func searchFilterApplied(body []byte) bool {
	var v struct {
		SearchFilterApplied bool `json:"searchFilterApplied"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return true // 非 JSON/解析失败：无法证明可共享，保守视为不可缓存
	}
	return v.SearchFilterApplied
}

// atoiNonNegative 解析非负整数（允许 0，覆盖 remaining=0 耗尽场景），失败返回 false。
func atoiNonNegative(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n >= 0
}
