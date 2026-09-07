// 响应缓存：hash 由调用层（REST 代理）计算并去抖动，存储层只负责按 TTL 存取。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CacheEntry 是一次命中的缓存响应；Body 为上游响应体原文。
type CacheEntry struct {
	Body        []byte
	ContentType string
}

// 缓存过期口径说明：三处时间取值统一为“该次操作开始时的 Unix 秒”：
//   - SetCache：写入时刻 + TTL；
//   - GetCache：读取时刻判定并删除，判定与删除用同一 now（DELETE 带
//     expires_at 条件，防止并发覆盖的新条目被旧判断误删）；
//   - CleanExpiredCache：清理时刻。
// 同一请求内 GetCache 只取一次 now 做判定与删除，二者不会互相矛盾；
// 秒级粒度意味着条目最多多存活不足 1 秒，对分钟级 TTL 无实际影响。

// ErrCacheMiss 表示 hash 无对应条目或条目已过期，调用方据此走上游回源。
var ErrCacheMiss = errors.New("cache miss")

// SetCache 写入缓存条目，hash 相同则覆盖（同 key 换新响应时以最新为准）。
func (s *Store) SetCache(hash string, body []byte, contentType string, ttl time.Duration) error {
	expires := time.Now().Add(ttl).Unix()
	_, err := s.db.Exec(`INSERT INTO cache (hash, body, content_type, expires_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(hash) DO UPDATE SET
		body = excluded.body, content_type = excluded.content_type, expires_at = excluded.expires_at`,
		hash, body, contentType, expires)
	return err
}

// GetCache 读取未过期条目；缺失或已过期返回 ErrCacheMiss。
// 过期判断与删除用同一 now，DELETE 带 expires_at 条件：即便并发请求已先
// 用新 TTL 覆盖了该条目，也不会被本请求的旧判断误删。删除错误不吞。
func (s *Store) GetCache(hash string) (CacheEntry, error) {
	row := s.db.QueryRow(`SELECT body, content_type, expires_at FROM cache WHERE hash = ?`, hash)
	var (
		e         CacheEntry
		expiresAt int64
	)
	if err := row.Scan(&e.Body, &e.ContentType, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CacheEntry{}, ErrCacheMiss
		}
		return CacheEntry{}, err
	}
	now := time.Now().Unix()
	if expiresAt <= now {
		if _, err := s.db.Exec(`DELETE FROM cache WHERE hash = ? AND expires_at <= ?`, hash, now); err != nil {
			return CacheEntry{}, fmt.Errorf("delete expired cache: %w", err)
		}
		return CacheEntry{}, ErrCacheMiss
	}
	return e, nil
}

// CleanExpiredCache 删除全部过期条目；启动时调用，平时靠 GetCache 惰性删除。
func (s *Store) CleanExpiredCache() error {
	_, err := s.db.Exec(`DELETE FROM cache WHERE expires_at <= ?`, time.Now().Unix())
	return err
}
