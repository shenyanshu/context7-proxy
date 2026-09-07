import { Badge, Card, Progress, Tooltip, Typography } from 'antd';
import { fromNow } from '../format';
import { keyStatusColor, keyStatusText } from '../keyStatusUi';
import type { KeyStatus } from '../types';
import { CooldownClock } from './CooldownClock';

interface KeyCardProps {
  keyStatus: KeyStatus;
}

/** 单个上游 Key 的状态卡：状态、额度、使用量、最近使用 */
export function KeyCard({ keyStatus }: KeyCardProps) {
  const { status, value, remaining, limit, cooldownUntil, requestCount, lastUsedAt } = keyStatus;
  const quotaKnown = remaining !== null && limit !== null && limit > 0;

  return (
    <Card size="small" className="key-card">
      <div className="key-card-head">
        <Typography.Text code className="stat-num">
          {value}
        </Typography.Text>
        <Badge color={keyStatusColor(status)} text={keyStatusText(status)} />
      </div>

      <div className="key-card-row">
        <span className="key-card-label">剩余额度</span>
        {quotaKnown ? (
          <Tooltip title={`${remaining} / ${limit}`}>
            <Progress
              percent={Math.round((remaining! / limit!) * 100)}
              size="small"
              strokeColor={remaining === 0 ? '#ff4d4f' : undefined}
            />
          </Tooltip>
        ) : (
          <Typography.Text type="secondary">未知</Typography.Text>
        )}
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
