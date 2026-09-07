import { ConfigProvider } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import React from 'react';
import ReactDOM from 'react-dom/client';
import { HashRouter } from 'react-router-dom';
import App from './App';
import './style.css';

// 产物会被 Go 内嵌并挂在 /admin 一类的子路径，HashRouter 让前端路由完全活在 hash 里，
// 后端无需再做 history fallback 配置；配合 vite base './'，资源引用同样与路径无关
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ConfigProvider
      locale={zhCN}
      theme={{
        token: {
          // 信息密度优先：默认尺寸略收紧，等宽数字由 .stat-num 保证
          fontSize: 14,
          borderRadius: 6,
        },
      }}
    >
      <HashRouter>
        <App />
      </HashRouter>
    </ConfigProvider>
  </React.StrictMode>,
);
