// key 池调度视图与运行时状态更新：供 query 服务做候选选择与额度观察。
package store

import (
	"database/sql"
	"time"
)

// PoolKey 是调度候选的内部视图，含 key 原文。
// Value 必须带 json:"-"：仅靠"约定不导出"无法阻止意外序列化，tag 是硬防线。
type PoolKey struct {
	ID            int64
	Value         string `json:"-"`
	Remaining     *int64
	CooldownUntil *int64
}

// ListPoolKeys 返回全部启用 key（按 ID 升序）。冷却是否生效是时间函数，
// 由调用方按读取时刻统一判断，过期冷却自然恢复，无需写端补偿。
func (s *Store) ListPoolKeys() ([]PoolKey, error) {
	rows, err := s.db.Query(`SELECT id, value, remaining, cooldown_until
		FROM keys WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PoolKey
	for rows.Next() {
		var (
			k          PoolKey
			remaining  sql.NullInt64
			cooldownAt sql.NullInt64
		)
		if err := rows.Scan(&k.ID, &k.Value, &remaining, &cooldownAt); err != nil {
			return nil, err
		}
		k.Remaining = nullInt64(remaining)
		k.CooldownUntil = nullInt64(cooldownAt)
		out = append(out, k)
	}
	return out, rows.Err()
}

// MarkKeyAttempt 记一次真实上游尝试（成败皆计）并刷新 lastUsedAt；
// 入口层的请求统计只记一次，这里的计数只反映上游尝试次数。
// 目标 key 不存在（并发删除）返回 ErrKeyNotFound，不静默零行。
func (s *Store) MarkKeyAttempt(id int64) error {
	return s.updateKeyRow(`UPDATE keys SET request_count = request_count + 1, last_used_at = ?
		WHERE id = ?`, time.Now().Unix(), id)
}

// CooldownKey 将 key 冷却至指定 Unix 时刻。
func (s *Store) CooldownKey(id int64, until int64) error {
	return s.updateKeyRow(`UPDATE keys SET cooldown_until = ? WHERE id = ?`, until, id)
}

// ObserveKeyQuota 更新观察到的额度；任一字段为 nil 表示本次响应未提供该信息，
// 保留旧值不清除——上游不保证每次都带 RateLimit-*，缺失不等于清零。
func (s *Store) ObserveKeyQuota(id int64, remaining, limit *int64) error {
	return s.updateKeyRow(`UPDATE keys SET
		remaining   = COALESCE(?, remaining),
		rate_limit  = COALESCE(?, rate_limit)
		WHERE id = ?`, remaining, limit, id)
}

// updateKeyRow 执行单行 UPDATE 并校验 RowsAffected：0 行说明 key 已被删除，
// 返回 ErrKeyNotFound 让调用方感知，而不是无声成功。
func (s *Store) updateKeyRow(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrKeyNotFound
	}
	return nil
}
