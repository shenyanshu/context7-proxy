import { ReloadOutlined } from '@ant-design/icons';
import {
  Button,
  Drawer,
  Empty,
  Input,
  Result,
  Space,
  Spin,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { api } from '../api';
import { formatBytes, fromNow, fullDateTime } from '../format';
import type { CacheDetail, CacheEntry, CacheList } from '../types';

/** 按 key 前缀区分来源，两种之外的形态兜底为"其他"而非猜测 */
function sourceTag(key: string) {
  if (key.startsWith('mcp:')) {
    return <Tag color="geekblue">MCP</Tag>;
  }
  if (key.startsWith('/api/v2')) {
    return <Tag color="green">REST</Tag>;
  }
  return <Tag>其他</Tag>;
}

/** 判断为 JSON 时尝试美化输出；解析失败（截断的内容等）回退原文 */
function renderBody(detail: CacheDetail): string {
  const trimmed = detail.body.trimStart();
  const looksJson =
    detail.contentType.includes('json') || trimmed.startsWith('{') || trimmed.startsWith('[');
  if (!looksJson) {
    return detail.body;
  }
  try {
    return JSON.stringify(JSON.parse(detail.body), null, 2);
  } catch {
    return detail.body;
  }
}

function buildColumns(onView: (entry: CacheEntry) => void): ColumnsType<CacheEntry> {
  return [
    { title: '来源', key: 'source', width: 90, render: (_, e) => sourceTag(e.key) },
    {
      title: '缓存键',
      dataIndex: 'key',
      // 整列阻止冒泡：行点击打开详情，但点复制图标不该触发行点击
      render: (key: string) => (
        <span onClick={(e) => e.stopPropagation()}>
          <Typography.Text
            copyable={{ text: key }}
            ellipsis={{ tooltip: key }}
            className="stat-num"
            style={{ maxWidth: 420 }}
          >
            {key}
          </Typography.Text>
        </span>
      ),
    },
    {
      title: '大小',
      dataIndex: 'size',
      width: 100,
      align: 'right',
      sorter: (a, b) => a.size - b.size,
      render: (size: number) => <span className="stat-num">{formatBytes(size)}</span>,
    },
    { title: '类型', dataIndex: 'contentType', ellipsis: true, width: 160 },
    {
      title: '过期时间',
      dataIndex: 'expiresAt',
      width: 140,
      sorter: (a, b) => a.expiresAt - b.expiresAt,
      render: (t: number) => (
        <Tooltip title={fullDateTime(t)}>
          <span>{fromNow(t)}</span>
        </Tooltip>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 80,
      render: (_, e) => (
        <Button
          type="link"
          size="small"
          onClick={(ev) => {
            ev.stopPropagation();
            onView(e);
          }}
        >
          查看
        </Button>
      ),
    },
  ];
}

interface DetailDrawerProps {
  cacheKey: string | null;
  onClose: () => void;
  /** 详情拉取失败（通常是条目恰已过期）：通知父组件报错并刷新列表 */
  onLoadError: (message: string) => void;
}

/** 缓存正文抽屉，打开时才请求详情，避免列表页加载全部 body */
function DetailDrawer({ cacheKey, onClose, onLoadError }: DetailDrawerProps) {
  const [detail, setDetail] = useState<CacheDetail | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (cacheKey === null) {
      return;
    }
    setDetail(null);
    setLoading(true);
    api
      .get<CacheDetail>(`/api/admin/cache/${encodeURIComponent(cacheKey)}`)
      .then(setDetail)
      .catch((e: unknown) => {
        onLoadError(e instanceof Error ? e.message : '加载失败');
      })
      .finally(() => setLoading(false));
  }, [cacheKey, onLoadError]);

  return (
    <Drawer title="缓存内容" width={720} open={cacheKey !== null} onClose={onClose}>
      {loading && (
        <div style={{ textAlign: 'center', padding: 48 }}>
          <Spin />
        </div>
      )}
      {detail && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <div>
            <div className="key-card-label" style={{ marginBottom: 4 }}>
              缓存键
            </div>
            <Typography.Paragraph
              copyable
              className="stat-num"
              style={{ wordBreak: 'break-all', marginBottom: 0 }}
            >
              {detail.key}
            </Typography.Paragraph>
          </div>
          <div>
            <span className="key-card-label" style={{ marginRight: 8 }}>
              类型
            </span>
            <Tag>{detail.contentType}</Tag>
          </div>
          <pre
            style={{
              margin: 0,
              padding: 12,
              background: '#fafafa',
              border: '1px solid #e5e7eb',
              borderRadius: 6,
              maxHeight: '60vh',
              overflow: 'auto',
              fontSize: 12,
              fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
              whiteSpace: 'pre-wrap',
              wordBreak: 'break-all',
            }}
          >
            {renderBody(detail)}
          </pre>
        </div>
      )}
    </Drawer>
  );
}

/** 缓存页：只读查看缓存条目与正文，进入页面或点刷新时拉取，搜索/排序均在客户端完成 */
export function Cache() {
  const [entries, setEntries] = useState<CacheEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [search, setSearch] = useState('');
  const [detailKey, setDetailKey] = useState<string | null>(null);
  const [messageApi, contextHolder] = message.useMessage();

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.get<CacheList>('/api/admin/cache');
      setEntries(data?.entries ?? []);
      setLoadError(null);
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : '加载失败');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return q ? entries.filter((e) => e.key.toLowerCase().includes(q)) : entries;
  }, [entries, search]);

  // 详情 404 说明条目刚过期，顺手刷新列表让界面回到真实状态
  const handleDetailError = useCallback(
    (msg: string) => {
      messageApi.error(msg);
      setDetailKey(null);
      void load();
    },
    [messageApi, load],
  );

  const columns = useMemo(() => buildColumns((e) => setDetailKey(e.key)), []);

  if (loadError && entries.length === 0) {
    return <Result status="error" title="加载失败" subTitle={loadError} />;
  }

  return (
    <div className="page-stack">
      {contextHolder}
      <div className="keys-toolbar">
        <Typography.Text type="secondary">
          共 {entries.length} 条{search.trim() ? `，筛选出 ${filtered.length} 条` : ''}
        </Typography.Text>
        <Space>
          <Input.Search
            allowClear
            placeholder="按缓存键搜索"
            style={{ width: 280 }}
            onChange={(e) => setSearch(e.target.value)}
          />
          <Button icon={<ReloadOutlined />} loading={loading} onClick={() => void load()}>
            刷新
          </Button>
        </Space>
      </div>

      <Table<CacheEntry>
        rowKey="key"
        columns={columns}
        dataSource={filtered}
        loading={loading}
        size="middle"
        pagination={false}
        onRow={(record) => ({
          onClick: () => setDetailKey(record.key),
          style: { cursor: 'pointer' },
        })}
        locale={{
          emptyText: (
            <Empty
              description={
                entries.length === 0
                  ? '暂无缓存条目，发起一次查询后这里会展示缓存详情'
                  : '没有匹配的缓存条目'
              }
            />
          ),
        }}
      />

      <DetailDrawer
        cacheKey={detailKey}
        onClose={() => setDetailKey(null)}
        onLoadError={handleDetailError}
      />
    </div>
  );
}
