// MCP 透传层的 key 选择：与 REST 共用同一套池过滤/轮询/冷却实现，
// 单维护点——MCP 只多一个“不产生额度观察”的入口，不多一套调度逻辑。
package query

import (
	"fmt"
	"net/http"
)

// PickProxyKey 为 MCP 透传层选一个可用 key：复用池过滤（跳过禁用/冷却）
// 与 Service 轮询游标，选后计数尝试。已知取舍：MCP 流量不经过 callUpstream，
// 自身不产生额度观察（官方 MCP 网关不返回 RateLimit-* 头），冷却只能由
// 上游 429 显式触发；MCP 请求借此共享 REST 流量积累的冷却状态受益。
func (s *Service) PickProxyKey() (keyID int64, value string, err error) {
	k, err := s.pickKey()
	if err != nil {
		return 0, "", err
	}
	if err := s.pool.MarkKeyAttempt(k.ID); err != nil {
		return 0, "", fmt.Errorf("mark key attempt: %w", err)
	}
	return k.ID, k.Value, nil
}

// CooldownProxyKey 对 MCP 透传收到 429 的 key 落盘冷却：复用三级 fallback
// （RateLimit-Reset > Retry-After > 5 分钟兜底），与 REST 的 429 冷却是
// 同一实现同一优先级，不另立口径。
func (s *Service) CooldownProxyKey(keyID int64, h http.Header) error {
	if _, err := s.cooldownFor429(keyID, h); err != nil {
		return fmt.Errorf("cooldown MCP proxy key: %w", err)
	}
	return nil
}
