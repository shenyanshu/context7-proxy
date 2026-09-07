import { Card, Col, Row, Statistic } from 'antd';
import { useEffect, useState } from 'react';
import type { Overview } from '../types';

interface StatsRowProps {
  overview: Overview;
}

/** 顶部四格统计：总请求、今日请求、缓存命中率、Key 健康度 */
export function StatsRow({ overview }: StatsRowProps) {
  const { totalRequests, requestsToday, cacheHitRate, keys } = overview;
  const activeCount = keys.filter((k) => k.status === 'active').length;
  const coolingCount = keys.filter((k) => k.status === 'cooldown').length;

  return (
    <Row gutter={[16, 16]}>
      <Col xs={12} md={6}>
        <Card>
          <Statistic title="总请求量" value={totalRequests} className="stat-num" />
        </Card>
      </Col>
      <Col xs={12} md={6}>
        <Card>
          <Statistic title="今日请求" value={requestsToday} className="stat-num" />
        </Card>
      </Col>
      <Col xs={12} md={6}>
        <Card>
          <Statistic
            title="缓存命中率"
            value={Math.round(cacheHitRate * 100)}
            suffix="%"
            className="stat-num"
          />
        </Card>
      </Col>
      <Col xs={12} md={6}>
        <Card>
          <KeyHealthCard activeCount={activeCount} coolingCount={coolingCount} />
        </Card>
      </Col>
    </Row>
  );
}

interface KeyHealthCardProps {
  activeCount: number;
  coolingCount: number;
}

/** 可用/冷却中 key 数；冷却中的数量带实时感（数字每秒随倒计时更新） */
function KeyHealthCard({ activeCount, coolingCount }: KeyHealthCardProps) {
  // 冷却计数本质依赖"现在"这个时刻，触发重渲染让展示更贴近真实状态
  const [, setTick] = useState(0);
  useEffect(() => {
    const timer = window.setInterval(() => setTick((t) => t + 1), 1000);
    return () => window.clearInterval(timer);
  }, []);

  return (
    <div>
      <div className="stat-title">Key 状态</div>
      <div className="key-health">
        <span className="stat-num key-health-main">{activeCount} 可用</span>
        <span className="stat-num key-health-sub">{coolingCount} 冷却中</span>
      </div>
    </div>
  );
}
