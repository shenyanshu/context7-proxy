import {
  DashboardOutlined,
  DatabaseOutlined,
  KeyOutlined,
  LogoutOutlined,
} from '@ant-design/icons';
import { Layout, Menu, Tabs, Typography } from 'antd';
import { Navigate, Outlet, Route, Routes, useNavigate, useSearchParams } from 'react-router-dom';
import { KEY_STORAGE } from './api';
import { Cache } from './views/Cache';
import { Dashboard } from './views/Dashboard';
import { Keys } from './views/Keys';
import { Login } from './views/Login';

/** 未携带 master key 时不渲染首页，直接送去登录 */
function RequireAuth() {
  if (!localStorage.getItem(KEY_STORAGE)) {
    return <Navigate to="/login" replace />;
  }
  return <Outlet />;
}

/** 登出按钮逻辑独立，避免 Layout 壳掺入业务动作 */
function useLogout() {
  const navigate = useNavigate();
  return () => {
    localStorage.removeItem(KEY_STORAGE);
    navigate('/login', { replace: true });
  };
}

const TAB_DASHBOARD = 'dashboard';
const TAB_KEYS = 'keys';
const TAB_CACHE = 'cache';

// 非法 tab 值一律回落到仪表盘，避免旧链接或手改 URL 造成空白页
const VALID_TABS = new Set([TAB_KEYS, TAB_CACHE]);

/** 主框架：侧边导航 + 顶部标题栏，右侧内容按 tab 切换 */
function AdminLayout() {
  const [params, setParams] = useSearchParams();
  const raw = params.get('tab') ?? '';
  const tab = VALID_TABS.has(raw) ? raw : TAB_DASHBOARD;
  const logout = useLogout();

  const switchTab = (key: string) => {
    // tab 写入 querystring，刷新后仍停留在原视图
    setParams(key === TAB_DASHBOARD ? {} : { tab: key }, { replace: true });
  };

  return (
    <Layout className="admin-shell">
      <Layout.Sider breakpoint="lg" theme="light" width={200}>
        <div className="admin-brand">
          <Typography.Text strong>C7 Proxy</Typography.Text>
        </div>
        <Menu
          mode="inline"
          selectedKeys={[tab]}
          onClick={(e) => switchTab(e.key)}
          items={[
            { key: TAB_DASHBOARD, icon: <DashboardOutlined />, label: '仪表盘' },
            { key: TAB_KEYS, icon: <KeyOutlined />, label: 'Key 管理' },
            { key: TAB_CACHE, icon: <DatabaseOutlined />, label: '缓存' },
          ]}
        />
      </Layout.Sider>

      <Layout>
        <Layout.Header className="admin-header">
          <Typography.Title level={4} className="admin-header-title">
            Context7 Proxy 管理控制台
          </Typography.Title>
          <Tabs
            activeKey={tab}
            onChange={switchTab}
            items={[
              { key: TAB_DASHBOARD, label: '仪表盘' },
              { key: TAB_KEYS, label: 'Key 管理' },
              { key: TAB_CACHE, label: '缓存' },
            ]}
          />
          <span
            className="admin-logout"
            onClick={logout}
            role="button"
            tabIndex={0}
            onKeyDown={(e) => e.key === 'Enter' && logout()}
          >
            <LogoutOutlined /> 退出
          </span>
        </Layout.Header>

        <Layout.Content className="admin-content">
          {tab === TAB_KEYS ? <Keys /> : tab === TAB_CACHE ? <Cache /> : <Dashboard />}
        </Layout.Content>
      </Layout>
    </Layout>
  );
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route element={<RequireAuth />}>
        <Route path="/" element={<AdminLayout />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
