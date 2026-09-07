package store

import (
	"strings"
	"testing"
	"time"
)

// newTestStore 每个用例独立临时库文件，保证用例间无残留状态。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestKeyCRUD(t *testing.T) {
	s := newTestStore(t)

	// 用真实形态长度的 key，保证明文回显断言贴近生产输入
	const raw = "ctx7sk-0123456789abcdef1234"
	added, err := s.AddKey(raw)
	if err != nil || added.ID == 0 {
		t.Fatalf("AddKey = (%v, %v), want non-zero id", added, err)
	}

	// 重复 key 必须可判断
	if _, err := s.AddKey(raw); err != ErrDuplicateKey {
		t.Fatalf("AddKey duplicate err = %v, want ErrDuplicateKey", err)
	}

	// 格式信任边界：短值/错前缀/空串一律拒绝入库
	for _, bad := range []string{"sk-short", "ctx7sk", "", "ctx7sk-12"} {
		if _, err := s.AddKey(bad); err != ErrInvalidKeyFormat {
			t.Errorf("AddKey(%q) err = %v, want ErrInvalidKeyFormat", bad, err)
		}
	}

	k, err := s.GetKeyByID(added.ID)
	if err != nil || k.Value != raw {
		t.Fatalf("GetKeyByID = (%v, %v)", k, err)
	}

	if _, err := s.GetKeyByID(9999); err != ErrKeyNotFound {
		t.Fatalf("GetKeyByID missing err = %v, want ErrKeyNotFound", err)
	}

	keys, err := s.ListKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys = (%v, %v)", keys, err)
	}
	ks := keys[0]
	if ks.Value != raw {
		t.Fatalf("value = %q, want full plaintext key (self-hosted admin view)", ks.Value)
	}
	// 新 key 从未测试过：额度字段必须为 null 而非 0（0 是有效余量，语义不同）
	if ks.Status != "untested" || ks.Remaining != nil || ks.Limit != nil ||
		ks.CooldownUntil != nil || ks.LastUsedAt != nil {
		t.Fatalf("new key status = %+v, want untested with nil quota fields", ks)
	}

	if err := s.SetKeyEnabled(added.ID, false); err != nil {
		t.Fatalf("SetKeyEnabled: %v", err)
	}
	keys, _ = s.ListKeys()
	if keys[0].Enabled || keys[0].Status != "disabled" {
		t.Fatalf("after disable = %+v, want enabled=false status=disabled", keys[0])
	}

	if err := s.SetKeyEnabled(9999, true); err != ErrKeyNotFound {
		t.Fatalf("SetKeyEnabled missing err = %v, want ErrKeyNotFound", err)
	}
	if err := s.DeleteKey(9999); err != ErrKeyNotFound {
		t.Fatalf("DeleteKey missing err = %v, want ErrKeyNotFound", err)
	}
	if err := s.DeleteKey(added.ID); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
}

func TestCacheExpiry(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetCache("h1", []byte("body"), "application/json", time.Minute); err != nil {
		t.Fatalf("SetCache: %v", err)
	}
	e, err := s.GetCache("h1")
	if err != nil || string(e.Body) != "body" || e.ContentType != "application/json" {
		t.Fatalf("GetCache = (%v, %v)", e, err)
	}

	if _, err := s.GetCache("missing"); err != ErrCacheMiss {
		t.Fatalf("GetCache missing err = %v, want ErrCacheMiss", err)
	}

	// 写入已过期条目，读取必须判 miss 且顺带删除
	if err := s.SetCache("h2", []byte("old"), "application/json", -time.Second); err != nil {
		t.Fatalf("SetCache expired: %v", err)
	}
	if _, err := s.GetCache("h2"); err != ErrCacheMiss {
		t.Fatalf("expired GetCache err = %v, want ErrCacheMiss", err)
	}

	if err := s.SetCache("h3", []byte("x"), "text/plain", time.Minute); err != nil {
		t.Fatalf("SetCache h3: %v", err)
	}
	if err := s.CleanExpiredCache(); err != nil {
		t.Fatalf("CleanExpiredCache: %v", err)
	}
	if _, err := s.GetCache("h3"); err != nil {
		t.Fatalf("h3 should survive cleanup: %v", err)
	}
}

