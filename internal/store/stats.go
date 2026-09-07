// 请求日志（环形保留最近 500 条）与累计统计。
// 统计与日志分表存储：日志会裁剪，totalRequests/cacheHits/requestsToday 必须独立持久累计。
package store

import (
	"database/sql"
	"errors"
	"time"
)

// 请求日志环形上限：超过后删除最旧条目。
const maxRequestLog = 500

// RequestRecord 与 web/src/types.ts 的 RequestRecord 严格对齐。
type RequestRecord struct {
	Time       int64  `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	KeyID      int64  `json:"keyId"`
	KeyMasked  string `json:"keyMasked"`
	Cached     bool   `json:"cached"`
	DurationMs int64  `json:"durationMs"`
}

// RecordRequest 写入一条代理请求日志并裁剪环形窗口，同时累加全局与当日计数。
// cached=true 表示缓存命中（也计入总请求但不消耗 key）；本方法放在一个事务里，
// 保证日志行数与统计计数永远一致。
func (s *Store) RecordRequest(rec RequestRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO request_log
		(time, method, path, status, key_id, key_masked, cached, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Time, rec.Method, rec.Path, rec.Status, rec.KeyID, rec.KeyMasked,
		rec.Cached, rec.DurationMs); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM request_log WHERE id NOT IN
		(SELECT id FROM request_log ORDER BY id DESC LIMIT ?)`, maxRequestLog); err != nil {
		return err
	}
	// totalRequests 对缓存命中也累加：命中率 = cacheHits / totalRequests，
	// 若只计回源请求，缓存越好命中率反而趋近 1，失去监控意义
	if _, err := tx.Exec(`INSERT INTO stats_counters (id, total_requests, cache_hits)
		VALUES (1, 1, ?) ON CONFLICT(id) DO UPDATE SET
		total_requests = total_requests + 1,
		cache_hits = cache_hits + excluded.cache_hits`,
		boolToInt(rec.Cached)); err != nil {
		return err
	}
	// 当日按 UTC 计算：服务器部署环境时区不定，UTC 日界是唯一可复现口径；
	// 用 rec.Time 而非 time.Now()——入口传请求时刻，统计口径才不被落库延迟拉偏
	day := time.Unix(rec.Time, 0).UTC().Format("2006-01-02")
	if _, err := tx.Exec(`INSERT INTO stats_daily (day, requests) VALUES (?, 1)
		ON CONFLICT(day) DO UPDATE SET requests = requests + 1`, day); err != nil {
		return err
	}
	return tx.Commit()
}

// RecentRequests 返回最近的请求日志，新的在前，最多 500 条。
func (s *Store) RecentRequests() ([]RequestRecord, error) {
	rows, err := s.db.Query(`SELECT time, method, path, status, key_id, key_masked, cached, duration_ms
		FROM request_log ORDER BY id DESC LIMIT ?`, maxRequestLog)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestRecord
	for rows.Next() {
		var r RequestRecord
		var cached int
		if err := rows.Scan(&r.Time, &r.Method, &r.Path, &r.Status, &r.KeyID,
			&r.KeyMasked, &cached, &r.DurationMs); err != nil {
			return nil, err
		}
		r.Cached = cached != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// Stats 是管理端 overview 的统计部分；cacheHitRate 由调用方计算。
type Stats struct {
	TotalRequests int64
	CacheHits     int64
	RequestsToday int64
}

// GetStats 返回累计统计；requestsToday 为今天（UTC）的请求数。
// 全新库（stats_counters 已在 schema 初始化固定 id=1 行）与无请求的今天
// 都返回零值而非错误：无请求的零统计是合法状态。
func (s *Store) GetStats() (Stats, error) {
	var st Stats
	if err := s.db.QueryRow(`SELECT total_requests, cache_hits FROM stats_counters WHERE id = 1`).
		Scan(&st.TotalRequests, &st.CacheHits); err != nil {
		return Stats{}, err
	}
	day := time.Now().UTC().Format("2006-01-02")
	// 当天无行 = 0 请求；sql.ErrNoRows 折算为零值，不作为错误上抛
	if err := s.db.QueryRow(`SELECT requests FROM stats_daily WHERE day = ?`, day).
		Scan(&st.RequestsToday); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Stats{}, err
	}
	return st, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
