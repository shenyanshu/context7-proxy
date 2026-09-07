import { Badge, Card, Typography } from 'antd';
import { fromNow } from '../format';
import { keyStatusColor, keyStatusText } from '../keyStatusUi';
import type { KeyStatus } from '../types';
import { CooldownClock } from './CooldownClock';

interface KeyCardProps {
  keyStatus: KeyStatus;
}

/** 单个上游 Key 的状态卡：状态、使用量、最近使用与冷却倒计时 */
export function KeyCard({ keyStatus }: KeyCardProps) {
  const { status, value, cooldownUntil, requestCount, lastUsedAt } = keyStatus;

  return (
    <Card size="small" className="key-card">
      <div className="key-card-head">
        <Typography.Text code className="stat-num">
          {value}
        </Typography.Text>
        <Badge color={keyStatusColor(status)} text={keyStatusText(status)} />
      </div>

      <div className="key-card-row">
        <span className="key-card-label">累计请求</span>
        <span className="stat-num">{requestCount}</span>
      </div>

      <div className="key-card-row">
        <span className="key-card-label">最近使用</span>
        <span>{fromNow(lastUsedAt)}</span>
      </div>

      {status === 'cooldown' && cooldownUntil !== null && (
        <div className="key-card-row">
          <span className="key-card-label">冷却剩余</span>
          <CooldownClock cooldownUntil={cooldownUntil} />
        </div>
      )}
    </Card>
  );
}
