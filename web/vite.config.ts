import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [react()],
  // 产物会被 Go 内嵌并挂载到 /admin 之类的子路径，资源引用必须用相对路径才能避免 404
  base: './',
  server: {
    proxy: {
      '/api/admin': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
});
