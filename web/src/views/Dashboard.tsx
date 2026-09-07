import { Alert, Col, Empty, Result, Row } from 'antd';
import { useCallback, useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { KeyCard } from '../components/KeyCard';
import { RequestsTable } from '../components/RequestsTable';
import { StatsRow } from '../components/StatsRow';
import type { Overview } from '../types';

const POLL_INTERVAL_MS = 5000;

/** 仪表盘：概览统计 + Key 卡片 + 最近请求，每 5 秒轮询 */
export function Dashboard() {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // 首次加载失败时给出整页错误；轮询失败仅在顶部提示，不打断已有数据浏览
  const [pollError, setPollError] = useState<string | null>(null);
  // load 需保持稳定引用（供轮询 effect 使用），"是否已有数据"用 ref 追踪避免闭包过期
  const hasData = useRef(false);

  const load = useCallback(async () => {
    try {
      const data = await api.get<Overview>('/api/admin/overview');
      hasData.current = true;
      setOverview(data);
      setError(null);
      setPollError(null);
    } catch (e) {
      // 401 时 api 层已跳登录页，无需再渲染错误
      if (e instanceof ApiError && e.status === 401) {
        return;
      }
      const message = e instanceof Error ? e.message : '加载失败';
      if (hasData.current) {
        setPollError(message);
      } else {
        setError(message);
      }
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [load]);

  if (error) {
    return <Result status="error" title="加载失败" subTitle={error} />;
  }

  if (!overview && loading) {
    return null;
  }

  // 轮询竞态防御：理论上 loading 结束必有 overview 或 error，这里兜底
  if (!overview) {
    return null;
  }

  const emptyKeys = overview.keys.length === 0;

  return (
    <div className="page-stack">
      {pollError && <Alert type="warning" showIcon closable message={`刷新失败：${pollError}`} />}

      <StatsRow overview={overview} />

      <section>
        <h3 className="section-title">Key 状态</h3>
        {emptyKeys ? (
          <Empty description="尚未添加任何上游 Key，请到「Key 管理」页添加" />
        ) : (
          <Row gutter={[16, 16]}>
            {overview.keys.map((k) => (
              <Col xs={24} sm={12} lg={8} xxl={6} key={k.id}>
                <KeyCard keyStatus={k} />
              </Col>
            ))}
          </Row>
        )}
      </section>

      <section>
        <h3 className="section-title">最近请求</h3>
        <RequestsTable records={overview.recentRequests} loading={loading} />
      </section>
    </div>
  );
}