func TestRequestLogRingAndStats(t *testing.T) {
	s := newTestStore(t)

	// 写 600 条（超过环形上限 500），一半 cached
	for i := 0; i < 600; i++ {
		err := s.RecordRequest(RequestRecord{
			Time: time.Now().Unix(), Method: "GET", Path: "/api/v2/context",
			Status: 200, Cached: i%2 == 0, DurationMs: 10,
		})
		if err != nil {
			t.Fatalf("RecordRequest %d: %v", i, err)
		}
	}

	recs, err := s.RecentRequests()
	if err != nil || len(recs) != maxRequestLog {
		t.Fatalf("RecentRequests len = %d, want %d (err %v)", len(recs), maxRequestLog, err)
	}

	st, err := s.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	// 累计值不能从 500 条日志推算，必须仍是全量 600
	if st.TotalRequests != 600 || st.CacheHits != 300 || st.RequestsToday != 600 {
		t.Fatalf("stats = %+v, want total 600 / hits 300 / today 600", st)
	}

	// 跨 UTC 日界后 requestsToday 重新起算，历史累计不受影响
	if err := insertFakeDay(s, "2000-01-01", 5); err != nil {
		t.Fatalf("insertFakeDay: %v", err)
	}
	st, _ = s.GetStats()
	if st.RequestsToday != 600 {
		t.Fatalf("today after fake history = %d, want 600", st.RequestsToday)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/reopen.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	added, err := s.AddKey("ctx7sk-persist-9999")
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if err := s.RecordRequest(RequestRecord{Time: time.Now().Unix(), Method: "GET",
		Path: "/p", Status: 200, KeyID: added.ID, KeyMasked: "m", DurationMs: 1}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	k, err := s2.GetKeyByID(added.ID)
	if err != nil || k.Value != "ctx7sk-persist-9999" {
		t.Fatalf("key after reopen = (%v, %v)", k, err)
	}
	recs, err := s2.RecentRequests()
	if err != nil || len(recs) != 1 {
		t.Fatalf("log after reopen len = %d (err %v)", len(recs), err)
	}
	st, err := s2.GetStats()
	if err != nil || st.TotalRequests != 1 {
		t.Fatalf("stats after reopen = (%+v, %v)", st, err)
	}
}

// insertFakeDay 直接写入历史日期计数，模拟“昨天有请求”验证今日口径独立。
func insertFakeDay(s *Store, day string, n int) error {
	for i := 0; i < n; i++ {
		if _, err := s.db.Exec(`INSERT INTO stats_daily (day, requests) VALUES (?, 1)
			ON CONFLICT(day) DO UPDATE SET requests = requests + 1`, day); err != nil {
			return err
		}
	}
	return nil
}

// 短值脱敏安全：长度低于安全线的任何输入，masked 只能是 "***"，
// 不得含原文任何子串（短值前8+后4 可重组全文，是泄漏路径）。
func TestMaskedKeyShortValues(t *testing.T) {
	for _, v := range []string{"x", "12345678", "123456789012"} {
		m := MaskedKey(v)
		if m != "***" {
			t.Errorf("MaskedKey(%q) = %q, want ***", v, m)
		}
		if strings.Contains(m, v) || (len(v) > 4 && strings.Contains(m, v[:4])) {
			t.Errorf("MaskedKey(%q) = %q leaks original material", v, m)
		}
	}
	// 合法长度 key 正常脱敏：前 8 字符 + *** + 后 4 字符
	if m := MaskedKey("ctx7sk-abcdefgh99"); m != "ctx7sk-a***gh99" {
		t.Errorf("long key masked = %q, want ctx7sk-a***gh99", m)
	}
}

// 全新库 GetStats 必须返回零值而非 ErrNoRows：无请求的零统计是合法状态。
func TestGetStatsFreshDB(t *testing.T) {
	s := newTestStore(t)
	st, err := s.GetStats()
	if err != nil {
		t.Fatalf("GetStats on fresh db: %v", err)
	}
	if st != (Stats{}) {
		t.Fatalf("fresh stats = %+v, want zero", st)
	}
}

// status 与 cooldownUntil 用同一 now 判定：冷却到期后两字段口径一致。
func TestCooldownExpiryConsistency(t *testing.T) {
	s := newTestStore(t)
	added, err := s.AddKey("ctx7sk-expiry-123456")
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	// 先让它"用过"以脱离 untested 态
	if err := s.MarkKeyAttempt(added.ID); err != nil {
		t.Fatalf("MarkKeyAttempt: %v", err)
	}
	// 冷却到过去：status 必须是 active 且 cooldownUntil 必须是 nil
	if err := s.CooldownKey(added.ID, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatalf("CooldownKey: %v", err)
	}
	keys, err := s.ListKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys = (%v, %v)", keys, err)
	}
	if keys[0].Status != "active" || keys[0].CooldownUntil != nil {
		t.Fatalf("expired cooldown = status %s until %v, want active + nil",
			keys[0].Status, keys[0].CooldownUntil)
	}
	// 冷却到未来：status=cooldown 且 cooldownUntil 非 nil
	if err := s.CooldownKey(added.ID, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("CooldownKey: %v", err)
	}
	keys, _ = s.ListKeys()
	if keys[0].Status != "cooldown" || keys[0].CooldownUntil == nil {
		t.Fatalf("active cooldown = status %s until %v", keys[0].Status, keys[0].CooldownUntil)
	}
}

// pool 侧 UPDATE 目标不存在时必须返回 ErrKeyNotFound 而非静默成功。
func TestPoolUpdateMissingKey(t *testing.T) {
	s := newTestStore(t)
	for name, fn := range map[string]func() error{
		"MarkKeyAttempt":  func() error { return s.MarkKeyAttempt(999) },
		"CooldownKey":     func() error { return s.CooldownKey(999, 1) },
		"ObserveKeyQuota": func() error { return s.ObserveKeyQuota(999, nil, nil) },
	} {
		if err := fn(); err != ErrKeyNotFound {
			t.Errorf("%s on missing key = %v, want ErrKeyNotFound", name, err)
		}
	}
}

// 日统计用 rec.Time 归日：昨日时刻的记录必须落在昨天的行，不进入今天。
func TestDailyStatsUsesRecordTime(t *testing.T) {
	s := newTestStore(t)
	yesterday := time.Now().UTC().Add(-24 * time.Hour)
	if err := s.RecordRequest(RequestRecord{Time: yesterday.Unix(), Method: "GET",
		Path: "/p", Status: 200, DurationMs: 1}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	st, err := s.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	// TotalRequests 计 1，但今天的 requestsToday 不含昨天的记录
	if st.TotalRequests != 1 || st.RequestsToday > 0 {
		t.Fatalf("stats = %+v, want total 1 / today 0", st)
	}
}
