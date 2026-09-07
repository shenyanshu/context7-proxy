import { DeleteOutlined, PlusOutlined } from '@ant-design/icons';
import {
  Alert,
  Button,
  Form,
  Input,
  Modal,
  Popconfirm,
  Result,
  Switch,
  Table,
  Tag,
  Typography,
  message,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useState } from 'react';
import { api } from '../api';
import { fromNow } from '../format';
import { keyStatusColor, keyStatusText } from '../keyStatusUi';
import type { AddedKey, KeyList, KeyStatus, OkResponse } from '../types';

function statusTag(status: KeyStatus['status']) {
  return <Tag color={keyStatusColor(status)}>{keyStatusText(status)}</Tag>;
}

/** Key 管理：列表 + 增删启停 */
export function Keys() {
  const [keys, setKeys] = useState<KeyStatus[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [modalOpen, setModalOpen] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [form] = Form.useForm<{ value: string }>();
  const [messageApi, contextHolder] = message.useMessage();

  const load = useCallback(async () => {
    try {
      const data = await api.get<KeyList>('/api/admin/keys');
      setKeys(data?.keys ?? []);
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

  const addKey = async (values: { value: string }) => {
    setSubmitting(true);
    try {
      const added = await api.post<AddedKey>('/api/admin/keys', { value: values.value.trim() });
      messageApi.success(`已添加 ${added?.value ?? '新 Key'}`);
      setModalOpen(false);
      form.resetFields();
      await load();
    } catch (e) {
      messageApi.error(e instanceof Error ? e.message : '添加失败');
    } finally {
      setSubmitting(false);
    }
  };

  const toggleEnabled = async (k: KeyStatus, enabled: boolean) => {
    try {
      await api.patch<OkResponse>(`/api/admin/keys/${k.id}`, { enabled });
      await load();
    } catch (e) {
      messageApi.error(e instanceof Error ? e.message : '操作失败');
    }
  };

  const removeKey = async (k: KeyStatus) => {
    try {
      await api.delete<OkResponse>(`/api/admin/keys/${k.id}`);
      messageApi.success(`已删除 ${k.value}`);
      await load();
    } catch (e) {
      messageApi.error(e instanceof Error ? e.message : '删除失败');
    }
  };

  const columns: ColumnsType<KeyStatus> = [
    {
      title: 'Key',
      dataIndex: 'value',
      render: (v: string) => (
        <Typography.Text code className="stat-num">
          {v}
        </Typography.Text>
      ),
    },
    { title: '状态', dataIndex: 'status', width: 110, render: statusTag },
    {
      title: '累计请求',
      dataIndex: 'requestCount',
      width: 110,
      align: 'right',
      render: (n: number) => <span className="stat-num">{n}</span>,
    },
    {
      title: '添加时间',
      dataIndex: 'addedAt',
      width: 120,
      render: (t: number) => fromNow(t),
    },
    {
      title: '启用',
      dataIndex: 'enabled',
      width: 80,
      render: (enabled: boolean, k) => (
        <Switch checked={enabled} size="small" onChange={(v) => void toggleEnabled(k, v)} />
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 90,
      render: (_, k) => (
        <Popconfirm
          title={`删除 ${k.value}？`}
          description="删除后该 Key 不再参与轮换"
          okText="删除"
          okButtonProps={{ danger: true }}
          cancelText="取消"
          onConfirm={() => void removeKey(k)}
        >
          <Button type="text" danger icon={<DeleteOutlined />} />
        </Popconfirm>
      ),
    },
  ];

  if (loadError && keys.length === 0) {
    return <Result status="error" title="加载失败" subTitle={loadError} />;
  }

  return (
    <div className="page-stack">
      {contextHolder}
      <div className="keys-toolbar">
        <Alert
          type="info"
          showIcon
          message="启停立即生效；处于冷却中的 Key 即使启用也不会被调度。"
        />
        <Button type="primary" icon={<PlusOutlined />} onClick={() => setModalOpen(true)}>
          添加 Key
        </Button>
      </div>

      <Table<KeyStatus>
        rowKey="id"
        columns={columns}
        dataSource={keys}
        loading={loading}
        size="middle"
        pagination={false}
      />

      <Modal
        title="添加上游 Key"
        open={modalOpen}
        onCancel={() => setModalOpen(false)}
        onOk={() => form.submit()}
        confirmLoading={submitting}
        okText="添加"
        cancelText="取消"
        destroyOnClose
      >
        <Form form={form} layout="vertical" onFinish={addKey} preserve={false}>
          <Form.Item
            name="value"
            label="Context7 API Key"
            rules={[{ required: true, whitespace: true, message: '请粘贴 Key' }]}
            extra="以 ctx7sk- 开头，添加后默认启用；有效性在实际使用后自动更新。"
          >
            <Input.TextArea
              rows={3}
              placeholder="ctx7sk-..."
              // password 形态：浏览器不记密码、不自动补全
              autoComplete="off"
              className="stat-num mono-secret"
            />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}
