import { Badge, Empty, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { fullTime } from '../format';
import type { RequestRecord } from '../types';

interface RequestsTableProps {
  records: RequestRecord[];
  loading: boolean;
}

/** 状态码按语义着色：2xx 正常、429 限流预警、其余 4xx/5xx 异常 */
function statusCodeTag(status: number) {
  const color = status === 429 ? 'orange' : status >= 200 && status < 300 ? 'green' : 'red';
  return <Tag color={color}>{status}</Tag>;
}

const columns: ColumnsType<RequestRecord> = [
  {
    title: '时间',
    dataIndex: 'time',
    width: 100,
    render: (t: number) => <span className="stat-num">{fullTime(t)}</span>,
  },
  {
    title: '请求',
    key: 'request',
    render: (_, r) => (
      <Typography.Text code className="stat-num">
        {r.method} {r.path}
      </Typography.Text>
    ),
  },
  {
    title: '状态码',
    dataIndex: 'status',
    width: 90,
    render: statusCodeTag,
  },
  {
    title: '使用 Key',
    dataIndex: 'keyMasked',
    ellipsis: true,
    render: (m: string) => <span className="stat-num">{m}</span>,
  },
  {
    title: '缓存',
    dataIndex: 'cached',
    width: 80,
    render: (hit: boolean) =>
      hit ? (
        <Badge status="success" text="命中" />
      ) : (
        <Typography.Text type="secondary">—</Typography.Text>
      ),
  },
  {
    title: '耗时',
    dataIndex: 'durationMs',
    width: 90,
    align: 'right',
    render: (ms: number) => <span className="stat-num">{ms} ms</span>,
  },
];

/** 最近请求列表，由父组件控制数据与加载态 */
export function RequestsTable({ records, loading }: RequestsTableProps) {
  return (
    <Table<RequestRecord>
      rowKey={(r) => `${r.time}-${r.keyId}-${r.path}`}
      columns={columns}
      dataSource={records}
      loading={loading}
      size="small"
      pagination={false}
      locale={{ emptyText: <Empty description="暂无请求记录" /> }}
    />
  );
}
