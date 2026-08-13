import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  server: {
    host: '127.0.0.1',
    proxy: {
      // target 用 localhost（::1）而非 127.0.0.1：本机 127.0.0.1:18080 可能被
      // 其他进程（如 mock-mfp-api.mjs 联调服务）占用，特定地址绑定优先于 0.0.0.0，
      // 代理会误入 mock 返回 404。localhost 解析到 ::1 命中真后端。
      '/admin': { target: 'http://localhost:18080', changeOrigin: true },
      '/user': {
        target: 'http://localhost:18080',
        changeOrigin: true,
        // 页面导航（Accept: text/html，如 /user/login /user/keys 等 SPA 路由）交回
        // vite 返回 index.html；API fetch（Accept: */*）代理到后端。
        // 注意 vite 8 语义：bypass 返回 string 才绕过代理（url 重写），true/undefined 均继续代理。
        bypass: (req) =>
          req.headers.accept?.includes('text/html') ? req.url : undefined,
      },
    },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
