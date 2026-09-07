// key 调度：可用性过滤与轮询选择。
// 设计基线是轮询（可靠、防饿死）：上游不保证每次响应都带额度头，
// 任何基于额度的"智能排序"都不可依赖；额度观察只持久化到 store 供管理界面展示，
// 不参与调度决策。
package query

import (
	"context7-proxy/internal/store"
)

// availableKey 是过滤后的候选（冷却字段已剔除）。
type availableKey struct {
	id    int64
	value string
}

// filterAvailable 过滤启用且冷却已到期的 key。
// 到期即自动恢复：不会因旧 remaining=0 观察值让 key 永久不可用。
func filterAvailable(keys []store.PoolKey, nowUnix int64) []availableKey {
	var out []availableKey
	for _, k := range keys {
		if k.CooldownUntil != nil && *k.CooldownUntil > nowUnix {
			continue // 冷却未到期
		}
		out = append(out, availableKey{id: k.ID, value: k.Value})
	}
	return out
}

// pickRoundRobin 按 Service 轮询计数选一个 key；轮转位置随 Service 生命周期
// 保持，跨请求打散上游负载。原子递增即可：轮询不需要严格公平，偶尔跳位无碍。
func (s *Service) pickRoundRobin(candidates []availableKey) store.PoolKey {
	n := s.robin.Add(1)
	k := candidates[int(n-1)%len(candidates)]
	return store.PoolKey{ID: k.id, Value: k.value}
}
