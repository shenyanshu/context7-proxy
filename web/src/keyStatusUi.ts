// Key 状态展示是唯一维护点：颜色、文案只在这里定，避免卡片/表格两处各自硬编码
import type { KeyStatusValue } from './types';

export const KEY_STATUS_UI: Record<KeyStatusValue, { color: string; text: string }> = {
  active: { color: 'green', text: '可用' },
  cooldown: { color: 'orange', text: '冷却中' },
  untested: { color: 'blue', text: '未验证' },
  disabled: { color: 'default', text: '已禁用' },
};

/** 冷却中的 key 即便 enabled 也不应展示为运行中，以 status 字段为准 */
export function keyStatusColor(status: KeyStatusValue): string {
  return KEY_STATUS_UI[status].color;
}

export function keyStatusText(status: KeyStatusValue): string {
  return KEY_STATUS_UI[status].text;
}
