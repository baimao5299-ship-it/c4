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
      '/admin': { target: 'http://127.0.0.1:18080', changeOrigin: true },
      '/user': {
        target: 'http://127.0.0.1:18080',
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
