// 管理 API 契约类型，字段命名与后端严格对齐，时间戳均为 Unix 秒

export type KeyStatusValue = 'untested' | 'active' | 'cooldown' | 'disabled';

export interface KeyStatus {
  id: number;
  /** 完整 key 原文（自用部署场景，管理界面明文显示） */
  value: string;
  enabled: boolean;
  status: KeyStatusValue;
  /** 剩余额度，未知为 null */
  remaining: number | null;
  /** 额度上限，未知为 null */
  limit: number | null;
  /** 冷却结束时间（Unix 秒），不在冷却中为 null */
  cooldownUntil: number | null;
  requestCount: number;
  lastUsedAt: number | null;
  addedAt: number;
}

export interface RequestRecord {
  time: number;
  method: string;
  path: string;
  status: number;
  keyId: number;
  keyMasked: string;
  cached: boolean;
  durationMs: number;
}

export interface Overview {
  totalRequests: number;
  cacheHits: number;
  /** 0~1 之间的小数命中率 */
  cacheHitRate: number;
  requestsToday: number;
  keys: KeyStatus[];
  recentRequests: RequestRecord[];
}

export interface KeyList {
  keys: KeyStatus[];
}

export interface AddedKey {
  id: number;
  /** 完整 key 原文（自用部署场景，明文回显） */
  value: string;
}

export interface OkResponse {
  ok: boolean;
}

export interface ErrorResponse {
  error: string;
}
