import { LockOutlined } from '@ant-design/icons';
import { Alert, Button, Card, Form, Input, Typography } from 'antd';
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api, KEY_STORAGE } from '../api';
import type { Overview } from '../types';

/** 登录页：粘贴 master key，用 overview 接口验证可用后落盘 */
export function Login() {
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const navigate = useNavigate();

  const submit = async (values: { key: string }) => {
    const key = values.key.trim();
    setLoading(true);
    setError(null);
    // 先存再请求：请求成功即落盘符合预期，失败则 api 层 401 时自动清除
    localStorage.setItem(KEY_STORAGE, key);
    try {
      await api.get<Overview>('/api/admin/overview');
      navigate('/', { replace: true });
    } catch (e) {
      localStorage.removeItem(KEY_STORAGE);
      setError(e instanceof Error && e.message ? e.message : '验证失败，请确认 Master Key 是否正确');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="login-page">
      <Card className="login-card">
        <Typography.Title level={3} className="login-title">
          Context7 Proxy 管理控制台
        </Typography.Title>
        <Typography.Paragraph type="secondary">
          请输入 Master Key 以访问管理接口
        </Typography.Paragraph>

        {error && <Alert type="error" showIcon message={error} className="login-error" />}

        <Form layout="vertical" onFinish={submit} requiredMark={false}>
          <Form.Item
            name="key"
            rules={[{ required: true, whitespace: true, message: '请输入 Master Key' }]}
          >
            <Input.Password
              prefix={<LockOutlined />}
              placeholder="Master Key"
              size="large"
              autoComplete="off"
            />
          </Form.Item>
          <Button type="primary" htmlType="submit" size="large" block loading={loading}>
            登录
          </Button>
        </Form>
      </Card>
    </div>
  );
}
