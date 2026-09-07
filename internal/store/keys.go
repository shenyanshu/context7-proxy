// key 池的 CRUD 与额度状态持久化。KeyStatus 自用场景输出明文（用户确认），
// 请求日志（RequestRecord.keyMasked）仍走脱敏。
package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// SQLITE_CONSTRAINT_UNIQUE：驱动未导出该常量别名，2067 是 lib 层的稳定值，
// 用具名常量固化，避免魔法数字。
const sqliteConstraintUnique = 2067

// 信任边界校验：Context7 key 固定 ctx7sk- 前缀，最短合法长度 13（前缀 7 + 至少 1 位密文）。
// 格式约束只做边界防御（拒 typo/垃圾输入），不做安全保证。
const (
	keyPrefix    = "ctx7sk"
	minKeyLength = len(keyPrefix) + 7 // 前缀 + 连字符 + 至少 1 位密文，脱敏拼接安全线
)

// MaskedKey 生成脱敏展示：前 8 字符 + *** + 后 4 字符，与前端 keyMasked 展示一致。
// 长度低于安全线的值（不该出现，防御历史脏数据）一律只返回 "***"，
// 任何情况下不从短值拼接原文片段——短值前 8 + 后 4 可重组出全文。
func MaskedKey(value string) string {
	if len(value) < minKeyLength {
		return "***"
	}
	return value[:8] + "***" + value[len(value)-4:]
}

// KeyStatus 是管理 API 对外模型，字段与 web/src/types.ts 的 KeyStatus 严格对齐：
// remaining/limit/cooldownUntil/lastUsedAt 未知时为 nil。
// Value 输出完整 key 原文：用户确认的自用部署场景，管理界面需要明文核对；
// RequestRecord.keyMasked（最近请求表）继续脱敏——那是紧凑标识列，两者语义不同。
type KeyStatus struct {
	ID            int64  `json:"id"`
	Value         string `json:"value"`
	Enabled       bool   `json:"enabled"`
	Status        string `json:"status"` // untested / active / cooldown / disabled
	Remaining     *int64 `json:"remaining"`
	Limit         *int64 `json:"limit"`
	CooldownUntil *int64 `json:"cooldownUntil"`
	RequestCount  int64  `json:"requestCount"`
	LastUsedAt    *int64 `json:"lastUsedAt"`
	AddedAt       int64  `json:"addedAt"`
}

// UpstreamKey 是 key 原文的内部载体，仅限存储层与调度层使用。
// Value 必须带 json："-"：无 tag 时 encoding/json 仍会导出字段，tag 是唯一硬防线。
type UpstreamKey struct {
	ID    int64
	Value string `json:"-"`
}

// AddKey 插入新 key，成功返回完整记录。
// 重复 key 返回 ErrDuplicateKey；格式不合法（前缀/长度）返回 ErrInvalidKeyFormat。
func (s *Store) AddKey(value string) (UpstreamKey, error) {
	// 信任边界校验放在入口：越早拒绝越不会以短值形态落库，
	// 从源头杜绝 MaskedKey 遇到不可安全脱敏的短值
	if len(value) < minKeyLength || !strings.HasPrefix(value, keyPrefix) {
		return UpstreamKey{}, ErrInvalidKeyFormat
	}
	res, err := s.db.Exec(`INSERT INTO keys (value, added_at) VALUES (?, ?)`, value, time.Now().Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return UpstreamKey{}, ErrDuplicateKey
		}
		return UpstreamKey{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return UpstreamKey{}, err
	}
	return UpstreamKey{ID: id, Value: value}, nil
}

// GetKeyByID 按 ID 取 key 原文（内部调度用），不存在返回 ErrKeyNotFound。
func (s *Store) GetKeyByID(id int64) (UpstreamKey, error) {
	row := s.db.QueryRow(`SELECT id, value FROM keys WHERE id = ?`, id)
	var k UpstreamKey
	if err := row.Scan(&k.ID, &k.Value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UpstreamKey{}, ErrKeyNotFound
		}
		return UpstreamKey{}, err
	}
	return k, nil
}

// DeleteKey 按 ID 删除，不存在返回 ErrKeyNotFound。
func (s *Store) DeleteKey(id int64) error {
	res, err := s.db.Exec(`DELETE FROM keys WHERE id = ?`, id)
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

// SetKeyEnabled 启停 key，不存在返回 ErrKeyNotFound。
func (s *Store) SetKeyEnabled(id int64, enabled bool) error {
	res, err := s.db.Exec(`UPDATE keys SET enabled = ? WHERE id = ?`, enabled, id)
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

// ListKeys 返回全部 key 的对外脱敏状态，按 ID 升序（稳定展示顺序）。
func (s *Store) ListKeys() ([]KeyStatus, error) {
	rows, err := s.db.Query(`SELECT id, value, enabled, remaining, rate_limit,
		cooldown_until, request_count, last_used_at, added_at FROM keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyStatus
	for rows.Next() {
		ks, err := scanKeyStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ks)
	}
	return out, rows.Err()
}

// scanKeyStatus 从行扫描脱敏状态并派生 status 字段。
// status 与 cooldownUntil 用同一 now 判定：已到期的冷却不输出为 cooldown
// 状态，且 cooldownUntil 输出 nil——两个字段口径一致，避免出现"状态 active
// 但冷却值残留"的矛盾展示。
func scanKeyStatus(row interface{ Scan(...any) error }) (KeyStatus, error) {
	var (
		ks                                       KeyStatus
		value                                    string
		enabled                                  int
		remaining, rateLimit, cooldown, lastUsed sql.NullInt64
	)
	if err := row.Scan(&ks.ID, &value, &enabled, &remaining, &rateLimit,
		&cooldown, &ks.RequestCount, &lastUsed, &ks.AddedAt); err != nil {
		return KeyStatus{}, err
	}
	now := time.Now().Unix()
	ks.Value = value
	ks.Enabled = enabled != 0
	ks.Remaining = nullInt64(remaining)
	ks.Limit = nullInt64(rateLimit)
	ks.CooldownUntil = nullInt64(cooldown)
	ks.LastUsedAt = nullInt64(lastUsed)
	ks.Status = deriveStatus(ks, now)
	if ks.CooldownUntil != nil && *ks.CooldownUntil <= now {
		ks.CooldownUntil = nil // 冷却已到期：与 status=active 的口径一致
	}
	return ks, nil
}

// deriveStatus 按契约推导展示状态；冷却结束时刻已过则回落 active。
func deriveStatus(ks KeyStatus, now int64) string {
	switch {
	case !ks.Enabled:
		return "disabled"
	case ks.CooldownUntil != nil && *ks.CooldownUntil > now:
		return "cooldown"
	case ks.RequestCount == 0:
		return "untested"
	default:
		return "active"
	}
}

func nullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	val := v.Int64
	return &val
}

// isUniqueViolation 判断驱动错误是否为 UNIQUE 约束冲突（重复 key 插入）。
// 依赖驱动导出的 *sqlite.Error 类型断言，不解析错误字符串。
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}
